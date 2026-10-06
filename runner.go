package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	StatusNever       = "never"
	StatusRunning     = "running"
	StatusSuccess     = "success"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
	StatusWaiting     = "waiting" // waiting for CI
	StatusSkipped     = "skipped" // not deployed: CI failed or a newer push replaced it

	TriggerWebhook = "webhook"
	TriggerManual  = "manual"

	ResultStarted = "started"
	ResultQueued  = "queued"

	latestLogName = "latest.log"
	stateFileName = "status.json"

	// killGrace is how long a timed-out deploy gets between SIGTERM and SIGKILL.
	killGrace = 10 * time.Second
	// seenDeliveries is how many delivery IDs are remembered to drop duplicates.
	seenDeliveries = 1000
)

var (
	ErrBusy          = errors.New("deploy already running")
	ErrShuttingDown  = errors.New("server is shutting down")
	ErrDuplicate     = errors.New("delivery already processed")
	ErrUnknownDeploy = errors.New("unknown deploy")
)

// Trigger describes what started a deploy.
type Trigger struct {
	Source     string
	Provider   string
	Delivery   string
	Repository string
	Ref        string
	Branch     string
	Commit     string
	Pusher     string
	// Params are the values captured from the webhook (or given to a manual run).
	Params []Param
	// lane is what the lock and queue apply to: the deploy, or the deploy plus
	// the value of its queue_key param.
	lane string
}

// State is the last known state of a deploy, exposed on /status and
// persisted to <logdir>/<deploy>/status.json.
type State struct {
	Deploy     string      `json:"deploy"`
	Status     string      `json:"status"`
	Trigger    string      `json:"trigger,omitempty"`
	Provider   string      `json:"provider,omitempty"`
	StartedAt  *time.Time  `json:"started_at,omitempty"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
	Duration   string      `json:"duration,omitempty"`
	Delivery   string      `json:"delivery,omitempty"`
	Repository string      `json:"repository,omitempty"`
	Branch     string      `json:"branch,omitempty"`
	Commit     string      `json:"commit,omitempty"`
	Pusher     string      `json:"pusher,omitempty"`
	Params     []Param     `json:"params,omitempty"`
	ExitCode   *int        `json:"exit_code,omitempty"`
	Error      string      `json:"error,omitempty"`
	Log        string      `json:"log,omitempty"`
	Queued     *QueuedInfo `json:"queued,omitempty"`
}

// QueuedInfo is the push waiting for the running deploy to finish.
type QueuedInfo struct {
	Commit   string    `json:"commit,omitempty"`
	Delivery string    `json:"delivery,omitempty"`
	Pusher   string    `json:"pusher,omitempty"`
	Params   []Param   `json:"params,omitempty"`
	Since    time.Time `json:"since"`
}

// SubmitResult is returned to whoever asked for a deploy.
type SubmitResult struct {
	Result string `json:"result"`
	// Replaced is the commit of a queued push superseded by this one.
	Replaced string `json:"replaced,omitempty"`
	State    State  `json:"state"`
}

type pendingRun struct {
	trigger Trigger
	since   time.Time
}

type Runner struct {
	dir string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu           sync.Mutex
	cfg          *Config
	notifier     *Notifier
	secretEnvs   map[string]bool
	closing      bool
	running      map[string]int  // per lane (see Trigger.lane)
	active       map[string]bool // log files currently being written
	states       map[string]*State
	lastFinished map[string]string      // status of the last completed run
	pending      map[string]*pendingRun // per lane
	seen         *deliveryCache
}

func NewRunner(cfg *Config, notifier *Notifier) (*Runner, error) {
	if err := os.MkdirAll(cfg.Logging.Directory, 0o750); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		dir:          cfg.Logging.Directory,
		ctx:          ctx,
		cancel:       cancel,
		running:      map[string]int{},
		active:       map[string]bool{},
		states:       map[string]*State{},
		lastFinished: map[string]string{},
		pending:      map[string]*pendingRun{},
		seen:         newDeliveryCache(seenDeliveries),
	}
	r.SetConfig(cfg, notifier)
	return r, nil
}

// SetConfig applies a (re)loaded config. Running deploys keep the settings
// they started with.
func (r *Runner) SetConfig(cfg *Config, notifier *Notifier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	r.notifier = notifier
	r.secretEnvs = map[string]bool{}
	for _, name := range cfg.secretEnvNames() {
		if name != "" {
			r.secretEnvs[name] = true
		}
	}
	for _, name := range cfg.DeployNames() {
		if _, ok := r.states[name]; !ok {
			r.loadState(name)
		}
	}
}

func (r *Runner) loadState(name string) {
	b, err := os.ReadFile(filepath.Join(r.dir, name, stateFileName))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("deploy=%s cannot read state: %v", name, err)
		}
		return
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		log.Printf("deploy=%s cannot parse state: %v", name, err)
		return
	}
	if st.Status == StatusRunning || st.Status == StatusWaiting {
		st.Status = StatusInterrupted
		st.Error = "service stopped while deploy was running"
	}
	if st.Queued != nil {
		log.Printf("deploy=%s queued push for commit %s was lost on restart", name, st.Queued.Commit)
		st.Queued = nil
	}
	st.Deploy = name
	r.states[name] = &st
	r.lastFinished[name] = st.Status
}

// State returns a copy of the deploy's last known state.
func (r *Runner) State(name string) State {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.states[name]; ok {
		return copyState(st)
	}
	return State{Deploy: name, Status: StatusNever}
}

// Submit starts a deploy, or queues it behind the running one. Queued pushes
// are coalesced: only the newest one runs.
func (r *Runner) Submit(name string, t Trigger) (SubmitResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closing {
		return SubmitResult{}, ErrShuttingDown
	}
	d, ok := r.cfg.Deploy[name]
	if !ok {
		return SubmitResult{}, ErrUnknownDeploy
	}
	if r.seen.has(t.Delivery) {
		return SubmitResult{}, ErrDuplicate
	}
	t.lane = laneOf(d, t.Params)

	if d.lock && r.running[t.lane] > 0 {
		if !d.queue {
			return SubmitResult{}, ErrBusy
		}
		res := SubmitResult{Result: ResultQueued}
		if prev := r.pending[t.lane]; prev != nil {
			res.Replaced = firstNonEmpty(prev.trigger.Commit, formatParams(prev.trigger.Params), prev.trigger.Delivery)
		}
		now := time.Now()
		r.pending[t.lane] = &pendingRun{trigger: t, since: now}
		st := r.states[name]
		st.Queued = &QueuedInfo{Commit: t.Commit, Delivery: t.Delivery, Pusher: t.Pusher, Params: t.Params, Since: now.Truncate(time.Second)}
		r.persist(st)
		r.seen.add(t.Delivery)
		res.State = copyState(st)
		return res, nil
	}

	st, err := r.startLocked(d, t)
	if err != nil {
		return SubmitResult{}, err
	}
	r.seen.add(t.Delivery)
	return SubmitResult{Result: ResultStarted, State: copyState(st)}, nil
}

// startLocked creates the log and launches the command. Callers hold r.mu.
func (r *Runner) startLocked(d *DeployConfig, t Trigger) (*State, error) {
	start := time.Now()
	f, path, err := createDeployLog(r.dir, d.Name, t.Delivery, start)
	if err != nil {
		return nil, fmt.Errorf("create log: %w", err)
	}
	if err := updateLatest(filepath.Dir(path), filepath.Base(path)); err != nil {
		log.Printf("deploy=%s cannot update %s: %v", d.Name, latestLogName, err)
	}

	startedAt := start.Truncate(time.Second)
	st := &State{
		Deploy:     d.Name,
		Status:     initialStatus(d, t),
		Trigger:    t.Source,
		Provider:   t.Provider,
		StartedAt:  &startedAt,
		Delivery:   t.Delivery,
		Repository: t.Repository,
		Branch:     t.Branch,
		Commit:     t.Commit,
		Pusher:     t.Pusher,
		Params:     t.Params,
		Log:        filepath.Base(path),
	}
	r.running[t.lane]++
	r.active[path] = true
	r.states[d.Name] = st
	r.persist(st)

	env := r.commandEnv(d, t)
	notifier := r.notifier
	retain := r.cfg.Logging.Retain

	log.Printf("deploy=%s status=started trigger=%s delivery=%s commit=%s log=%s", d.Name, t.Source, t.Delivery, t.Commit, st.Log)
	r.wg.Add(1)
	go r.run(d, t, env, f, path, st, start, notifier, retain)
	return st, nil
}

func (r *Runner) run(d *DeployConfig, t Trigger, env []string, f *os.File, path string, st *State, start time.Time, notifier *Notifier, retain int) {
	defer r.wg.Done()

	logf := func(format string, args ...any) {
		fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	}

	logf("deploy=%s status=started", d.Name)
	logf("trigger=%s", t.Source)
	logf("provider=%s", t.Provider)
	logf("repository=%s", t.Repository)
	logf("branch=%s", t.Branch)
	logf("commit=%s", t.Commit)
	logf("pusher=%s", t.Pusher)
	logf("delivery=%s", t.Delivery)
	for _, p := range t.Params {
		logf("param.%s=%s", p.Name, p.Value)
	}
	logf("command=%s", strings.Join(append([]string{d.Command}, d.Args...), " "))
	if d.WorkingDirectory != "" {
		logf("working_directory=%s", d.WorkingDirectory)
	}
	logf("timeout=%s", d.Timeout)
	if !d.logOutput {
		logf("log_output=false (command output discarded)")
	}
	fmt.Fprintln(f)

	status, errMsg := StatusSuccess, ""
	var exitCode *int
	superseded := false
	cmdStart := start
	proceed := true
	if needsCI(d, t) {
		logf("ci: waiting for %s (timeout %s)", strings.Join(d.WaitForCI, ", "), d.CITimeout)
		ok, reason, sup := r.waitForCI(d, t, notifier, logf)
		if ok {
			logf("ci: passed after %s, deploying", formatDuration(time.Since(start)))
			fmt.Fprintln(f)
			r.setStatus(d.Name, st, StatusRunning)
			cmdStart = time.Now()
		} else {
			logf("ci: not deploying: %s", reason)
			proceed, status, errMsg, superseded = false, StatusSkipped, reason, sup
		}
	}
	if proceed {
		notifier.CommitStatus(d, t, "pending", "Deploying")
		status, exitCode, errMsg = r.execute(d, env, f)
	}

	finished := time.Now()
	duration := formatDuration(finished.Sub(cmdStart))

	fmt.Fprintln(f)
	logf("status=%s", status)
	logf("duration=%s", duration)
	if exitCode != nil {
		logf("exit_code=%d", *exitCode)
	}
	if errMsg != "" {
		logf("error=%q", errMsg)
	}
	if cerr := f.Close(); cerr != nil {
		log.Printf("deploy=%s cannot close log: %v", d.Name, cerr)
	}

	if errMsg != "" {
		log.Printf("deploy=%s status=%s duration=%s error=%q log=%s", d.Name, status, duration, errMsg, path)
	} else {
		log.Printf("deploy=%s status=%s duration=%s log=%s", d.Name, status, duration, path)
	}

	r.mu.Lock()
	r.running[t.lane]--
	delete(r.active, path)
	finishedAt := finished.Truncate(time.Second)
	st.Status = status
	st.FinishedAt = &finishedAt
	st.Duration = duration
	st.ExitCode = exitCode
	st.Error = errMsg
	recovered := false
	if status != StatusSkipped {
		// A skipped deploy changed nothing, so it neither breaks nor fixes.
		prev := r.lastFinished[d.Name]
		r.lastFinished[d.Name] = status
		recovered = status == StatusSuccess && (prev == StatusFailed || prev == StatusInterrupted)
	}
	if r.states[d.Name] == st {
		// With lock = false a newer run may have replaced this one; don't clobber it.
		r.persist(st)
	}
	if err := pruneLogs(filepath.Dir(path), retain, r.active); err != nil {
		log.Printf("deploy=%s cannot prune logs: %v", d.Name, err)
	}
	r.startPendingLocked(t.lane)
	final := copyState(st)
	final.Queued = nil
	r.mu.Unlock()

	notifier.Finished(d, t, final, recovered, path, superseded)
}

// execute runs the deploy command, writing its output to f.
func (r *Runner) execute(d *DeployConfig, env []string, f *os.File) (status string, exitCode *int, errMsg string) {
	ctx, cancel := context.WithTimeout(r.ctx, d.Timeout.Duration)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.Command, d.Args...)
	cmd.Dir = d.WorkingDirectory
	cmd.Env = env
	if d.logOutput {
		// The child writes straight to the file: nothing is buffered in memory.
		cmd.Stdout = f
		cmd.Stderr = f
	}
	// Run in its own process group so a timeout kills the whole tree
	// (npm, git, etc.), not just the top-level script.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = killGrace

	err := cmd.Run()
	if ctx.Err() != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	code := 0
	if err == nil {
		return StatusSuccess, &code, ""
	}
	code = -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	switch {
	case r.ctx.Err() != nil:
		errMsg = "canceled: service shutting down"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		errMsg = fmt.Sprintf("timeout after %s", d.Timeout)
	default:
		errMsg = err.Error()
	}
	return StatusFailed, &code, errMsg
}

// setStatus updates a running deploy's status and persists it.
func (r *Runner) setStatus(name string, st *State, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st.Status = status
	if r.states[name] == st {
		r.persist(st)
	}
}

// startPendingLocked runs the lane's queued push, if any. Callers hold r.mu.
func (r *Runner) startPendingLocked(lane string) {
	p := r.pending[lane]
	if p == nil || r.running[lane] > 0 {
		return
	}
	delete(r.pending, lane)
	name, _, _ := strings.Cut(lane, laneSep)
	if st := r.states[name]; st != nil && st.Queued != nil && !r.hasPendingLocked(name) {
		st.Queued = nil
		r.persist(st)
	}
	if r.closing {
		log.Printf("deploy=%s queued commit %s dropped: shutting down", name, p.trigger.Commit)
		return
	}
	d, ok := r.cfg.Deploy[name]
	if !ok {
		log.Printf("deploy=%s queued commit %s dropped: deploy removed from config", name, p.trigger.Commit)
		return
	}
	if _, err := r.startLocked(d, p.trigger); err != nil {
		log.Printf("deploy=%s cannot start queued commit %s: %v", name, p.trigger.Commit, err)
	}
}

func (r *Runner) commandEnv(d *DeployConfig, t Trigger) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if r.secretEnvs[key] {
			continue // never leak secrets to deploy scripts
		}
		env = append(env, kv)
	}
	env = append(env, d.Env...)
	for _, p := range t.Params {
		env = append(env, p.Name+"="+p.Value)
	}
	return append(env,
		"DEPLOY_NAME="+d.Name,
		"DEPLOY_TRIGGER="+t.Source,
		"DEPLOY_PROVIDER="+t.Provider,
		"DEPLOY_REPOSITORY="+t.Repository,
		"DEPLOY_REF="+t.Ref,
		"DEPLOY_BRANCH="+t.Branch,
		"DEPLOY_COMMIT="+t.Commit,
		"DEPLOY_PUSHER="+t.Pusher,
		"DEPLOY_DELIVERY="+t.Delivery,
	)
}

// persist writes the state atomically. Callers must hold r.mu.
func (r *Runner) persist(st *State) {
	dir := filepath.Join(r.dir, st.Deploy)
	b, err := json.MarshalIndent(st, "", "  ")
	if err == nil {
		tmp := filepath.Join(dir, "."+stateFileName+".tmp")
		if err = os.WriteFile(tmp, append(b, '\n'), 0o640); err == nil {
			err = os.Rename(tmp, filepath.Join(dir, stateFileName))
		}
	}
	if err != nil {
		log.Printf("deploy=%s cannot persist state: %v", st.Deploy, err)
	}
}

// Shutdown stops accepting deploys, waits up to grace for running ones and
// then kills whatever is left.
func (r *Runner) Shutdown(grace time.Duration) {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()

	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		log.Printf("deploys still running after %s, killing them", grace)
		r.cancel()
		<-done
	}
	r.cancel()
}

// laneSep separates the deploy name from the queue_key value in a lane.
const laneSep = "\x00"

// laneOf is what the lock and the queue apply to: the whole deploy, or with
// queue_key one lane per value ("restart api" doesn't replace "restart web").
func laneOf(d *DeployConfig, params []Param) string {
	if d.QueueKey == "" {
		return d.Name
	}
	return d.Name + laneSep + paramValue(params, d.QueueKey)
}

// hasPendingLocked reports whether any lane of the deploy has a queued run.
func (r *Runner) hasPendingLocked(name string) bool {
	for lane := range r.pending {
		if lane == name || strings.HasPrefix(lane, name+laneSep) {
			return true
		}
	}
	return false
}

func initialStatus(d *DeployConfig, t Trigger) string {
	if needsCI(d, t) {
		return StatusWaiting
	}
	return StatusRunning
}

// isActive reports whether a status means the deploy hasn't finished.
func isActive(status string) bool { return status == StatusRunning || status == StatusWaiting }

func copyState(st *State) State {
	c := *st
	if st.Queued != nil {
		q := *st.Queued
		q.Params = append([]Param(nil), st.Queued.Params...)
		c.Queued = &q
	}
	c.Params = append([]Param(nil), st.Params...)
	return c
}

// deliveryCache remembers the last N delivery IDs.
type deliveryCache struct {
	ids  map[string]bool
	ring []string
	next int
}

func newDeliveryCache(size int) *deliveryCache {
	return &deliveryCache{ids: map[string]bool{}, ring: make([]string, size)}
}

func (c *deliveryCache) has(id string) bool { return id != "" && c.ids[id] }

func (c *deliveryCache) add(id string) {
	if id == "" || c.ids[id] {
		return
	}
	delete(c.ids, c.ring[c.next])
	c.ring[c.next] = id
	c.ids[id] = true
	c.next = (c.next + 1) % len(c.ring)
}

func createDeployLog(baseDir, name, delivery string, now time.Time) (*os.File, string, error) {
	dir := filepath.Join(baseDir, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, "", err
	}
	base := now.Format("20060102-150405") + "-" + shortID(delivery)
	for i := 1; i <= 100; i++ {
		filename := base + ".log"
		if i > 1 {
			filename = fmt.Sprintf("%s-%d.log", base, i)
		}
		path := filepath.Join(dir, filename)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o640)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, path, err
	}
	return nil, "", fmt.Errorf("too many log files named %s*", base)
}

// shortID returns the first 6 alphanumeric chars of the delivery ID, or a
// random one when there is none (manual deploys).
func shortID(delivery string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(delivery) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			if b.Len() == 6 {
				break
			}
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	buf := make([]byte, 3)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// updateLatest atomically points <dir>/latest.log at filename.
func updateLatest(dir, filename string) error {
	tmp := filepath.Join(dir, "."+latestLogName+".tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(filename, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, latestLogName))
}

// pruneLogs keeps the newest retain log files in dir. Names start with a
// timestamp, so lexical order is chronological.
func pruneLogs(dir string, retain int, active map[string]bool) error {
	if retain <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var logs []string
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(name, ".log") && !strings.HasPrefix(name, ".") {
			logs = append(logs, name)
		}
	}
	sort.Strings(logs)
	var errs []error
	for _, name := range logs[:max(0, len(logs)-retain)] {
		path := filepath.Join(dir, name)
		if active[path] {
			continue
		}
		if err := os.Remove(path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}
