package main

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// History returns the deploys whose logs are still kept (see logging.retain),
// newest first. It is rebuilt from the log headers and footers, so it covers
// runs from before any restart.
func (r *Runner) History(name string, limit int) ([]State, error) {
	dir := filepath.Join(r.dir, name)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var logs []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(n, ".log") && !strings.HasPrefix(n, ".") {
			logs = append(logs, n)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(logs)))
	if limit > 0 && len(logs) > limit {
		logs = logs[:limit]
	}

	r.mu.Lock()
	current := State{}
	if st := r.states[name]; st != nil {
		current = copyState(st)
	}
	active := map[string]bool{}
	for p := range r.active {
		active[p] = true
	}
	r.mu.Unlock()

	history := []State{}
	for _, n := range logs {
		path := filepath.Join(dir, n)
		st := parseLog(path)
		st.Log = n
		if st.Deploy == "" {
			st.Deploy = name
		}
		if st.Status == "" {
			// No footer: still running, or nimdeploy stopped mid-deploy.
			st.Status = StatusInterrupted
			if active[path] && current.Log == n {
				st.Status = current.Status
			}
		}
		history = append(history, st)
	}
	return history, nil
}

// parseLog reads the "<RFC3339> key=value" header (up to the first blank
// line) and footer (after the last blank line) that run() writes.
func parseLog(path string) State {
	var st State
	f, err := os.Open(path)
	if err != nil {
		return st
	}
	defer f.Close()

	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		ts, key, val, ok := splitLogLine(line)
		if !ok {
			continue
		}
		switch key {
		case "deploy":
			st.Deploy, _, _ = strings.Cut(val, " ")
			st.StartedAt = &ts
		case "trigger":
			st.Trigger = val
		case "provider":
			st.Provider = val
		case "repository":
			st.Repository = val
		case "branch":
			st.Branch = val
		case "commit":
			st.Commit = val
		case "pusher":
			st.Pusher = val
		case "delivery":
			st.Delivery = val
		default:
			if name, ok := strings.CutPrefix(key, "param."); ok {
				st.Params = append(st.Params, Param{Name: name, Value: val})
			}
		}
	}
	_ = sc.Err() // a truncated header just leaves fields empty

	const tail = 8 << 10
	if info, err := f.Stat(); err == nil && info.Size() > tail {
		_, _ = f.Seek(info.Size()-tail, io.SeekStart)
	} else {
		_, _ = f.Seek(0, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	footer := lines
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] == "" {
			footer = lines[i+1:]
			break
		}
	}
	for _, line := range footer {
		ts, key, val, ok := splitLogLine(line)
		if !ok {
			continue
		}
		switch key {
		case "status":
			st.Status = val
			st.FinishedAt = &ts
		case "duration":
			st.Duration = val
		case "exit_code":
			if code, err := strconv.Atoi(val); err == nil {
				st.ExitCode = &code
			}
		case "error":
			if msg, err := strconv.Unquote(val); err == nil {
				st.Error = msg
			} else {
				st.Error = val
			}
		}
	}
	return st
}

func splitLogLine(line string) (ts time.Time, key, val string, ok bool) {
	stamp, rest, found := strings.Cut(line, " ")
	if !found {
		return ts, "", "", false
	}
	ts, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return ts, "", "", false
	}
	key, val, found = strings.Cut(rest, "=")
	if !found || strings.ContainsAny(key, " :") {
		return ts, "", "", false
	}
	return ts, key, val, true
}
