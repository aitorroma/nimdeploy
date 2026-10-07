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
	logs, err := logsNewestFirst(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []State{}, nil
	}
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(logs) > limit {
		logs = logs[:limit]
	}

	r.mu.Lock()
	var labels map[string]string
	if d, ok := r.cfg.Deploy[name]; ok {
		labels = d.labels
	}
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
		st.Labels = copyLabels(labels) // labels live in the config, not in the logs
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
		case "event":
			st.Event = val
		case "resource_id":
			st.ResourceID = val
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

// logsNewestFirst lists a deploy's log files, newest first. Names start
// with the start time to the second; runs started within the same second
// are ordered by when their log was last written.
func logsNewestFirst(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type logFile struct {
		name string
		mod  time.Time
	}
	var files []logFile
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(n, ".log") && !strings.HasPrefix(n, ".") {
			lf := logFile{name: n}
			if info, err := e.Info(); err == nil {
				lf.mod = info.ModTime()
			}
			files = append(files, lf)
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		si, sj := logStamp(files[i].name), logStamp(files[j].name)
		if si != sj {
			return si > sj
		}
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}
		return files[i].name > files[j].name
	})
	logs := make([]string, len(files))
	for i, f := range files {
		logs[i] = f.name
	}
	return logs, nil
}

// logStamp is the "20060102-150405" start time a log name begins with.
func logStamp(name string) string {
	if len(name) >= 15 {
		return name[:15]
	}
	return name
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
