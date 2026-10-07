package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// client talks to the running server on its local listen address.
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(cfg *Config, envFile string) (*client, error) {
	c := &client{http: &http.Client{Timeout: 15 * time.Second}}
	if sock := cfg.Server.socketPath; sock != "" {
		c.base = "http://nimdeploy"
		c.http.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}
	} else {
		host, port, err := net.SplitHostPort(cfg.Server.Listen)
		if err != nil {
			return nil, fmt.Errorf("server.listen: %w", err)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		c.base = "http://" + net.JoinHostPort(host, port)
	}
	c.base += cfg.Server.BasePath

	var err error

	if name := cfg.Server.APITokenEnv; name != "" {
		c.token = os.Getenv(name)
		if c.token == "" {
			c.token, err = readEnvFile(envFile, name)
			if errors.Is(err, fs.ErrPermission) {
				return nil, fmt.Errorf("cannot read %s: run with sudo or export %s", envFile, name)
			}
			if err != nil {
				return nil, fmt.Errorf("api token: %w", err)
			}
		}
	}
	return c, nil
}

func (c *client) do(method, path string, body any, out any) (int, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("is nimdeploy running? %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		var e struct{ Error, Reason string }
		_ = json.Unmarshal(b, &e)
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s%s", resp.StatusCode, e.Error, e.Reason)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(b, out)
	}
	return resp.StatusCode, nil
}

func cliRun(cfg *Config, envFile string, args []string) int {
	fset := flag.NewFlagSet("run", flag.ExitOnError)
	commit := fset.String("commit", "", "commit to deploy (default: the script decides, usually the branch head)")
	follow := fset.Bool("f", false, "follow the log until the deploy finishes; exit code reflects the result")
	params := paramFlag{}
	fset.Var(params, "p", "param for the deploy, NAME=VALUE (repeatable; validated like webhook params)")
	fset.Parse(args)
	if fset.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: nimdeploy run [-commit SHA] [-p NAME=VALUE]... [-f] <deploy>")
		return 2
	}
	name := fset.Arg(0)
	if _, ok := cfg.Deploy[name]; !ok {
		fmt.Fprintf(os.Stderr, "unknown deploy %q (have: %s)\n", name, strings.Join(cfg.DeployNames(), ", "))
		return 2
	}
	c, err := newClient(cfg, envFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	user := os.Getenv("SUDO_USER")
	if user == "" {
		user = os.Getenv("USER")
	}
	var res SubmitResult
	if _, err := c.do(http.MethodPost, "/deploy/"+name, manualRequest{Commit: *commit, User: user, Params: params}, &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if res.Result == ResultQueued {
		fmt.Printf("%s: queued, will run after the current deploy (log %s)\n", name, res.State.Log)
		return 0
	}
	logPath := filepath.Join(cfg.Logging.Directory, name, res.State.Log)
	fmt.Printf("%s: started, log %s\n", name, logPath)
	if !*follow {
		return 0
	}
	return followLog(c, name, logPath, res.State.Log)
}

// followLog prints the log as it grows until the deploy that wrote it finishes.
func followLog(c *client, name, path, logName string) int {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot follow log: %v\n", err)
		return 1
	}
	defer f.Close()
	for {
		_, _ = io.Copy(os.Stdout, f)
		var st State
		if _, err := c.do(http.MethodGet, "/status/"+name, nil, &st); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if st.Log != logName || !isActive(st.Status) {
			_, _ = io.Copy(os.Stdout, f)
			if st.Log == logName && st.Status == StatusSuccess {
				return 0
			}
			if st.Log != logName {
				return 0 // a newer run replaced ours in /status; the log above has our result
			}
			return 1
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func cliStatus(cfg *Config, envFile string, args []string) int {
	fset := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fset.Bool("json", false, "print raw JSON")
	wide := fset.Bool("wide", false, "also show each deploy's labels")
	var labelSpecs listFlag
	fset.Var(&labelSpecs, "l", "only deploys with this label, key=value (repeatable)")
	fset.Parse(args)
	filter, err := parseLabelFilter(labelSpecs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	c, err := newClient(cfg, envFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	states := map[string]State{}
	if fset.NArg() > 0 {
		var st State
		_, err = c.do(http.MethodGet, "/status/"+fset.Arg(0), nil, &st)
		states[st.Deploy] = st
	} else {
		_, err = c.do(http.MethodGet, "/status", nil, &states)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for name, st := range states {
		if !filter.match(st.Labels) {
			delete(states, name)
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if fset.NArg() > 0 {
			_ = enc.Encode(states[fset.Arg(0)])
		} else {
			_ = enc.Encode(states)
		}
		return 0
	}

	withParams, withNext := false, false
	for _, st := range states {
		withParams = withParams || len(st.Params) > 0
		withNext = withNext || st.NextRun != nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	header := "DEPLOY\tSTATUS\tSTARTED\tDURATION\tCOMMIT\tBY\tLOG"
	if withNext {
		header += "\tNEXT RUN"
	}
	if withParams {
		header += "\tPARAMS"
	}
	if *wide {
		header += "\tLABELS"
	}
	fmt.Fprintln(tw, header)
	for _, name := range cfg.DeployNames() {
		st, ok := states[name]
		if !ok {
			continue
		}
		started := "-"
		if st.StartedAt != nil {
			started = st.StartedAt.Local().Format("2006-01-02 15:04:05")
		}
		status := st.Status
		if st.Status == StatusWaiting {
			status = "waiting (CI)"
		}
		if st.Queued != nil {
			status += fmt.Sprintf(" (+%d queued)", max(1, st.Queued.Count))
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s", name, status, started, dash(st.Duration), dash(commitOrEvent(st)), dash(st.Pusher), dash(st.Log))
		if withNext {
			next := "-"
			if st.NextRun != nil {
				next = st.NextRun.Local().Format("2006-01-02 15:04:05")
			}
			line += "\t" + next
		}
		if withParams {
			line += "\t" + dash(formatParams(st.Params))
		}
		if *wide {
			line += "\t" + dash(formatLabels(st.Labels))
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
	for _, name := range cfg.DeployNames() {
		if st := states[name]; (st.Status == StatusFailed || st.Status == StatusSkipped) && st.Error != "" {
			fmt.Printf("\n%s: %s\n", name, st.Error)
		}
	}
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// readEnvFile returns key's value from a systemd EnvironmentFile.
func readEnvFile(path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		return v, nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s not found in %s", key, path)
}

func cliNginx(cfg *Config, args []string) int {
	fset := flag.NewFlagSet("nginx", flag.ExitOnError)
	api := fset.Bool("api", false, "also publish /status and /deploy (requires server.api_token_env)")
	fset.Parse(args)
	if *api && cfg.Server.APITokenEnv == "" {
		fmt.Fprintln(os.Stderr, "refusing -api without server.api_token_env: /status would be public")
		return 1
	}
	fmt.Print(nginxSnippet(cfg, *api))
	return 0
}

// nginxSnippet returns location blocks that forward only the configured hook
// paths (and optionally the API) to nimdeploy. Exact and ^~ matches win over
// the regex locations of typical site configs (static files, PHP).
func nginxSnippet(cfg *Config, api bool) string {
	upstream := "http://unix:" + cfg.Server.socketPath
	if cfg.Server.socketPath == "" {
		host, port, _ := net.SplitHostPort(cfg.Server.Listen)
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		upstream = "http://" + net.JoinHostPort(host, port)
	}
	maxMB := (cfg.Server.MaxBodyBytes + 1<<20 - 1) >> 20
	base := cfg.Server.BasePath

	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by `nimdeploy nginx` (%s). Regenerate after changing hook paths.\n", version)
	block := func(match, comment string) {
		fmt.Fprintf(&b, `
# %s
location %s {
    proxy_pass %s;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
    client_max_body_size %dm;
}
`, comment, match, upstream, maxMB)
	}
	for _, name := range cfg.DeployNames() {
		if cfg.Deploy[name].Path == "" {
			continue // scheduled only: no webhook
		}
		block("= "+base+cfg.Deploy[name].Path, "deploy."+name)
	}
	if api {
		block("= "+base+"/status", "API: status (bearer token)")
		block("^~ "+base+"/status/", "API: status of one deploy (bearer token)")
		block("^~ "+base+"/deploy/", "API: manual deploys (bearer token)")
	}
	return b.String()
}

func cliHistory(cfg *Config, envFile string, args []string) int {
	fset := flag.NewFlagSet("history", flag.ExitOnError)
	limit := fset.Int("n", 20, "number of deploys to show")
	asJSON := fset.Bool("json", false, "print raw JSON")
	var labelSpecs listFlag
	fset.Var(&labelSpecs, "l", "only deploys with this label, key=value (repeatable)")
	fset.Parse(args)
	filter, ferr := parseLabelFilter(labelSpecs)
	if ferr != nil {
		fmt.Fprintln(os.Stderr, ferr)
		return 2
	}
	names := cfg.DeployNames()
	if fset.NArg() > 0 {
		names = fset.Args()[:1]
		if _, ok := cfg.Deploy[names[0]]; !ok {
			fmt.Fprintf(os.Stderr, "unknown deploy %q (have: %s)\n", names[0], strings.Join(cfg.DeployNames(), ", "))
			return 2
		}
	}
	c, err := newClient(cfg, envFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var all []State
	for _, name := range names {
		if d := cfg.Deploy[name]; !filter.match(d.labels) {
			continue
		}
		var h []State
		if _, err := c.do(http.MethodGet, fmt.Sprintf("/history/%s?limit=%d", name, *limit), nil, &h); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		all = append(all, h...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Log > all[j].Log })
	if len(all) > *limit {
		all = all[:*limit]
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(all)
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEPLOY\tSTATUS\tSTARTED\tDURATION\tTRIGGER\tCOMMIT\tBY\tLOG\tNOTE")
	for _, st := range all {
		started := "-"
		if st.StartedAt != nil {
			started = st.StartedAt.Local().Format("2006-01-02 15:04:05")
		}
		note := st.Error
		if note == "" {
			note = formatParams(st.Params)
		}
		if len(note) > 60 {
			note = note[:57] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", st.Deploy, st.Status, started, dash(st.Duration),
			dash(st.Trigger), dash(commitOrEvent(st)), dash(st.Pusher), dash(st.Log), note)
	}
	tw.Flush()
	if len(all) == 0 {
		fmt.Println("no deploys yet")
	}
	return 0
}

func cliRollback(cfg *Config, envFile string, args []string) int {
	fset := flag.NewFlagSet("rollback", flag.ExitOnError)
	to := fset.String("to", "", "commit to go back to (default: the last successful deploy before the latest one)")
	follow := fset.Bool("f", false, "follow the log until it finishes; exit code reflects the result")
	dry := fset.Bool("n", false, "only show which commit it would deploy")
	fset.Parse(args)
	if fset.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: nimdeploy rollback [-to SHA] [-n] [-f] <deploy>")
		return 2
	}
	name := fset.Arg(0)
	if _, ok := cfg.Deploy[name]; !ok {
		fmt.Fprintf(os.Stderr, "unknown deploy %q (have: %s)\n", name, strings.Join(cfg.DeployNames(), ", "))
		return 2
	}
	c, err := newClient(cfg, envFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *dry {
		if *to != "" {
			fmt.Printf("%s: would deploy %s\n", name, *to)
			return 0
		}
		var h []State
		if _, err := c.do(http.MethodGet, "/history/"+name, nil, &h); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		commit, from, err := rollbackTargetIn(h)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("%s: would deploy %s (deployed successfully by %s)\n", name, commit, from)
		return 0
	}
	user := firstNonEmpty(os.Getenv("SUDO_USER"), os.Getenv("USER"), "cli")
	var res SubmitResult
	if _, err := c.do(http.MethodPost, "/rollback/"+name, manualRequest{Commit: *to, User: user}, &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("%s: rolling back to %s (%s, log %s)\n", name, shortSHA(res.State.Commit), res.Result, res.State.Log)
	if res.Result == ResultQueued {
		fmt.Printf("  queued: it runs after the current deploy (queued commit %s)\n", shortSHA(res.State.Queued.Commit))
		return 0
	}
	if !*follow {
		return 0
	}
	return followLog(c, name, filepath.Join(cfg.Logging.Directory, name, res.State.Log), res.State.Log)
}

// eventRef is "order.updated #1234" for non-git events.
func eventRef(st State) string {
	switch {
	case st.Event != "" && st.ResourceID != "":
		return st.Event + " #" + st.ResourceID
	case st.ResourceID != "":
		return "#" + st.ResourceID
	}
	return st.Event
}

// commitOrEvent fills the COMMIT column: the commit, or what the event was about.
func commitOrEvent(st State) string {
	if st.Commit != "" {
		return shortSHA(st.Commit)
	}
	return eventRef(st)
}

// paramFlag collects repeated -p NAME=VALUE flags.
type paramFlag map[string]string

func (p paramFlag) String() string { return formatParams(nil) }

func (p paramFlag) Set(v string) error {
	name, value, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("want NAME=VALUE, got %q", v)
	}
	p[name] = value
	return nil
}

// cliSend posts a JSON body to a generic webhook, signed or with a token, so
// Ansible, CI jobs and scripts don't have to compute HMACs themselves. It
// needs no config: only the URL and the secret.
func cliSend(args []string) int {
	fset := flag.NewFlagSet("send", flag.ExitOnError)
	data := fset.String("data", "", "JSON body, or @file, or @- for stdin (default: {})")
	secretEnv := fset.String("secret-env", "NIMDEPLOY_SECRET", "environment variable holding the shared secret or token")
	auth := fset.String("auth", authHMAC, "hmac (sign the body) or token")
	sigHeader := fset.String("signature-header", defaultSignatureHeader, "header for the HMAC signature")
	tokenHeader := fset.String("token-header", defaultTokenHeader, `header for the token ("Authorization" sends "Bearer <token>")`)
	tsHeader := fset.String("timestamp-header", "", "also send the current Unix time in this header and sign it (the deploy's timestamp_header)")
	deliveryHeader := fset.String("delivery-header", defaultDeliveryHeader, "header with a unique ID for this request (empty: don't send)")
	timeout := fset.Duration("timeout", 30*time.Second, "request timeout")
	fset.Usage = func() {
		fmt.Fprintln(fset.Output(), "usage: nimdeploy send [flags] <url>\n\nPOST a signed JSON body to a nimdeploy generic webhook.\n\nFlags:")
		fset.PrintDefaults()
	}
	fset.Parse(args)
	if fset.NArg() != 1 {
		fset.Usage()
		return 2
	}
	url := fset.Arg(0)
	secret := os.Getenv(*secretEnv)
	if secret == "" {
		fmt.Fprintf(os.Stderr, "nimdeploy send: environment variable %s is empty; export the webhook's secret there or use -secret-env\n", *secretEnv)
		return 2
	}

	body := []byte(*data)
	switch {
	case *data == "":
		body = []byte("{}")
	case *data == "@-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "nimdeploy send:", err)
			return 1
		}
		body = b
	case strings.HasPrefix(*data, "@"):
		b, err := os.ReadFile((*data)[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "nimdeploy send:", err)
			return 1
		}
		body = b
	}
	if !json.Valid(body) {
		fmt.Fprintln(os.Stderr, "nimdeploy send: the body is not valid JSON")
		return 2
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "nimdeploy send:", err)
		return 2
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nimdeploy-send/"+version)
	switch *auth {
	case authHMAC:
		ts := ""
		if *tsHeader != "" {
			ts = fmt.Sprint(time.Now().Unix())
			req.Header.Set(*tsHeader, ts)
		}
		req.Header.Set(*sigHeader, signGeneric([]byte(secret), body, ts))
	case authToken:
		if strings.EqualFold(*tokenHeader, "Authorization") {
			req.Header.Set("Authorization", "Bearer "+secret)
		} else {
			req.Header.Set(*tokenHeader, secret)
		}
	default:
		fmt.Fprintln(os.Stderr, "nimdeploy send: -auth must be hmac or token")
		return 2
	}
	if *deliveryHeader != "" {
		req.Header.Set(*deliveryHeader, newDeliveryID())
	}

	resp, err := (&http.Client{Timeout: *timeout}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nimdeploy send:", err)
		return 1
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Printf("HTTP %d\n%s", resp.StatusCode, out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		fmt.Println()
	}
	if resp.StatusCode/100 != 2 {
		return 1
	}
	return 0
}

// newDeliveryID returns a random UUID (v4).
func newDeliveryID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
