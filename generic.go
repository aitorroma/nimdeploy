package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The "generic" provider accepts webhooks from anything that can POST JSON
// (Ansible, AWX, CI jobs, scripts, other services). Instead of a git push it
// is authenticated with a per-deploy HMAC signature or token, and its data
// reaches the command only through declared, validated params.

const (
	providerGeneric = "generic"

	authHMAC  = "hmac"
	authToken = "token"

	defaultSignatureHeader = "X-Signature"
	defaultTokenHeader     = "Authorization"
	defaultDeliveryHeader  = "X-Delivery-ID"
	defaultMaxSkew         = 5 * time.Minute
	defaultParamMaxLen     = 256
)

// defaultParamMatch is what a param must look like when it has neither enum
// nor match: identifiers, versions, hostnames, paths, refs. Anything wider
// has to be asked for explicitly.
var defaultParamMatch = regexp.MustCompile(`^[A-Za-z0-9._:/@+=,-]*$`)

var paramNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Variables a param may never set: they change how the command, its shell or
// its interpreter behave, or would hide nimdeploy's own DEPLOY_* values.
var reservedParamNames = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "PWD": true,
	"IFS": true, "ENV": true, "BASH_ENV": true, "CDPATH": true, "PS4": true, "SHELLOPTS": true, "BASHOPTS": true,
	"TMPDIR": true, "NODE_OPTIONS": true, "NODE_PATH": true, "PYTHONPATH": true, "PYTHONSTARTUP": true,
	"PERL5LIB": true, "PERL5OPT": true, "RUBYOPT": true, "RUBYLIB": true, "JAVA_TOOL_OPTIONS": true,
	"GIT_SSH": true, "GIT_SSH_COMMAND": true, "GIT_EXEC_PATH": true, "GIT_CONFIG_GLOBAL": true,
	"ANSIBLE_CONFIG": true, "PHPRC": true, "PHP_INI_SCAN_DIR": true,
}

func reservedParamName(name string) bool {
	return reservedParamNames[name] || strings.HasPrefix(name, "DEPLOY_") ||
		strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "BASH_FUNC_")
}

// ParamConfig declares one value taken from the webhook's JSON body and
// passed to the command as an environment variable.
type ParamConfig struct {
	// From is the JSON path: "service", "release.tag", "hosts[0].name".
	From     string   `toml:"from"`
	Enum     []string `toml:"enum"`
	Match    string   `toml:"match"`
	Required bool     `toml:"required"`
	Default  string   `toml:"default"`
	MaxLen   int      `toml:"max_len"`

	path []pathStep
	re   *regexp.Regexp
}

// Param is a captured value, in the order the names sort.
type Param struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (p *ParamConfig) validate(name string) error {
	if !paramNameRe.MatchString(name) {
		return fmt.Errorf("param %s: name must be UPPER_CASE (letters, digits, _)", name)
	}
	if reservedParamName(name) {
		return fmt.Errorf("param %s: name is reserved (PATH, LD_*, DEPLOY_*, interpreter options...)", name)
	}
	if p.From == "" {
		return fmt.Errorf("param %s: from (JSON path) is required", name)
	}
	var err error
	if p.path, err = parseJSONPath(p.From); err != nil {
		return fmt.Errorf("param %s: from: %w", name, err)
	}
	if len(p.Enum) > 0 && p.Match != "" {
		return fmt.Errorf("param %s: use enum or match, not both", name)
	}
	if p.Match != "" {
		if p.re, err = regexp.Compile(p.Match); err != nil {
			return fmt.Errorf("param %s: match: %w", name, err)
		}
	}
	if p.MaxLen == 0 {
		p.MaxLen = defaultParamMaxLen
	}
	if p.MaxLen < 0 {
		return fmt.Errorf("param %s: max_len must be positive", name)
	}
	if p.Required && p.Default != "" {
		return fmt.Errorf("param %s: a required param cannot have a default", name)
	}
	if p.Default != "" {
		if err := p.check(p.Default); err != nil {
			return fmt.Errorf("param %s: default: %w", name, err)
		}
	}
	return nil
}

// check validates a value from the payload (or a manual run).
func (p *ParamConfig) check(v string) error {
	if len(v) > p.MaxLen {
		return fmt.Errorf("longer than %d bytes", p.MaxLen)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return errors.New("contains control characters")
		}
	}
	switch {
	case len(p.Enum) > 0:
		for _, allowed := range p.Enum {
			if v == allowed {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", strings.Join(p.Enum, ", "))
	case p.re != nil:
		if !p.re.MatchString(v) {
			return fmt.Errorf("does not match %s", p.Match)
		}
	default:
		if !defaultParamMatch.MatchString(v) {
			return errors.New("has characters outside [A-Za-z0-9._:/@+=,-] (set match to allow more)")
		}
	}
	return nil
}

// extractParams reads the declared params from the JSON body. An error means
// the request must be rejected: nothing runs.
func extractParams(specs map[string]*ParamConfig, doc any) ([]Param, error) {
	params := make([]Param, 0, len(specs))
	for _, name := range sortedKeys(specs) {
		p := specs[name]
		raw, found := lookupJSON(doc, p.path)
		v := ""
		if found {
			s, ok := scalarString(raw)
			if !ok {
				return nil, fmt.Errorf("param %s: %s is not a string, number or boolean", name, p.From)
			}
			v = s
		}
		if !found || v == "" {
			if p.Required {
				return nil, fmt.Errorf("param %s: %s is missing", name, p.From)
			}
			if p.Default == "" {
				continue
			}
			v = p.Default
		}
		if err := p.check(v); err != nil {
			return nil, fmt.Errorf("param %s: %w", name, err)
		}
		params = append(params, Param{Name: name, Value: v})
	}
	return params, nil
}

// checkManualParams validates params given to a manual run ("nimdeploy run -p").
func checkManualParams(specs map[string]*ParamConfig, given map[string]string) ([]Param, error) {
	for name := range given {
		if _, ok := specs[name]; !ok {
			return nil, fmt.Errorf("unknown param %s", name)
		}
	}
	params := make([]Param, 0, len(specs))
	for _, name := range sortedKeys(specs) {
		p := specs[name]
		v := given[name]
		if v == "" {
			if p.Required {
				return nil, fmt.Errorf("param %s is required", name)
			}
			if p.Default == "" {
				continue
			}
			v = p.Default
		}
		if err := p.check(v); err != nil {
			return nil, fmt.Errorf("param %s: %w", name, err)
		}
		params = append(params, Param{Name: name, Value: v})
	}
	return params, nil
}

func paramValue(params []Param, name string) string {
	for _, p := range params {
		if p.Name == name {
			return p.Value
		}
	}
	return ""
}

func formatParams(params []Param) string {
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = p.Name + "=" + p.Value
	}
	return strings.Join(parts, " ")
}

// --- when ------------------------------------------------------------------------

// whenCond is one "path = value" condition; a list of values means any of them.
type whenCond struct {
	from   string
	path   []pathStep
	values []string
}

func parseWhen(when map[string]any) ([]whenCond, error) {
	conds := make([]whenCond, 0, len(when))
	for _, from := range sortedKeys(when) {
		path, err := parseJSONPath(from)
		if err != nil {
			return nil, fmt.Errorf("when %q: %w", from, err)
		}
		c := whenCond{from: from, path: path}
		switch v := when[from].(type) {
		case []any:
			if len(v) == 0 {
				return nil, fmt.Errorf("when %q: empty list", from)
			}
			for _, item := range v {
				s, ok := scalarString(item)
				if !ok {
					return nil, fmt.Errorf("when %q: values must be strings, numbers or booleans", from)
				}
				c.values = append(c.values, s)
			}
		default:
			s, ok := scalarString(v)
			if !ok {
				return nil, fmt.Errorf("when %q: value must be a string, number, boolean or a list of them", from)
			}
			c.values = []string{s}
		}
		conds = append(conds, c)
	}
	return conds, nil
}

// matchWhen returns "" when every condition holds, else why not.
func matchWhen(conds []whenCond, doc any) string {
	for _, c := range conds {
		raw, found := lookupJSON(doc, c.path)
		got, ok := scalarString(raw)
		if !found || !ok {
			return c.from + " is missing"
		}
		matched := false
		for _, want := range c.values {
			if got == want {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Sprintf("%s is %q, not %s", c.from, truncate(got, 40), strings.Join(c.values, " or "))
		}
	}
	return ""
}

// --- JSON paths --------------------------------------------------------------------

// pathStep is a key, or an array index when index >= 0.
type pathStep struct {
	key   string
	index int
}

var pathKeyRe = regexp.MustCompile(`^[A-Za-z0-9_$@-]+`)

// parseJSONPath parses "a.b[0].c", ["key.with.dots"] or [0] steps.
func parseJSONPath(s string) ([]pathStep, error) {
	var steps []pathStep
	rest := s
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "[\""):
			end := strings.Index(rest[2:], "\"]")
			if end < 0 {
				return nil, fmt.Errorf("%q: unterminated [\"...\"]", s)
			}
			steps = append(steps, pathStep{key: rest[2 : 2+end], index: -1})
			rest = rest[2+end+2:]
		case strings.HasPrefix(rest, "["):
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("%q: unterminated [", s)
			}
			n, err := strconv.Atoi(rest[1:end])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%q: [%s] is not an array index", s, rest[1:end])
			}
			steps = append(steps, pathStep{index: n})
			rest = rest[end+1:]
		default:
			if len(steps) > 0 {
				dot, ok := strings.CutPrefix(rest, ".")
				if !ok {
					return nil, fmt.Errorf("%q: expected . or [ after %q", s, s[:len(s)-len(rest)])
				}
				rest = dot
			}
			key := pathKeyRe.FindString(rest)
			if key == "" {
				return nil, fmt.Errorf("%q: invalid key at %q", s, rest)
			}
			steps = append(steps, pathStep{key: key, index: -1})
			rest = rest[len(key):]
		}
	}
	if len(steps) == 0 {
		return nil, errors.New("empty path")
	}
	return steps, nil
}

func lookupJSON(doc any, path []pathStep) (any, bool) {
	cur := doc
	for _, st := range path {
		if st.index >= 0 {
			arr, ok := cur.([]any)
			if !ok || st.index >= len(arr) {
				return nil, false
			}
			cur = arr[st.index]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = obj[st.key]; !ok {
			return nil, false
		}
	}
	return cur, cur != nil
}

// scalarString renders a JSON (or TOML) scalar; objects and arrays are refused.
func scalarString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false
}

func decodeJSON(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// --- authentication ------------------------------------------------------------------

// verifyGeneric authenticates a generic webhook. With timestamp_header the
// signature covers "<timestamp>.<body>" and old requests are refused.
func verifyGeneric(d *DeployConfig, r *http.Request, body []byte, now time.Time) error {
	switch d.Auth {
	case authToken:
		got := r.Header.Get(d.TokenHeader)
		if strings.EqualFold(d.TokenHeader, "Authorization") {
			var ok bool
			if got, ok = strings.CutPrefix(got, "Bearer "); !ok {
				return errors.New("missing Bearer token")
			}
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), d.secret) != 1 {
			return errors.New("invalid token")
		}
		return nil
	default:
		sig := strings.TrimSpace(r.Header.Get(d.SignatureHeader))
		if sig == "" {
			return fmt.Errorf("missing %s header", d.SignatureHeader)
		}
		signed := body
		if d.TimestampHeader != "" {
			raw := strings.TrimSpace(r.Header.Get(d.TimestampHeader))
			ts, err := parseTimestamp(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", d.TimestampHeader, err)
			}
			if skew := now.Sub(ts); skew > d.MaxSkew.Duration || skew < -d.MaxSkew.Duration {
				return fmt.Errorf("%s is %s away from the server's clock (max %s)", d.TimestampHeader, formatDuration(skew.Abs()), d.MaxSkew)
			}
			signed = append([]byte(raw+"."), body...)
		}
		if !validSignature(d.secret, signed, "sha256="+strings.TrimPrefix(sig, "sha256=")) {
			return errors.New("invalid signature")
		}
		return nil
	}
}

// parseTimestamp accepts Unix seconds or RFC 3339.
func parseTimestamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("missing")
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errors.New("must be Unix seconds or RFC 3339")
	}
	return t, nil
}

// signGeneric produces the signature verifyGeneric expects; used by "nimdeploy send".
func signGeneric(secret, body []byte, timestamp string) string {
	mac := hmac.New(sha256.New, secret)
	if timestamp != "" {
		mac.Write([]byte(timestamp + "."))
	}
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
