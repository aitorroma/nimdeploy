package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// [deploy.<name>.ansible] runs ansible-playbook (or ansible-pull) directly,
// instead of a wrapper script: inventory, limit, tags and extra vars come
// from the config and from the webhook's validated params, the command line
// is built without a shell, and the PLAY RECAP is summarised in the state,
// the notification, the metrics and the hub.

const (
	ansibleModePlaybook = "playbook"
	ansibleModePull     = "pull"
)

type AnsibleConfig struct {
	// Mode is "playbook" (ansible-playbook, default) or "pull" (ansible-pull:
	// fetch the playbooks from git and run them on this host).
	Mode     string `toml:"mode"`
	Playbook string `toml:"playbook"` // relative to working_directory (pull: to the checkout)
	// Inventory: files, directories or inventory plugin configs.
	Inventory []string `toml:"inventory"`
	// Limit, Tags, SkipTags, ExtraVars and Checkout may use ${PARAM}: the
	// value of a declared param, already validated.
	Limit     string         `toml:"limit"`
	Tags      []string       `toml:"tags"`
	SkipTags  []string       `toml:"skip_tags"`
	ExtraVars map[string]any `toml:"extra_vars"`
	// ParamsAsVars passes every param as an extra var, lowercased
	// (SERVICE → service). Default true.
	ParamsAsVars *bool  `toml:"params_as_vars"`
	Check        bool   `toml:"check"`
	Diff         bool   `toml:"diff"`
	Forks        int    `toml:"forks"`
	Verbosity    int    `toml:"verbosity"` // 0-4: -v ... -vvvv
	Config       string `toml:"config"`    // ANSIBLE_CONFIG
	// VaultPasswordFile is a file (or script) with the vault password; it is
	// never read by nimdeploy.
	VaultPasswordFile string   `toml:"vault_password_file"`
	Binary            string   `toml:"binary"` // default ansible-playbook / ansible-pull
	Args              []string `toml:"args"`   // more arguments, as given

	// ansible-pull
	URL           string `toml:"url"`
	Checkout      string `toml:"checkout"`
	Directory     string `toml:"directory"`
	OnlyIfChanged bool   `toml:"only_if_changed"`

	paramsAsVars bool
}

// AnsibleSummary adds up the PLAY RECAP of a run.
type AnsibleSummary struct {
	Hosts       int `json:"hosts"`
	Ok          int `json:"ok"`
	Changed     int `json:"changed"`
	Unreachable int `json:"unreachable"`
	Failed      int `json:"failed"`
	Skipped     int `json:"skipped"`
	Rescued     int `json:"rescued"`
	Ignored     int `json:"ignored"`
	// Hosts by outcome: failed, else unreachable, else changed, else ok.
	HostsFailed      int      `json:"hosts_failed"`
	HostsUnreachable int      `json:"hosts_unreachable"`
	HostsChanged     int      `json:"hosts_changed"`
	HostsOk          int      `json:"hosts_ok"`
	FailedHosts      []string `json:"failed_hosts,omitempty"`
	// UnreachableHosts are capped like FailedHosts, at 20.
	UnreachableHosts []string `json:"unreachable_hosts,omitempty"`
}

var (
	paramRefRe   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	extraVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// "web1 : ok=3 changed=1 unreachable=0 failed=0 skipped=2 rescued=0 ignored=0"
	recapLineRe = regexp.MustCompile(`^(\S+)\s+:\s+ok=(\d+)\s+changed=(\d+)\s+unreachable=(\d+)\s+failed=(\d+)(?:\s+skipped=(\d+))?(?:\s+rescued=(\d+))?(?:\s+ignored=(\d+))?`)
)

func (a *AnsibleConfig) validate(d *DeployConfig) error {
	if d.Command != "" || len(d.Args) > 0 {
		return errors.New("use command or [ansible], not both")
	}
	switch a.Mode {
	case "":
		a.Mode = ansibleModePlaybook
	case ansibleModePlaybook, ansibleModePull:
	default:
		return errors.New("mode must be playbook or pull")
	}
	if a.Mode == ansibleModePlaybook {
		if a.Playbook == "" {
			return errors.New("playbook is required")
		}
		if a.URL != "" || a.Checkout != "" || a.Directory != "" || a.OnlyIfChanged {
			return errors.New("url, checkout, directory and only_if_changed are for mode = \"pull\"")
		}
	} else if a.URL == "" {
		return errors.New("url (the git repository with the playbooks) is required for mode = \"pull\"")
	}
	if a.Binary == "" {
		a.Binary = "ansible-" + a.Mode
	}
	for _, inv := range a.Inventory {
		if strings.TrimSpace(inv) == "" {
			return errors.New("inventory has an empty entry")
		}
	}
	if a.Verbosity < 0 || a.Verbosity > 4 {
		return errors.New("verbosity must be 0-4")
	}
	if a.Forks < 0 {
		return errors.New("forks must be positive")
	}
	a.paramsAsVars = a.ParamsAsVars == nil || *a.ParamsAsVars
	if err := checkVarNames(a.ExtraVars); err != nil {
		return err
	}
	if _, clash := a.ExtraVars["nimdeploy"]; clash {
		return errors.New("extra_vars.nimdeploy is reserved (nimdeploy sets it)")
	}
	refs := []string{a.Limit, a.Checkout}
	refs = append(refs, a.Tags...)
	refs = append(refs, a.SkipTags...)
	refs = append(refs, stringsIn(a.ExtraVars)...)
	for _, s := range refs {
		for _, m := range paramRefRe.FindAllStringSubmatch(s, -1) {
			if _, ok := d.Params[m[1]]; !ok {
				return fmt.Errorf("${%s} is not one of the params", m[1])
			}
		}
	}
	if a.paramsAsVars {
		for name := range d.Params {
			if _, clash := a.ExtraVars[strings.ToLower(name)]; clash {
				return fmt.Errorf("extra_vars.%s is also param %s (set params_as_vars = false or rename it)", strings.ToLower(name), name)
			}
		}
	}
	return nil
}

func checkVarNames(vars map[string]any) error {
	for k := range vars {
		if !extraVarName.MatchString(k) {
			return fmt.Errorf("extra_vars: %q is not a valid variable name", k)
		}
	}
	return nil
}

// stringsIn collects every string in a TOML value, for ${PARAM} checks.
func stringsIn(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case map[string]any:
		var out []string
		for _, k := range sortedKeys(x) {
			out = append(out, stringsIn(x[k])...)
		}
		return out
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, stringsIn(e)...)
		}
		return out
	}
	return nil
}

// expandParams replaces ${PARAM} with its value; missing params are empty.
func expandParams(s string, params []Param) string {
	return paramRefRe.ReplaceAllStringFunc(s, func(m string) string {
		return paramValue(params, m[2:len(m)-1])
	})
}

func expandValue(v any, params []Param) any {
	switch x := v.(type) {
	case string:
		return expandParams(x, params)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = expandValue(e, params)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = expandValue(e, params)
		}
		return out
	}
	return v
}

// ansibleVars is everything passed with -e @file: extra_vars, the params and
// a "nimdeploy" dict describing the run.
func ansibleVars(d *DeployConfig, t Trigger, run string) map[string]any {
	a := d.Ansible
	vars := map[string]any{}
	params := map[string]string{}
	for _, p := range t.Params {
		params[p.Name] = p.Value
		if a.paramsAsVars {
			vars[strings.ToLower(p.Name)] = p.Value
		}
	}
	for k, v := range a.ExtraVars {
		vars[k] = expandValue(v, t.Params)
	}
	labels := map[string]string{}
	for k, v := range d.labels {
		labels[k] = v
	}
	vars["nimdeploy"] = map[string]any{
		"deploy": d.Name, "run": run, "trigger": t.Source, "provider": t.Provider,
		"repository": t.Repository, "ref": t.Ref, "branch": t.Branch, "commit": t.Commit,
		"pusher": t.Pusher, "delivery": t.Delivery, "event": t.Event, "resource_id": t.ResourceID,
		"labels": labels, "params": params,
	}
	return vars
}

// ansibleArgs builds the command line. varsFile holds the extra vars.
func ansibleArgs(a *AnsibleConfig, params []Param, varsFile string) ([]string, error) {
	var args []string
	if a.Mode == ansibleModePull {
		args = append(args, "--url", a.URL)
		if a.Checkout != "" {
			c := expandParams(a.Checkout, params)
			if c == "" {
				return nil, errors.New("checkout is empty: its param has no value")
			}
			args = append(args, "--checkout", c)
		}
		if a.Directory != "" {
			args = append(args, "--directory", a.Directory)
		}
		if a.OnlyIfChanged {
			args = append(args, "--only-if-changed")
		}
	}
	for _, inv := range a.Inventory {
		args = append(args, "--inventory", inv)
	}
	if a.Limit != "" {
		limit := expandParams(a.Limit, params)
		if strings.Trim(limit, ",:&! ") == "" {
			// An empty limit would mean every host: never guess that.
			return nil, fmt.Errorf("limit %q is empty for this run: its param has no value", a.Limit)
		}
		args = append(args, "--limit", limit)
	}
	join := func(list []string) string {
		var out []string
		for _, s := range list {
			if s = strings.TrimSpace(expandParams(s, params)); s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, ",")
	}
	if tags := join(a.Tags); tags != "" {
		args = append(args, "--tags", tags)
	}
	if skip := join(a.SkipTags); skip != "" {
		args = append(args, "--skip-tags", skip)
	}
	if a.Check {
		args = append(args, "--check")
	}
	if a.Diff {
		args = append(args, "--diff")
	}
	if a.Forks > 0 {
		args = append(args, "--forks", strconv.Itoa(a.Forks))
	}
	if a.VaultPasswordFile != "" {
		args = append(args, "--vault-password-file", a.VaultPasswordFile)
	}
	if varsFile != "" {
		args = append(args, "--extra-vars", "@"+varsFile)
	}
	if a.Verbosity > 0 {
		args = append(args, "-"+strings.Repeat("v", a.Verbosity))
	}
	args = append(args, a.Args...)
	if a.Playbook != "" {
		args = append(args, a.Playbook)
	}
	return args, nil
}

// ansibleEnv makes the output plain and predictable for the log and the recap.
func ansibleEnv(a *AnsibleConfig) []string {
	env := []string{"ANSIBLE_NOCOLOR=1", "ANSIBLE_FORCE_COLOR=0", "ANSIBLE_RETRY_FILES_ENABLED=0", "PYTHONUNBUFFERED=1"}
	if a.Config != "" {
		env = append(env, "ANSIBLE_CONFIG="+a.Config)
	}
	return env
}

// executeAnsible runs ansible for a deploy and summarises the recap.
func (r *Runner) executeAnsible(d *DeployConfig, t Trigger, env []string, f *os.File, logPath string, logf func(string, ...any)) (status string, exitCode *int, errMsg string, sum *AnsibleSummary) {
	a := d.Ansible
	varsFile := runFile(logPath, "ansible-vars.json")
	b, err := json.Marshal(ansibleVars(d, t, runID(logPath)))
	if err == nil {
		err = os.WriteFile(varsFile, b, 0o600)
	}
	if err != nil {
		return StatusFailed, nil, "ansible: cannot write the extra vars: " + err.Error(), nil
	}
	defer os.Remove(varsFile)

	args, err := ansibleArgs(a, t.Params, varsFile)
	if err != nil {
		logf("ansible: %v", err)
		return StatusFailed, nil, "ansible: " + err.Error(), nil
	}
	logf("ansible: %s %s", a.Binary, strings.Join(args, " "))
	fmt.Fprintln(f)
	recap := &recapParser{}
	status, exitCode, errMsg = r.runCmdTee(d, append(env, ansibleEnv(a)...), f, a.Binary, args, recap)
	sum = recap.summary()
	fmt.Fprintln(f)
	if sum == nil {
		if status == StatusSuccess && a.Mode == ansibleModePull && a.OnlyIfChanged {
			logf("ansible: no changes in %s, nothing ran", a.URL)
		} else {
			logf("ansible: no PLAY RECAP in the output (a custom stdout callback?)")
		}
		return status, exitCode, errMsg, nil
	}
	logf("ansible: %s", sum.text())
	if status != StatusSuccess && errMsg != errShutdownCanceled && !strings.HasPrefix(errMsg, "timeout") {
		if why := sum.failure(); why != "" {
			errMsg = "ansible: " + why
		}
	}
	return status, exitCode, errMsg, sum
}

func (s *AnsibleSummary) text() string {
	return fmt.Sprintf("%d hosts: ok=%d changed=%d unreachable=%d failed=%d skipped=%d rescued=%d ignored=%d",
		s.Hosts, s.Ok, s.Changed, s.Unreachable, s.Failed, s.Skipped, s.Rescued, s.Ignored)
}

func (s *AnsibleSummary) failure() string {
	var parts []string
	if len(s.FailedHosts) > 0 {
		parts = append(parts, "failed on "+listHosts(s.FailedHosts, s.Failed))
	}
	if len(s.UnreachableHosts) > 0 {
		parts = append(parts, "unreachable: "+listHosts(s.UnreachableHosts, len(s.UnreachableHosts)))
	}
	return strings.Join(parts, "; ")
}

func listHosts(hosts []string, _ int) string {
	if len(hosts) > 5 {
		return strings.Join(hosts[:5], ", ") + fmt.Sprintf(" and %d more", len(hosts)-5)
	}
	return strings.Join(hosts, ", ")
}

// recapParser watches ansible's output for PLAY RECAP lines. A host seen in
// several recaps (several plays or playbooks) counts with its last line.
type recapParser struct {
	mu    sync.Mutex
	buf   []byte
	hosts map[string][7]int
	order []string
}

func (p *recapParser) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		p.line(string(p.buf[:i]))
		p.buf = p.buf[i+1:]
	}
	if len(p.buf) > 64<<10 { // a huge line without newline: not a recap line
		p.buf = p.buf[:0]
	}
	return len(b), nil
}

func (p *recapParser) line(l string) {
	m := recapLineRe.FindStringSubmatch(strings.TrimSpace(l))
	if m == nil {
		return
	}
	var n [7]int
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+2])
	}
	if p.hosts == nil {
		p.hosts = map[string][7]int{}
	}
	if _, seen := p.hosts[m[1]]; !seen {
		p.order = append(p.order, m[1])
	}
	p.hosts[m[1]] = n
}

func (p *recapParser) summary() *AnsibleSummary {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) > 0 {
		p.line(string(p.buf))
		p.buf = nil
	}
	if len(p.hosts) == 0 {
		return nil
	}
	s := &AnsibleSummary{Hosts: len(p.hosts)}
	hosts := append([]string(nil), p.order...)
	sort.Strings(hosts)
	for _, h := range hosts {
		n := p.hosts[h]
		s.Ok += n[0]
		s.Changed += n[1]
		s.Unreachable += n[2]
		s.Failed += n[3]
		s.Skipped += n[4]
		s.Rescued += n[5]
		s.Ignored += n[6]
		switch {
		case n[3] > 0:
			s.HostsFailed++
		case n[2] > 0:
			s.HostsUnreachable++
		case n[1] > 0:
			s.HostsChanged++
		default:
			s.HostsOk++
		}
		if n[3] > 0 && len(s.FailedHosts) < 20 {
			s.FailedHosts = append(s.FailedHosts, h)
		}
		if n[2] > 0 && len(s.UnreachableHosts) < 20 {
			s.UnreachableHosts = append(s.UnreachableHosts, h)
		}
	}
	return s
}

var _ io.Writer = (*recapParser)(nil)
