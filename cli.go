package main

import (
	"bufio"
	"bytes"
	"context"
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
	fset.Parse(args)
	if fset.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: nimdeploy run [-commit SHA] [-f] <deploy>")
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
	if _, err := c.do(http.MethodPost, "/deploy/"+name, manualRequest{Commit: *commit, User: user}, &res); err != nil {
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
		if st.Log != logName || st.Status != StatusRunning {
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
	fset.Parse(args)
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

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEPLOY\tSTATUS\tSTARTED\tDURATION\tCOMMIT\tBY\tLOG")
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
		if st.Queued != nil {
			status += " (+1 queued)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, status, started, dash(st.Duration), dash(shortSHA(st.Commit)), dash(st.Pusher), dash(st.Log))
	}
	tw.Flush()
	for _, name := range cfg.DeployNames() {
		if st := states[name]; st.Status == StatusFailed && st.Error != "" {
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
