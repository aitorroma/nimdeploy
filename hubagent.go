package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The hub agent sends what happens on this server to a central nimdeploy
// running as a hub ("nimdeploy hub serve"): deploy events and a periodic
// heartbeat with the inventory of deploys and their labels. It only makes
// outgoing HTTPS requests, signed with this agent's token; the hub never
// connects to the server. Events wait in an on-disk outbox while the hub is
// unreachable.

const (
	hubEventsPath       = "/api/v1/events"
	defaultHeartbeat    = time.Minute
	defaultHubLogTail   = 20
	hubOutboxDir        = ".hub-outbox"
	hubOutboxMax        = 5000
	hubBatchMax         = 100
	hubMaxBackoff       = 5 * time.Minute
	hubSignatureHeader  = "X-Nimdeploy-Signature"
	hubTimestampHeader  = "X-Nimdeploy-Timestamp"
	hubAgentHeader      = "X-Nimdeploy-Agent"
	hubMaxClockSkew     = 5 * time.Minute
	hubEventDeployStart = "deploy.started"
	hubEventDeployDone  = "deploy.finished"
	hubEventRejected    = "deploy.rejected"
	hubEventHeartbeat   = "heartbeat"
)

var agentNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type HubConfig struct {
	URL         string   `toml:"url"`
	Agent       string   `toml:"agent"` // this server's name in the hub (default: hostname)
	TokenEnv    string   `toml:"token_env"`
	SendLogTail *int     `toml:"send_log_tail"` // log lines sent with failures (0 = none)
	Heartbeat   Duration `toml:"heartbeat"`

	token   string
	logTail int
}

func (h *HubConfig) validate() error {
	if h.URL == "" {
		return nil
	}
	u, err := url.Parse(h.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
		return errors.New("hub.url must be https://...")
	}
	h.URL = strings.TrimRight(h.URL, "/")
	if h.Agent == "" {
		host, _ := os.Hostname()
		h.Agent = strings.Split(host, ".")[0]
	}
	if !agentNameRe.MatchString(h.Agent) {
		return fmt.Errorf("hub.agent %q: letters, digits, _ . - (max 64)", h.Agent)
	}
	if h.TokenEnv == "" {
		return errors.New("hub.token_env is required (the agent's token, from \"nimdeploy hub agent add\")")
	}
	h.logTail = defaultHubLogTail
	if h.SendLogTail != nil {
		h.logTail = *h.SendLogTail
	}
	if h.logTail < 0 || h.logTail > 500 {
		return errors.New("hub.send_log_tail must be 0-500")
	}
	if h.Heartbeat.Duration == 0 {
		h.Heartbeat.Duration = defaultHeartbeat
	}
	if h.Heartbeat.Duration < 10*time.Second {
		return errors.New("hub.heartbeat must be at least 10s")
	}
	return nil
}

// hubEvent is what agents send and the hub stores.
type hubEvent struct {
	ID        string        `json:"id"`
	Type      string        `json:"type"`
	Time      time.Time     `json:"time"`
	Agent     string        `json:"agent"`
	Host      string        `json:"host,omitempty"`
	Version   string        `json:"version,omitempty"`
	Deploy    string        `json:"deploy,omitempty"`
	State     *State        `json:"state,omitempty"`
	LogTail   []string      `json:"log_tail,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	Inventory *hubInventory `json:"inventory,omitempty"`
}

type hubInventory struct {
	Started time.Time         `json:"started"`
	Labels  map[string]string `json:"labels,omitempty"`
	Deploys []hubDeploy       `json:"deploys"`
	Outbox  int               `json:"outbox"` // events waiting to be sent
	// HeartbeatS is the agent's heartbeat interval, so the hub knows when it's late.
	HeartbeatS int64 `json:"heartbeat_s"`
}

type hubDeploy struct {
	Name       string            `json:"name"`
	Provider   string            `json:"provider,omitempty"`
	Repository string            `json:"repository,omitempty"`
	Branch     string            `json:"branch,omitempty"`
	Schedule   string            `json:"schedule,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	State      State             `json:"state"`
	Running    int               `json:"running"`
	Queued     int               `json:"queued"`
}

type hubBatch struct {
	Events []hubEvent `json:"events"`
}

// signHub is the signature both sides compute: HMAC-SHA256 of "<ts>.<body>".
func signHub(token []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, token)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type hubAgent struct {
	runner  *Runner
	dir     string
	host    string
	started time.Time
	client  *http.Client
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}

	mu      sync.Mutex
	cfg     HubConfig
	failing bool
}

func newHubAgent(r *Runner, cfg HubConfig, logDir string) *hubAgent {
	host, _ := os.Hostname()
	a := &hubAgent{
		runner: r, dir: filepath.Join(logDir, hubOutboxDir), host: host, started: time.Now(),
		client: &http.Client{Timeout: 20 * time.Second},
		wake:   make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		cfg: cfg,
	}
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		log.Printf("hub: cannot create outbox %s: %v", a.dir, err)
	}
	go a.loop()
	return a
}

func (a *hubAgent) setConfig(cfg HubConfig) {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
}

func (a *hubAgent) config() HubConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// emit queues an event on disk and wakes the sender. It never blocks a deploy.
func (a *hubAgent) emit(ev hubEvent) {
	if a == nil {
		return
	}
	cfg := a.config()
	ev.ID = newDeliveryID()
	ev.Time = time.Now().UTC()
	ev.Agent, ev.Host, ev.Version = cfg.Agent, a.host, version
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	name := fmt.Sprintf("%020d-%s.json", ev.Time.UnixNano(), ev.ID[:8])
	tmp := filepath.Join(a.dir, "."+name)
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		err = os.Rename(tmp, filepath.Join(a.dir, name))
	}
	if err != nil {
		log.Printf("hub: cannot queue %s event: %v", ev.Type, err)
		return
	}
	a.trimOutbox()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// trimOutbox drops the oldest events beyond hubOutboxMax (hub down for long).
func (a *hubAgent) trimOutbox() {
	files := a.outbox()
	if len(files) <= hubOutboxMax {
		return
	}
	for _, f := range files[:len(files)-hubOutboxMax] {
		_ = os.Remove(filepath.Join(a.dir, f))
	}
	log.Printf("hub: outbox over %d events, dropped the %d oldest", hubOutboxMax, len(files)-hubOutboxMax)
}

func (a *hubAgent) outbox() []string {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			files = append(files, n)
		}
	}
	sort.Strings(files)
	return files
}

func (a *hubAgent) loop() {
	defer close(a.done)
	backoff := 2 * time.Second
	heartbeat := time.NewTimer(2 * time.Second) // first one soon after start
	defer heartbeat.Stop()
	for {
		sent, err := a.flush()
		a.report(err)
		wait := 2 * time.Second
		if err != nil {
			wait = backoff
			backoff = min(backoff*2, hubMaxBackoff)
		} else {
			backoff = 2 * time.Second
			if sent > 0 {
				continue // more may be waiting
			}
			wait = time.Hour
		}
		select {
		case <-a.stop:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = a.flushCtx(ctx)
			cancel()
			return
		case <-a.wake:
		case <-heartbeat.C:
			a.sendHeartbeat()
			heartbeat.Reset(a.config().Heartbeat.Duration)
		case <-time.After(wait):
		}
	}
}

func (a *hubAgent) report(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case err != nil && !a.failing:
		a.failing = true
		log.Printf("hub: cannot reach %s, events wait in %s: %v", a.cfg.URL, a.dir, err)
	case err == nil && a.failing:
		a.failing = false
		log.Printf("hub: %s reachable again, outbox sent", a.cfg.URL)
	}
}

func (a *hubAgent) flush() (int, error) { return a.flushCtx(context.Background()) }

// flushCtx sends the outbox in batches, oldest first, deleting what the hub accepted.
func (a *hubAgent) flushCtx(ctx context.Context) (int, error) {
	files := a.outbox()
	if len(files) == 0 {
		return 0, nil
	}
	if len(files) > hubBatchMax {
		files = files[:hubBatchMax]
	}
	var batch hubBatch
	var kept []string
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(a.dir, f))
		var ev hubEvent
		if err == nil {
			err = json.Unmarshal(b, &ev)
		}
		if err != nil {
			_ = os.Remove(filepath.Join(a.dir, f)) // unreadable: drop it
			continue
		}
		batch.Events = append(batch.Events, ev)
		kept = append(kept, f)
	}
	if len(batch.Events) == 0 {
		return 0, nil
	}
	if err := a.post(ctx, batch); err != nil {
		var bad hubRefused
		if !errors.As(err, &bad) {
			return 0, err
		}
		// Retrying won't help: drop them instead of blocking the outbox forever.
		log.Printf("hub: dropped %d events the hub refused: %v", len(kept), err)
	}
	for _, f := range kept {
		_ = os.Remove(filepath.Join(a.dir, f))
	}
	return len(kept), nil
}

func (a *hubAgent) post(ctx context.Context, batch hubBatch) error {
	cfg := a.config()
	body, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL+hubEventsPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nimdeploy/"+version)
	req.Header.Set(hubAgentHeader, cfg.Agent)
	req.Header.Set(hubTimestampHeader, ts)
	req.Header.Set(hubSignatureHeader, signHub([]byte(cfg.token), ts, body))
	resp, err := a.client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return uerr.Err
		}
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	if resp.StatusCode/100 != 2 {
		err := fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge {
			return hubRefused{err}
		}
		return err
	}
	return nil
}

// hubRefused is a batch the hub will never accept (malformed, too large).
type hubRefused struct{ error }

// sendHeartbeat sends the inventory directly (it is not worth queueing:
// the next one replaces it).
func (a *hubAgent) sendHeartbeat() {
	inv := a.runner.hubInventory()
	inv.Started = a.started
	inv.HeartbeatS = int64(a.config().Heartbeat.Seconds())
	inv.Outbox = len(a.outbox())
	cfg := a.config()
	ev := hubEvent{ID: newDeliveryID(), Type: hubEventHeartbeat, Time: time.Now().UTC(), Agent: cfg.Agent,
		Host: a.host, Version: version, Inventory: &inv}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a.report(a.post(ctx, hubBatch{Events: []hubEvent{ev}}))
}

func (a *hubAgent) shutdown() {
	if a == nil {
		return
	}
	select {
	case <-a.stop:
	default:
		close(a.stop)
	}
	<-a.done
}

// hubInventory describes every configured deploy and its current state.
func (r *Runner) hubInventory() hubInventory {
	r.mu.Lock()
	cfg := r.cfg
	r.mu.Unlock()
	inv := hubInventory{Labels: copyLabels(cfg.Labels)}
	g := r.gauges(cfg.DeployNames())
	for _, name := range cfg.DeployNames() {
		d := cfg.Deploy[name]
		inv.Deploys = append(inv.Deploys, hubDeploy{
			Name: name, Provider: d.Provider, Repository: d.Repository, Branch: d.Branch, Schedule: d.Schedule,
			Labels: copyLabels(d.labels), State: g[name].state, Running: g[name].running, Queued: g[name].queued,
		})
	}
	return inv
}
