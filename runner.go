package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
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

	TriggerWebhook  = "webhook"
	TriggerManual   = "manual"
	TriggerRollback = "rollback"

	ResultStarted = "started"
	ResultQueued  = "queued"

	latestLogName = "latest.log"
	stateFileName = "status.json"
	queueFileName = "queue.json"

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
	ErrQueueFull     = errors.New("queue is full")
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
	// Event and ResourceID describe non-git events, e.g. "order.updated" and "1234".
	Event      string `json:",omitempty"`
	ResourceID string `json:",omitempty"`
	// Payload is the request body, handed to the command as DEPLOY_PAYLOAD_FILE.
	Payload []byte `json:",omitempty"`
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
	Event      string      `json:"event,omitempty"`
	ResourceID string      `json:"resource_id,omitempty"`
	Params     []Param     `json:"params,omitempty"`
	ExitCode   *int        `json:"exit_code,omitempty"`
	Error      string      `json:"error,omitempty"`
	Log        string      `json:"log,omitempty"`
	Queued     *QueuedInfo `json:"queued,omitempty"`
	// NextRun is the next scheduled run (deploys with a schedule).
	NextRun *time.Time `json:"next_run,omitempty"`
}

// QueuedInfo is the push waiting for the running deploy to finish.
type QueuedInfo struct {
	Commit   string    `json:"commit,omitempty"`
	Delivery string    `json:"delivery,omitempty"`
	Pusher   string    `json:"pusher,omitempty"`
	Params   []Param   `json:"params,omitempty"`
	Since    time.Time `json:"since"`
	// Count is how many runs are waiting (queue_mode = "all" keeps them all).
	Count int `json:"count,omitempty"`
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
	lastFinished map[string]string        // status of the last completed run
	pending      map[string][]*pendingRun // per lane, oldest first
	nextRun      map[string]time.Time     // scheduled deploys
	reload       chan struct{}            // wakes the scheduler after a config change
	stopSched    chan struct{}
	schedDone    chan struct{}
	metrics      *metrics
	inflight     map[string]map[string]Trigger // queue_mode "all": running triggers per deploy, by log file
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
		pending:      map[string][]*pendingRun{},
		nextRun:      map[string]time.Time{},
		reload:       make(chan struct{}, 1),
		stopSched:    make(chan struct{}),
		schedDone:    make(chan struct{}),
		metrics:      newMetrics(),
		inflight:     map[string]map[string]Trigger{},
		seen:         newDeliveryCache(seenDeliveries),
	}
	r.SetConfig(cfg, notifier)
	r.mu.Lock()
	r.resumeLocked()
	r.mu.Unlock()
	go r.scheduler()
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
			r.loadQueue(cfg.Deploy[name])
		}
	}
	select {
	case r.reload <- struct{}{}:
	default:
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
	st := State{Deploy: name, Status: StatusNever}
	if s, ok := r.states[name]; ok {
		st = copyState(s)
	}
	if at, ok := r.nextRun[name]; ok {
		st.NextRun = &at
	}
	return st
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
		now := time.Now()
		run := &pendingRun{trigger: t, since: now}
		if d.queueAll {
			if len(r.pending[t.lane]) >= d.QueueMax {
				return SubmitResult{}, ErrQueueFull
			}
			r.pending[t.lane] = append(r.pending[t.lane], run)
		} else {
			if prev := r.pending[t.lane]; len(prev) > 0 {
				p := prev[0].trigger
				res.Replaced = firstNonEmpty(p.Commit, formatParams(p.Params), p.Delivery)
			}
			r.pending[t.lane] = []*pendingRun{run}
		}
		st := r.states[name]
		st.Queued = &QueuedInfo{Commit: t.Commit, Delivery: t.Delivery, Pusher: t.Pusher, Params: t.Params,
			Since: now.Truncate(time.Second), Count: r.pendingCountLocked(name)}
		r.persist(st)
		r.persistQueue(d)
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
		Event:      t.Event,
		ResourceID: t.ResourceID,
		Params:     t.Params,
		Log:        filepath.Base(path),
	}
	r.running[t.lane]++
	r.active[path] = true
	r.states[d.Name] = st
	r.persist(st)
	if d.queueAll {
		if r.inflight[d.Name] == nil {
			r.inflight[d.Name] = map[string]Trigger{}
		}
		r.inflight[d.Name][st.Log] = t
		r.persistQueue(d)
	}

	env := r.commandEnv(d, t)
	// The command's results ($DEPLOY_OUTPUT), for the email or "nimdeploy mail".
	output := runFile(path, "output")
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		log.Printf("deploy=%s cannot create the output file: %v", d.Name, err)
	} else {
		env = append(env, "DEPLOY_OUTPUT="+output)
	}
	if len(t.Payload) > 0 && d.payloadFile {
		payload := filepath.Join(filepath.Dir(path), "."+strings.TrimSuffix(st.Log, ".log")+".payload.json")
		if err := os.WriteFile(payload, t.Payload, 0o600); err != nil {
			log.Printf("deploy=%s cannot write payload file: %v", d.Name, err)
		} else {
			env = append(env, "DEPLOY_PAYLOAD_FILE="+payload)
		}
	}
	notifier := r.notifier
	retain := r.cfg.Logging.Retain
	cf := r.cfg.Cloudflare

	log.Printf("deploy=%s status=started trigger=%s delivery=%s commit=%s log=%s", d.Name, t.Source, t.Delivery, t.Commit, st.Log)
	r.wg.Add(1)
	go r.run(d, t, env, f, path, st, start, notifier, retain, cf)
	return st, nil
}

func (r *Runner) run(d *DeployConfig, t Trigger, env []string, f *os.File, path string, st *State, start time.Time, notifier *Notifier, retain int, cf CloudflareConfig) {
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
	if t.Event != "" {
		logf("event=%s", t.Event)
	}
	if t.ResourceID != "" {
		logf("resource_id=%s", t.ResourceID)
	}
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
		status, exitCode, errMsg = r.withHooks(d, env, f, logf, cf)
	}

	if line := r.sendDeployEmail(d, t, status, runFile(path, "output"), notifier); line != "" {
		fmt.Fprintln(f)
		logf("%s", line)
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

	_ = os.Remove(filepath.Join(filepath.Dir(path), "."+strings.TrimSuffix(st.Log, ".log")+".payload.json"))
	_ = os.Remove(runFile(path, "output"))

	r.mu.Lock()
	r.running[t.lane]--
	delete(r.active, path)
	if d.queueAll && errMsg != errShutdownCanceled {
		// Killed by a shutdown, it stays in queue.json and runs again on start.
		delete(r.inflight[d.Name], st.Log)
		r.persistQueue(d)
	}
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

	r.metrics.finished(d.Name, status, finished.Sub(cmdStart))
	notifier.Finished(d, t, final, recovered, path, superseded)

	if status == StatusFailed && d.RollbackOnFailure && t.Source != TriggerRollback && errMsg != errShutdownCanceled {
		r.autoRollback(d, t)
	}
}

// withHooks runs before, the command, the health check and the after hooks.
func (r *Runner) withHooks(d *DeployConfig, env []string, f *os.File, logf func(string, ...any), cf CloudflareConfig) (status string, exitCode *int, errMsg string) {
	hook := func(name, script string) (string, string) {
		fmt.Fprintln(f)
		logf("hook=%s", name)
		st, _, msg := r.runCmd(d, env, f, "/bin/bash", []string{"-eo", "pipefail", "-c", script})
		if st != StatusSuccess {
			logf("hook=%s failed: %s", name, msg)
		}
		fmt.Fprintln(f)
		return st, msg
	}
	if d.Before != "" {
		if st, msg := hook("before", d.Before); st != StatusSuccess {
			if d.AfterFailure != "" && msg != errShutdownCanceled {
				hook("after_failure", d.AfterFailure)
			}
			if msg == errShutdownCanceled {
				return StatusFailed, nil, msg
			}
			return StatusFailed, nil, "before hook: " + msg
		}
	}
	status, exitCode, errMsg = r.execute(d, env, f)
	if status == StatusSuccess && d.HealthURL != "" {
		if err := r.healthCheck(d, logf); err != nil {
			status, errMsg = StatusFailed, "health check: "+err.Error()
		}
	}
	if status == StatusSuccess {
		if d.CloudflareZoneID != "" {
			if err := purgeCloudflare(r.ctx, cf, d.CloudflareZoneID, d.CloudflarePurge); err != nil {
				logf("cloudflare: purge failed: %v", err) // the deploy itself worked
			} else {
				logf("cloudflare: purged %s", strings.Join(d.CloudflarePurge, ", "))
			}
		}
		if d.AfterSuccess != "" {
			hook("after_success", d.AfterSuccess) // logged; the deploy already succeeded
		}
	} else if d.AfterFailure != "" && errMsg != errShutdownCanceled {
		hook("after_failure", d.AfterFailure)
	}
	return status, exitCode, errMsg
}

// healthCheck waits for HealthURL to answer 2xx/3xx, up to HealthTimeout.
func (r *Runner) healthCheck(d *DeployConfig, logf func(string, ...any)) error {
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(d.HealthTimeout.Duration)
	last := ""
	for {
		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, d.HealthURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "nimdeploy-health/"+version)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode < 400 {
				logf("health: GET %s -> %d", d.HealthURL, resp.StatusCode)
				return nil
			}
			last = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) || r.ctx.Err() != nil {
			logf("health: GET %s -> %s after %s", d.HealthURL, last, d.HealthTimeout)
			return fmt.Errorf("%s did not answer 2xx/3xx in %s (%s)", d.HealthURL, d.HealthTimeout, last)
		}
		select {
		case <-r.ctx.Done():
		case <-time.After(min(2*time.Second, d.HealthTimeout.Duration)):
		}
	}
}

// RollbackTarget is the commit of the most recent successful deploy other
// than the latest run's: what was live before the latest change.
func (r *Runner) RollbackTarget(name string) (commit, fromLog string, err error) {
	h, err := r.History(name, 0)
	if err != nil {
		return "", "", err
	}
	return rollbackTargetIn(h)
}

// rollbackTargetIn picks the target from a history, newest first.
func rollbackTargetIn(h []State) (commit, fromLog string, err error) {
	if len(h) == 0 {
		return "", "", errors.New("no deploys in the history")
	}
	latest := h[0].Commit
	for _, st := range h {
		if st.Status == StatusSuccess && st.Commit != "" && st.Commit != latest {
			return st.Commit, st.Log, nil
		}
	}
	return "", "", fmt.Errorf("no earlier successful deploy with another commit in the kept logs (latest: %s)", dash(shortSHA(latest)))
}

// autoRollback redeploys the last good commit after a failed deploy.
func (r *Runner) autoRollback(d *DeployConfig, failed Trigger) {
	commit, from, err := r.RollbackTarget(d.Name)
	if err != nil {
		log.Printf("deploy=%s rollback_on_failure: nothing to roll back to: %v", d.Name, err)
		return
	}
	log.Printf("deploy=%s rollback_on_failure: redeploying %s (deployed by %s)", d.Name, shortSHA(commit), from)
	_, err = r.Submit(d.Name, Trigger{Source: TriggerRollback, Provider: d.Provider, Repository: d.Repository,
		Ref: "refs/heads/" + d.Branch, Branch: d.Branch, Commit: commit,
		Pusher: "auto, " + dash(shortSHA(failed.Commit)) + " failed"})
	if err != nil {
		log.Printf("deploy=%s rollback_on_failure: %v", d.Name, err)
	}
}

// execute runs the deploy command, writing its output to f.
func (r *Runner) execute(d *DeployConfig, env []string, f *os.File) (status string, exitCode *int, errMsg string) {
	return r.runCmd(d, env, f, d.Command, d.Args)
}

// runCmd runs a command (the deploy's or a hook) with the deploy's timeout,
// directory and environment, in its own process group.
func (r *Runner) runCmd(d *DeployConfig, env []string, f *os.File, command string, args []string) (status string, exitCode *int, errMsg string) {
	ctx, cancel := context.WithTimeout(r.ctx, d.Timeout.Duration)
	defer cancel()

	cmd := exec.CommandContext(ctx, command, args...)
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
		errMsg = errShutdownCanceled
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
	queue := r.pending[lane]
	if len(queue) == 0 || r.running[lane] > 0 {
		return
	}
	name, _, _ := strings.Cut(lane, laneSep)
	d, ok := r.cfg.Deploy[name]
	if r.closing && ok && d.queueAll {
		return // kept in queue.json, runs on the next start
	}
	p := queue[0]
	if len(queue) == 1 {
		delete(r.pending, lane)
	} else {
		r.pending[lane] = queue[1:]
	}
	if st := r.states[name]; st != nil && st.Queued != nil {
		if n := r.pendingCountLocked(name); n == 0 {
			st.Queued = nil
		} else {
			st.Queued.Count = n
		}
		r.persist(st)
	}
	what := firstNonEmpty(p.trigger.Commit, p.trigger.ResourceID, p.trigger.Delivery)
	if r.closing {
		log.Printf("deploy=%s queued run %s dropped: shutting down", name, what)
		return
	}
	if !ok {
		log.Printf("deploy=%s queued run %s dropped: deploy removed from config", name, what)
		return
	}
	if _, err := r.startLocked(d, p.trigger); err != nil {
		log.Printf("deploy=%s cannot start queued run %s: %v", name, what, err)
		r.persistQueue(d)
	}
}

// notifyRejected tells the notification channel about a request that could
// not run (WooCommerce: an order that will not be retried by the shop).
func (r *Runner) notifyRejected(d *DeployConfig, t Trigger, reason string) {
	r.mu.Lock()
	n := r.notifier
	r.mu.Unlock()
	go n.Rejected(d, t, reason)
}

// pendingCountLocked counts the queued runs of every lane of a deploy.
func (r *Runner) pendingCountLocked(name string) int {
	n := 0
	for lane, q := range r.pending {
		if lane == name || strings.HasPrefix(lane, name+laneSep) {
			n += len(q)
		}
	}
	return n
}

// queueFile is what queue_mode = "all" keeps on disk so nothing is lost on
// a restart: runs that were interrupted, then the ones waiting.
type queueFile struct {
	Interrupted []Trigger `json:"interrupted,omitempty"`
	Pending     []Trigger `json:"pending,omitempty"`
}

// persistQueue writes the deploy's queue.json (0600: payloads may hold
// personal data). Callers hold r.mu.
func (r *Runner) persistQueue(d *DeployConfig) {
	if !d.queueAll {
		return
	}
	var q queueFile
	logs := sortedKeys(r.inflight[d.Name])
	for _, l := range logs {
		q.Interrupted = append(q.Interrupted, r.inflight[d.Name][l])
	}
	var runs []*pendingRun
	for lane, p := range r.pending {
		if lane == d.Name || strings.HasPrefix(lane, d.Name+laneSep) {
			runs = append(runs, p...)
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].since.Before(runs[j].since) })
	for _, p := range runs {
		q.Pending = append(q.Pending, p.trigger)
	}
	file := filepath.Join(r.dir, d.Name, queueFileName)
	if len(q.Interrupted) == 0 && len(q.Pending) == 0 {
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("deploy=%s cannot remove %s: %v", d.Name, queueFileName, err)
		}
		return
	}
	b, err := json.Marshal(q)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(file), 0o750); err == nil {
			tmp := file + ".tmp"
			if err = os.WriteFile(tmp, b, 0o600); err == nil {
				err = os.Rename(tmp, file)
			}
		}
	}
	if err != nil {
		log.Printf("deploy=%s cannot persist %s: %v", d.Name, queueFileName, err)
	}
}

// loadQueue restores queue.json: interrupted runs go first, so they run again.
func (r *Runner) loadQueue(d *DeployConfig) {
	b, err := os.ReadFile(filepath.Join(r.dir, d.Name, queueFileName))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("deploy=%s cannot read %s: %v", d.Name, queueFileName, err)
		}
		return
	}
	var q queueFile
	if err := json.Unmarshal(b, &q); err != nil {
		log.Printf("deploy=%s cannot parse %s: %v", d.Name, queueFileName, err)
		return
	}
	if !d.queueAll {
		log.Printf("deploy=%s %s ignored: queue_mode is not \"all\" anymore (%d runs not resumed)", d.Name, queueFileName, len(q.Interrupted)+len(q.Pending))
		return
	}
	now := time.Now()
	for i, t := range append(q.Interrupted, q.Pending...) {
		if i < len(q.Interrupted) {
			log.Printf("deploy=%s run %s was interrupted, running it again", d.Name, firstNonEmpty(t.ResourceID, t.Commit, t.Delivery))
		}
		t.lane = laneOf(d, t.Params)
		// Keep the order: a nanosecond apart.
		r.pending[t.lane] = append(r.pending[t.lane], &pendingRun{trigger: t, since: now.Add(time.Duration(i))})
		r.seen.add(t.Delivery)
	}
	if n := r.pendingCountLocked(d.Name); n > 0 {
		log.Printf("deploy=%s resuming %d queued runs", d.Name, n)
	}
}

// resumeLocked starts the first queued run of every idle lane.
func (r *Runner) resumeLocked() {
	for _, lane := range sortedKeys(r.pending) {
		r.startPendingLocked(lane)
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
	if r.cfg.path != "" {
		// Lets scripts call nimdeploy (e.g. "nimdeploy woocommerce note").
		env = append(env, "NIMDEPLOY_CONFIG="+r.cfg.path)
	}
	if exe, err := os.Executable(); err == nil {
		env = append(env, "NIMDEPLOY="+exe)
	}
	return append(env,
		"DEPLOY_EVENT="+t.Event,
		"DEPLOY_RESOURCE_ID="+t.ResourceID,
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
	alreadyClosing := r.closing
	r.closing = true
	r.mu.Unlock()
	if !alreadyClosing {
		close(r.stopSched)
		<-r.schedDone
	}

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

const errShutdownCanceled = "canceled: service shutting down"

// runFile is a private per-run file next to the log: .<log name>.<kind>
func runFile(logPath, kind string) string {
	return filepath.Join(filepath.Dir(logPath), "."+strings.TrimSuffix(filepath.Base(logPath), ".log")+"."+kind)
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

// pruneLogs keeps the newest retain log files in dir, in the same order as
// the history (see logsNewestFirst).
func pruneLogs(dir string, retain int, active map[string]bool) error {
	if retain <= 0 {
		return nil
	}
	logs, err := logsNewestFirst(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range logs[min(retain, len(logs)):] {
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
