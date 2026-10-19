package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// [deploy.<name>.helm] runs helm upgrade --install for one or more releases,
// in order, instead of a wrapper script: the same chart with each release's
// values (e.g. one install per customer), --wait so a release only counts
// when its pods are ready, and --atomic so helm undoes a release that does
// not get there. The first failure stops the run: the releases after it keep
// their version, and with on_failure = "rollback" the ones already upgraded
// in this run go back to their previous revision too. Each release's outcome
// is in the state, the notification, the metrics and the hub.

const (
	helmOnFailureStop     = "stop"
	helmOnFailureRollback = "rollback"
)

type HelmConfig struct {
	// Chart is a path (relative to working_directory), repo/chart or an oci:// URL.
	Chart   string `toml:"chart"`
	Version string `toml:"version"` // chart version, for repository charts
	// Values files for every release, before each release's own.
	Values []string `toml:"values"`
	// Set values for every release (--set-string). Values may use ${PARAM}
	// and ${DEPLOY_TAG}, ${DEPLOY_COMMIT}, ${DEPLOY_BRANCH}.
	Set             map[string]string `toml:"set"`
	Kubeconfig      string            `toml:"kubeconfig"`
	KubeContext     string            `toml:"kube_context"`
	CreateNamespace bool              `toml:"create_namespace"`
	// Timeout is per release (helm --timeout); default 5m. The deploy's
	// timeout still bounds the whole run.
	Timeout Duration `toml:"timeout"`
	// Atomic undoes a release that does not become ready (default true).
	Atomic *bool `toml:"atomic"`
	// OnFailure: "stop" (default) or "rollback" (also roll back the
	// releases this run already upgraded).
	OnFailure string        `toml:"on_failure"`
	Binary    string        `toml:"binary"` // default helm
	Args      []string      `toml:"args"`   // more arguments for every upgrade, as given
	Releases  []HelmRelease `toml:"release"`
	// Discover adds the releases of this chart found in the cluster, after
	// the listed ones: a customer installed by a sale is updated with the
	// rest without editing the config.
	Discover *HelmDiscover `toml:"discover"`

	atomic bool
}

// HelmRelease is one release: [[deploy.<name>.helm.release]]. Client,
// Name, Namespace and Values may use ${PARAM}: one release whose customer
// comes from the webhook (a sale) instead of a fixed list.
type HelmRelease struct {
	// Client is who the release is for, shown with its outcome.
	Client    string            `toml:"client"`
	Name      string            `toml:"name"`
	Namespace string            `toml:"namespace"`
	Values    []string          `toml:"values"`
	Set       map[string]string `toml:"set"`
	// ReuseValues keeps the values the release already has
	// (--reset-then-reuse-values); discovered releases always do.
	ReuseValues bool `toml:"reuse_values"`
	discovered  bool
}

// HelmDiscover finds releases with helm list --all-namespaces. They are
// upgraded with the values they already have (--reset-then-reuse-values)
// plus the shared values and set: they don't need a values file.
type HelmDiscover struct {
	Filter string `toml:"filter"` // release names, a regular expression (helm list --filter)
	Chart  string `toml:"chart"`  // only releases of this chart (its name, without the version)
	filter *regexp.Regexp
}

// HelmSummary is what a run did to each release, in order.
type HelmSummary struct {
	Releases   []HelmResult `json:"releases"`
	Upgraded   int          `json:"upgraded"`
	Failed     int          `json:"failed"`
	RolledBack int          `json:"rolled_back"`
	Skipped    int          `json:"skipped"`
}

// HelmResult is one release's outcome: upgraded, installed, failed,
// rolled_back (upgraded, then undone by on_failure) or skipped (not reached).
type HelmResult struct {
	Client    string `json:"client,omitempty"`
	Release   string `json:"release"`
	Namespace string `json:"namespace"`
	Outcome   string `json:"outcome"`
	Revision  int    `json:"revision,omitempty"` // the release's revision after the run
	Previous  int    `json:"previous,omitempty"` // before it (0: a new install)
	Error     string `json:"error,omitempty"`
}

var (
	// A release name and a namespace: DNS-1123 labels (a release, at most 53).
	helmNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	helmSetKey = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-\[\]]*$`)
	// The run's own values a set may use, besides the params.
	helmBuiltins = map[string]bool{"DEPLOY_TAG": true, "DEPLOY_COMMIT": true, "DEPLOY_BRANCH": true}
)

func (h *HelmConfig) validate(d *DeployConfig) error {
	if d.Command != "" || len(d.Args) > 0 {
		return errors.New("use command or [helm], not both")
	}
	if d.Ansible != nil {
		return errors.New("use [ansible] or [helm], not both")
	}
	if strings.TrimSpace(h.Chart) == "" {
		return errors.New("chart is required")
	}
	if len(h.Releases) == 0 && h.Discover == nil {
		return errors.New("at least one [[release]] (or [discover]) is required")
	}
	if dc := h.Discover; dc != nil {
		if dc.Filter == "" && dc.Chart == "" {
			return errors.New("discover needs filter or chart (never every release in the cluster)")
		}
		if dc.Filter != "" {
			re, err := regexp.Compile(dc.Filter)
			if err != nil {
				return fmt.Errorf("discover.filter: %v", err)
			}
			dc.filter = re
		}
	}
	switch h.OnFailure {
	case "":
		h.OnFailure = helmOnFailureStop
	case helmOnFailureStop, helmOnFailureRollback:
	default:
		return errors.New(`on_failure must be "stop" or "rollback"`)
	}
	if h.Binary == "" {
		h.Binary = "helm"
	}
	if h.Timeout.Duration == 0 {
		h.Timeout.Duration = 5 * time.Minute
	}
	if h.Timeout.Duration < time.Second {
		return errors.New("timeout must be at least 1s")
	}
	h.atomic = h.Atomic == nil || *h.Atomic
	checkSet := func(where string, set map[string]string) error {
		for k, v := range set {
			if !helmSetKey.MatchString(k) {
				return fmt.Errorf("%sset: %q is not a value path (image.tag, env[0].name...)", where, k)
			}
			for _, m := range paramRefRe.FindAllStringSubmatch(v, -1) {
				if _, ok := d.Params[m[1]]; !ok && !helmBuiltins[m[1]] {
					return fmt.Errorf("%sset.%s: ${%s} is not one of the params (nor DEPLOY_TAG, DEPLOY_COMMIT, DEPLOY_BRANCH)", where, k, m[1])
				}
			}
		}
		return nil
	}
	if err := checkSet("", h.Set); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, rel := range h.Releases {
		where := fmt.Sprintf("release %d: ", i+1)
		if rel.Namespace == "" {
			return fmt.Errorf("%snamespace is required", where)
		}
		refs := append([]string{rel.Client, rel.Name, rel.Namespace}, rel.Values...)
		for _, s := range refs {
			for _, m := range paramRefRe.FindAllStringSubmatch(s, -1) {
				if _, ok := d.Params[m[1]]; !ok {
					return fmt.Errorf("%s${%s} is not one of the params", where, m[1])
				}
			}
		}
		// Fixed names are checked now; the ones with ${PARAM}, on each run.
		if !paramRefRe.MatchString(rel.Name + rel.Namespace) {
			if err := checkRelease(rel); err != nil {
				return fmt.Errorf("%s%v", where, err)
			}
			id := rel.Namespace + "/" + rel.Name
			if seen[id] {
				return fmt.Errorf("%s%s appears twice", where, id)
			}
			seen[id] = true
		}
		for _, v := range rel.Values {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("%svalues has an empty entry", where)
			}
		}
		if err := checkSet(where, rel.Set); err != nil {
			return err
		}
	}
	for _, v := range h.Values {
		if strings.TrimSpace(v) == "" {
			return errors.New("values has an empty entry")
		}
	}
	return nil
}

func checkRelease(rel HelmRelease) error {
	if len(rel.Name) > 53 || !helmNameRe.MatchString(rel.Name) {
		return fmt.Errorf("name %q must be lowercase letters, digits and - (at most 53)", rel.Name)
	}
	if len(rel.Namespace) > 63 || !helmNameRe.MatchString(rel.Namespace) {
		return fmt.Errorf("namespace %q must be lowercase letters, digits and - (at most 63)", rel.Namespace)
	}
	return nil
}

// resolveReleases fills in the params of every release for this run and
// checks the result: a param can name a release, never escape it.
func resolveReleases(h *HelmConfig, t Trigger) ([]HelmRelease, error) {
	out := make([]HelmRelease, len(h.Releases))
	seen := map[string]bool{}
	for i, rel := range h.Releases {
		r := rel
		r.Client = strings.TrimSpace(expandParams(rel.Client, t.Params))
		r.Name = expandParams(rel.Name, t.Params)
		r.Namespace = expandParams(rel.Namespace, t.Params)
		if err := checkRelease(r); err != nil {
			return nil, fmt.Errorf("release %d: %v", i+1, err)
		}
		r.Values = nil
		for _, v := range rel.Values {
			x := expandParams(v, t.Params)
			if x != v && (strings.Contains(x, "..") || strings.ContainsAny(strings.TrimSpace(x), "\n\r")) {
				return nil, fmt.Errorf("release %d: values %q: a param can't leave the directory", i+1, x)
			}
			r.Values = append(r.Values, x)
		}
		id := r.Namespace + "/" + r.Name
		if seen[id] {
			return nil, fmt.Errorf("release %d: %s appears twice", i+1, id)
		}
		seen[id] = true
		out[i] = r
	}
	return out, nil
}

// helmExpand replaces ${PARAM} and ${DEPLOY_TAG|COMMIT|BRANCH}.
func helmExpand(s string, t Trigger) string {
	return paramRefRe.ReplaceAllStringFunc(s, func(m string) string {
		switch name := m[2 : len(m)-1]; name {
		case "DEPLOY_TAG":
			return t.Tag
		case "DEPLOY_COMMIT":
			return t.Commit
		case "DEPLOY_BRANCH":
			return t.Branch
		default:
			return paramValue(t.Params, name)
		}
	})
}

// helmEscape keeps a value whole for --set-string: helm splits on commas.
func helmEscape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `,`, `\,`).Replace(v)
}

// helmSets is a release's --set-string list: the shared ones, then its own.
// A set whose value comes out empty stops the run: an empty image.tag
// would mean "whatever the chart defaults to".
func helmSets(h *HelmConfig, rel HelmRelease, t Trigger) ([]string, error) {
	merged := map[string]string{}
	for k, v := range h.Set {
		merged[k] = v
	}
	for k, v := range rel.Set {
		merged[k] = v
	}
	var out []string
	for _, k := range sortedKeys(merged) {
		raw := merged[k]
		v := helmExpand(raw, t)
		if v == "" && raw != "" {
			return nil, fmt.Errorf("set %s = %q is empty for this run", k, raw)
		}
		if strings.ContainsAny(v, "\n\r") {
			return nil, fmt.Errorf("set %s: the value has a line break", k)
		}
		out = append(out, k+"="+helmEscape(v))
	}
	return out, nil
}

// helmGlobal are the flags that pick the cluster.
func helmGlobal(h *HelmConfig) []string {
	var args []string
	if h.Kubeconfig != "" {
		args = append(args, "--kubeconfig", h.Kubeconfig)
	}
	if h.KubeContext != "" {
		args = append(args, "--kube-context", h.KubeContext)
	}
	return args
}

// helmUpgradeArgs builds one release's helm upgrade --install, without a shell.
func helmUpgradeArgs(h *HelmConfig, rel HelmRelease, t Trigger) ([]string, error) {
	sets, err := helmSets(h, rel, t)
	if err != nil {
		return nil, err
	}
	args := []string{"upgrade", "--install", rel.Name, h.Chart, "--namespace", rel.Namespace}
	args = append(args, helmGlobal(h)...)
	if h.CreateNamespace {
		args = append(args, "--create-namespace")
	}
	if h.Version != "" {
		args = append(args, "--version", h.Version)
	}
	args = append(args, "--wait", "--timeout", h.Timeout.Duration.String())
	if h.atomic {
		args = append(args, "--atomic")
	}
	if rel.ReuseValues || rel.discovered {
		args = append(args, "--reset-then-reuse-values")
	}
	for _, v := range append(append([]string(nil), h.Values...), rel.Values...) {
		args = append(args, "--values", v)
	}
	for _, s := range sets {
		args = append(args, "--set-string", s)
	}
	return append(args, h.Args...), nil
}

// helmRevision is a release's current revision (0: not installed).
func (r *Runner) helmRevision(d *DeployConfig, env []string, rel HelmRelease) (int, error) {
	h := d.Helm
	ctx, cancel := context.WithTimeout(r.ctx, time.Minute)
	defer cancel()
	args := append([]string{"status", rel.Name, "--namespace", rel.Namespace, "--output", "json"}, helmGlobal(h)...)
	cmd := exec.CommandContext(ctx, h.Binary, args...)
	cmd.Dir, cmd.Env = d.WorkingDirectory, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "not found") {
			return 0, nil
		}
		return 0, fmt.Errorf("helm status %s/%s: %s", rel.Namespace, rel.Name, firstNonEmpty(lastLine(stderr.String()), err.Error()))
	}
	var st struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		return 0, fmt.Errorf("helm status %s/%s: %v", rel.Namespace, rel.Name, err)
	}
	return st.Version, nil
}

// helmDiscover lists the cluster's releases that match [discover], by namespace.
func (r *Runner) helmDiscover(d *DeployConfig, env []string) ([]HelmRelease, error) {
	h := d.Helm
	ctx, cancel := context.WithTimeout(r.ctx, time.Minute)
	defer cancel()
	args := []string{"list", "--all-namespaces", "--output", "json", "--max", "0"}
	if h.Discover.Filter != "" {
		args = append(args, "--filter", h.Discover.Filter)
	}
	cmd := exec.CommandContext(ctx, h.Binary, append(args, helmGlobal(h)...)...)
	cmd.Dir, cmd.Env = d.WorkingDirectory, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("helm list: %s", firstNonEmpty(lastLine(stderr.String()), err.Error()))
	}
	var list []struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Chart     string `json:"chart"` // name-version
	}
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
		return nil, fmt.Errorf("helm list: %v", err)
	}
	var out []HelmRelease
	for _, l := range list {
		if h.Discover.filter != nil && !h.Discover.filter.MatchString(l.Name) {
			continue
		}
		if c := h.Discover.Chart; c != "" {
			i := strings.LastIndex(l.Chart, "-")
			if i < 0 || l.Chart[:i] != c {
				continue
			}
		}
		rel := HelmRelease{Name: l.Name, Namespace: l.Namespace, discovered: true}
		if checkRelease(rel) == nil {
			out = append(out, rel)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	return out, nil
}

// executeHelm upgrades every release in order and stops at the first failure.
func (r *Runner) executeHelm(d *DeployConfig, t Trigger, env []string, f *os.File, logf func(string, ...any)) (status string, exitCode *int, errMsg string, sum *HelmSummary) {
	h := d.Helm
	releases, err := resolveReleases(h, t)
	if err == nil && h.Discover != nil {
		var found []HelmRelease
		if found, err = r.helmDiscover(d, env); err == nil {
			listed := map[string]bool{}
			for _, rel := range releases {
				listed[rel.Namespace+"/"+rel.Name] = true
			}
			n := 0
			for _, rel := range found {
				if !listed[rel.Namespace+"/"+rel.Name] {
					releases = append(releases, rel)
					n++
				}
			}
			logf("helm: discover found %d releases, %d not listed", len(found), n)
			if len(releases) == 0 {
				err = errors.New("discover found no releases")
			}
		}
	}
	if err != nil {
		logf("helm: %v", err)
		return StatusFailed, nil, "helm: " + err.Error(), nil
	}
	sum = &HelmSummary{}
	for _, rel := range releases {
		sum.Releases = append(sum.Releases, HelmResult{Client: rel.Client, Release: rel.Name, Namespace: rel.Namespace, Outcome: "skipped"})
	}
	status = StatusSuccess
	for i, rel := range releases {
		res := &sum.Releases[i]
		id := res.label()
		fail := func(msg string) {
			res.Outcome, res.Error = "failed", msg
			status, errMsg = StatusFailed, "helm: "+id+": "+msg
		}
		prev, err := r.helmRevision(d, env, rel)
		if err != nil {
			logf("helm: %v", err)
			fail(err.Error())
			break
		}
		res.Previous = prev
		args, err := helmUpgradeArgs(h, rel, t)
		if err != nil {
			logf("helm: %s: %v", id, err)
			fail(err.Error())
			break
		}
		fmt.Fprintln(f)
		logf("helm: [%d/%d]%s %s %s", i+1, len(releases), clientNote(rel.Client), h.Binary, strings.Join(args, " "))
		errs := &helmErrors{}
		st, code, msg := r.runCmdTee(d, env, f, h.Binary, args, errs)
		exitCode = code
		if st != StatusSuccess {
			if msg == errShutdownCanceled || strings.HasPrefix(msg, "timeout") {
				fail(msg)
				errMsg = msg
			} else {
				fail(firstNonEmpty(errs.last(), msg))
			}
			logf("helm: %s failed: %s", id, res.Error)
			if h.atomic && prev > 0 {
				logf("helm: %s is back on revision %d (--atomic)", id, prev)
			}
			break
		}
		res.Outcome = "upgraded"
		if prev == 0 {
			res.Outcome = "installed"
		}
		if rev, err := r.helmRevision(d, env, rel); err == nil {
			res.Revision = rev
		}
		logf("helm: %s %s (revision %d)", id, res.Outcome, res.Revision)
	}
	if status != StatusSuccess && h.OnFailure == helmOnFailureRollback && errMsg != errShutdownCanceled {
		r.helmRollbackDone(d, env, f, sum, logf)
	}
	sum.count()
	fmt.Fprintln(f)
	logf("helm: %s", sum.text())
	return status, exitCode, errMsg, sum
}

// helmRollbackDone takes the releases this run upgraded back to the
// revision they had. New installs stay: there is nothing to go back to.
func (r *Runner) helmRollbackDone(d *DeployConfig, env []string, f *os.File, sum *HelmSummary, logf func(string, ...any)) {
	h := d.Helm
	for i := len(sum.Releases) - 1; i >= 0; i-- {
		res := &sum.Releases[i]
		if res.Outcome != "upgraded" || res.Previous == 0 {
			continue
		}
		id := res.Namespace + "/" + res.Release
		args := []string{"rollback", res.Release, strconv.Itoa(res.Previous), "--namespace", res.Namespace,
			"--wait", "--timeout", h.Timeout.Duration.String()}
		args = append(args, helmGlobal(h)...)
		fmt.Fprintln(f)
		logf("helm: on_failure = rollback: %s %s", h.Binary, strings.Join(args, " "))
		if st, _, msg := r.runCmd(d, env, f, h.Binary, args); st != StatusSuccess {
			logf("helm: rollback of %s failed: %s", id, msg)
			res.Error = "rollback failed: " + msg
			continue
		}
		res.Outcome = "rolled_back"
		if rev, err := r.helmRevision(d, env, HelmRelease{Name: res.Release, Namespace: res.Namespace}); err == nil {
			res.Revision = rev
		}
		logf("helm: %s rolled back to revision %d", id, res.Previous)
	}
}

func clientNote(c string) string {
	if c == "" {
		return ""
	}
	return " " + c + ":"
}

// label is a release in messages: "cliente1 (ns/name)" or "ns/name".
func (r HelmResult) label() string {
	if r.Client != "" {
		return r.Client + " (" + r.Namespace + "/" + r.Release + ")"
	}
	return r.Namespace + "/" + r.Release
}

func (s *HelmSummary) count() {
	s.Upgraded, s.Failed, s.RolledBack, s.Skipped = 0, 0, 0, 0
	for _, r := range s.Releases {
		switch r.Outcome {
		case "upgraded", "installed":
			s.Upgraded++
		case "failed":
			s.Failed++
		case "rolled_back":
			s.RolledBack++
		case "skipped":
			s.Skipped++
		}
	}
}

func (s *HelmSummary) text() string {
	parts := []string{fmt.Sprintf("%d upgraded", s.Upgraded)}
	for _, r := range s.Releases {
		if r.Outcome == "failed" {
			parts = append(parts, "failed: "+r.label())
		}
	}
	if s.RolledBack > 0 {
		parts = append(parts, fmt.Sprintf("%d rolled back", s.RolledBack))
	}
	if s.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d not reached", s.Skipped))
	}
	return fmt.Sprintf("%d releases: %s", len(s.Releases), strings.Join(parts, ", "))
}

// helmErrors keeps helm's last "Error: ..." line for the failure message.
type helmErrors struct {
	mu   sync.Mutex
	buf  []byte
	line string
}

func (e *helmErrors) Write(b []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.buf = append(e.buf, b...)
	for {
		i := bytes.IndexByte(e.buf, '\n')
		if i < 0 {
			break
		}
		e.take(string(e.buf[:i]))
		e.buf = e.buf[i+1:]
	}
	if len(e.buf) > 64<<10 {
		e.buf = e.buf[:0]
	}
	return len(b), nil
}

func (e *helmErrors) take(l string) {
	if msg, ok := strings.CutPrefix(strings.TrimSpace(l), "Error: "); ok {
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		e.line = msg
	}
}

func (e *helmErrors) last() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.buf) > 0 {
		e.take(string(e.buf))
		e.buf = nil
	}
	return e.line
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
