package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Commands: the hub can ask an agent to run a deploy again or roll it back.
// The hub never connects to agents: an agent with accept_commands = true
// asks for its pending commands with a request the hub holds open until
// there is one (long poll). Answers are signed with the agent's token, so
// only its hub can give it orders; each command runs at most once, and only
// within hubCommandTTL of being created.

const (
	hubCommandsPath     = "/api/v1/commands"
	hubCommandWait      = 25 * time.Second
	hubCommandLongPoll  = 40 * time.Second // client timeout, above the hub's wait
	hubCommandActionRun = "run"
	hubCommandActionRB  = "rollback"
	TriggerHub          = "hub"
)

var shaRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// emitWithID queues an event that keeps its ID (a command's).
func (a *hubAgent) emitWithID(ev hubEvent) { a.emit(ev) }

type hubCommandBatch struct {
	Commands []hubCommand `json:"commands"`
}

// --- hub side -------------------------------------------------------------------------------

// commandWaiters wakes an agent's long poll when a command is created for it.
type commandWaiters struct {
	mu sync.Mutex
	m  map[string][]chan struct{}
}

func (c *commandWaiters) wait(agent string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string][]chan struct{}{}
	}
	c.m[agent] = append(c.m[agent], ch)
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		list := c.m[agent]
		for i, x := range list {
			if x == ch {
				c.m[agent] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}
}

func (c *commandWaiters) notify(agent string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.m[agent] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// authAgent checks an agent's request: known and not revoked, its own
// client certificate when it requires one, a fresh timestamp and a valid
// signature over signed. It answers the request itself on failure.
func (h *hubServer) authAgent(w http.ResponseWriter, r *http.Request, signed []byte) (hubAgentRow, bool) {
	name := r.Header.Get(hubAgentHeader)
	if !agentNameRe.MatchString(name) {
		writeError(w, http.StatusUnauthorized, "missing or bad "+hubAgentHeader)
		return hubAgentRow{}, false
	}
	refuse := func(why string) (hubAgentRow, bool) {
		log.Printf("hub: refused %s from %s (%s): %s", r.URL.Path, name, clientAddr(r), why)
		writeError(w, http.StatusUnauthorized, why)
		return hubAgentRow{}, false
	}
	// Any verified client certificate must be this agent's.
	if names := clientNames(r); len(names) > 0 && !clientAllowed(r, []string{name}) {
		return refuse(fmt.Sprintf("client certificate %v is not for agent %s", names, name))
	}
	if h.opts.TLSClientCA != "" && h.opts.AgentCerts == tlsClientAuthRequire && len(clientNames(r)) == 0 {
		return refuse("client certificate required for agent " + name)
	}
	a, ok, err := h.store.agent(r.Context(), name)
	if err != nil {
		log.Printf("hub: %v", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return hubAgentRow{}, false
	}
	if !ok || a.RevokedAt != "" {
		return refuse("unknown or revoked agent")
	}
	if a.RequireCert {
		fp := requestCertFingerprint(r)
		if fp == "" || !hmac.Equal([]byte(fp), []byte(a.CertFP)) {
			return refuse("this agent's client certificate is required")
		}
	}
	if err := verifyHubSignature(a.token, r.Header.Get(hubTimestampHeader), signed, r.Header.Get(hubSignatureHeader), time.Now()); err != nil {
		return refuse(err.Error())
	}
	return a, true
}

// handleCommands hands an agent its pending commands, waiting up to
// hubCommandWait for one to appear.
func (h *hubServer) handleCommands(w http.ResponseWriter, r *http.Request) {
	a, ok := h.authAgent(w, r, []byte(http.MethodGet+" "+hubCommandsPath))
	if !ok {
		return
	}
	wait := hubCommandWait
	if s, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && s >= 0 && time.Duration(s)*time.Second < wait {
		wait = time.Duration(s) * time.Second
	}
	ch, done := h.waiters.wait(a.Name)
	defer done()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Second) // commands from another hub process
	defer tick.Stop()
	for {
		cmds, err := h.store.takeCommands(r.Context(), a.Name)
		if err != nil {
			log.Printf("hub: commands for %s: %v", a.Name, err)
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		if len(cmds) > 0 {
			for _, c := range cmds {
				log.Printf("hub: command %s sent to %s: %s %s (by %s)", c.ID, a.Name, c.Action, c.Deploy, c.By)
			}
			h.writeSigned(w, a.token, hubCommandBatch{Commands: cmds})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			h.writeSigned(w, a.token, hubCommandBatch{Commands: []hubCommand{}})
			return
		case <-ch:
		case <-tick.C:
		}
	}
}

// writeSigned answers with a body signed by the agent's token.
func (h *hubServer) writeSigned(w http.ResponseWriter, token string, v any) {
	body, _ := json.Marshal(v)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(hubTimestampHeader, ts)
	w.Header().Set(hubSignatureHeader, signHub([]byte(token), ts, body))
	_, _ = w.Write(body)
}

func newCommandID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// queueCommand records a command for an agent and wakes its long poll.
func (h *hubServer) queueCommand(ctx context.Context, c hubCommand) (hubCommand, error) {
	if c.Action != hubCommandActionRun && c.Action != hubCommandActionRB {
		return c, errors.New("action must be run or rollback")
	}
	a, ok, err := h.store.agent(ctx, c.Agent)
	if err != nil {
		return c, err
	}
	if !ok || a.RevokedAt != "" {
		return c, fmt.Errorf("unknown agent %s", c.Agent)
	}
	if !a.AcceptsCommands {
		return c, fmt.Errorf("%s does not accept commands: set accept_commands = true in its [hub] section", c.Agent)
	}
	if c.Commit != "" && !shaRe.MatchString(c.Commit) {
		return c, errors.New("commit must be a hexadecimal SHA")
	}
	c.ID, c.CreatedAt = newCommandID(), time.Now().UTC()
	if err := h.store.createCommand(ctx, c); err != nil {
		return c, err
	}
	log.Printf("hub: command %s queued for %s: %s %s (by %s)", c.ID, c.Agent, c.Action, c.Deploy, c.By)
	h.waiters.notify(c.Agent)
	return c, nil
}

// --- agent side ------------------------------------------------------------------------------

// commandLoop asks the hub for commands while accept_commands is on.
func (a *hubAgent) commandLoop() {
	defer close(a.cmdDone)
	backoff := 2 * time.Second
	for {
		select {
		case <-a.stop:
			return
		default:
		}
		cfg := a.config()
		if !cfg.AcceptCommands {
			select {
			case <-a.stop:
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}
		cmds, err := a.fetchCommands(cfg)
		if err != nil {
			a.cmdReport(err)
			select {
			case <-a.stop:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, hubMaxBackoff)
			continue
		}
		a.cmdReport(nil)
		backoff = 2 * time.Second
		for _, c := range cmds {
			a.runCommand(c)
		}
	}
}

func (a *hubAgent) cmdReport(err error) {
	a.mu.Lock()
	changed := (err != nil) != a.cmdFailing
	a.cmdFailing = err != nil
	a.mu.Unlock()
	if changed && err != nil {
		log.Printf("hub: cannot fetch commands from %s: %v", a.config().URL, err)
	} else if changed {
		log.Printf("hub: fetching commands from %s again", a.config().URL)
	}
}

func (a *hubAgent) fetchCommands(cfg HubConfig) ([]hubCommand, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hubCommandLongPoll)
	defer cancel()
	go func() {
		select {
		case <-a.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL+hubCommandsPath+"?wait="+strconv.Itoa(int(hubCommandWait.Seconds())), nil)
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("User-Agent", "nimdeploy/"+version)
	req.Header.Set(hubAgentHeader, cfg.Agent)
	req.Header.Set(hubTimestampHeader, ts)
	req.Header.Set(hubSignatureHeader, signHub([]byte(cfg.token), ts, []byte(http.MethodGet+" "+hubCommandsPath)))
	resp, err := a.longPollClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(truncate(string(body), 200)))
	}
	// Only our hub knows our token: an answer without its signature is ignored.
	if err := verifyHubSignature(cfg.token, resp.Header.Get(hubTimestampHeader), body, resp.Header.Get(hubSignatureHeader), time.Now()); err != nil {
		return nil, fmt.Errorf("answer not signed by the hub: %v", err)
	}
	var batch hubCommandBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, err
	}
	return batch.Commands, nil
}

// runCommand starts a deploy or rollback the hub asked for, once.
func (a *hubAgent) runCommand(c hubCommand) {
	a.mu.Lock()
	if a.cmdSeen == nil {
		a.cmdSeen = newDeliveryCache(1000)
	}
	dup := a.cmdSeen.has("", c.ID)
	a.cmdSeen.add("", c.ID)
	a.mu.Unlock()
	if dup || c.ID == "" {
		return
	}
	refuse := func(why string) {
		log.Printf("hub: command %s (%s %s by %s) refused: %s", c.ID, c.Action, c.Deploy, c.By, why)
		a.emitWithID(hubEvent{ID: c.ID, Type: hubEventCommandRefused, Deploy: c.Deploy, Reason: why})
	}
	if age := time.Since(c.CreatedAt); age > hubCommandTTL+hubMaxClockSkew || age < -hubMaxClockSkew {
		refuse("expired")
		return
	}
	r := a.runner
	cfg := r.config()
	d, ok := cfg.Deploy[c.Deploy]
	if !ok {
		refuse("no deploy " + c.Deploy + " on this server")
		return
	}
	by := "hub:" + firstNonEmpty(c.By, "unknown")
	t := Trigger{Source: TriggerHub, Provider: d.Provider, Repository: d.Repository, Commit: c.Commit, Pusher: by,
		Delivery: hubCommandDelivery + c.ID}
	if _, git := providers[d.Provider]; git {
		t.Ref, t.Branch = "refs/heads/"+d.Branch, d.Branch
	}
	switch c.Action {
	case hubCommandActionRun:
		params, err := checkManualParams(d.Params, c.Params)
		if err != nil {
			refuse(err.Error())
			return
		}
		t.Params = params
	case hubCommandActionRB:
		if _, git := providers[d.Provider]; !git {
			refuse("rollback needs a git deploy")
			return
		}
		t.Source = TriggerRollback
		if t.Commit == "" {
			commit, _, err := r.RollbackTarget(d.Name)
			if err != nil {
				refuse(err.Error())
				return
			}
			t.Commit = commit
		}
	default:
		refuse("unknown action " + c.Action)
		return
	}
	res, err := r.Submit(d.Name, t)
	if err != nil {
		refuse(err.Error())
		return
	}
	log.Printf("deploy=%s %s requested by %s through the hub: %s", d.Name, c.Action, by, res.Result)
}
