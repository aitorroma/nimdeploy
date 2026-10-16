package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The hub's MCP server (streamable HTTP, JSON responses, no sessions): an AI
// assistant asks how the deploys are doing. Every tool only reads; running or
// rolling back stays in the dashboard. Clients connect with OAuth (see
// huboauth.go) or, like the REST API, with the hub's token.

var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = `nimdeploy hub: the deploys of every agent (server), grouped by client and environment.
Start with "overview" or "attention"; use "list_deploys" to find a deploy and "get_deploy" for its runs and log.
Everything is read-only: to run or roll back a deploy, point the user to the dashboard link in the results.`

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	run         func(ctx context.Context, h *hubServer, base string, args mcpArgs) (any, error)
}

type mcpArgs map[string]any

func (a mcpArgs) str(k string) string {
	s, _ := a[k].(string)
	return strings.TrimSpace(s)
}

func (a mcpArgs) num(k string, def, max int) int {
	n := def
	switch v := a[k].(type) {
	case float64:
		n = int(v)
	case string:
		fmt.Sscan(v, &n)
	}
	if n <= 0 {
		n = def
	}
	if n > max {
		n = max
	}
	return n
}

func (a mcpArgs) flag(k string) bool {
	b, _ := a[k].(bool)
	return b
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

var readOnly = map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}

var mcpTools = []mcpTool{
	{Name: "overview", Title: "Deploys overview",
		Description: "Summary of the hub: deploys by status, agents online and offline, failing deploys per client and environment, and runs per day (successful and failed) over the last days.",
		InputSchema: schema(map[string]any{"days": prop("integer", "Days of run history to count (1-30, default 7).")}),
		run:         mcpOverview},
	{Name: "attention", Title: "What needs attention",
		Description: "What needs someone now: failed deploys, agents offline or never seen, outdated agents and client certificates about to expire.",
		InputSchema: schema(map[string]any{}), run: mcpAttention},
	{Name: "list_deploys", Title: "List deploys",
		Description: "Deploys with their current state (status, commit, when, error). Filters are optional and combine.",
		InputSchema: schema(map[string]any{
			"client": prop("string", "Only this client (exact name, case-insensitive)."),
			"env":    prop("string", "Only this environment, e.g. prod, stage."),
			"agent":  prop("string", "Only deploys of this agent."),
			"status": map[string]any{"type": "string", "description": "Only this status.",
				"enum": []string{StatusSuccess, StatusFailed, StatusRunning, StatusWaiting, StatusSkipped, StatusInterrupted, StatusNever}},
			"query": prop("string", "Text to find in the deploy, client, repository or branch."),
			"limit": prop("integer", "At most this many (default 100, max 500)."),
		}), run: mcpListDeploys},
	{Name: "get_deploy", Title: "Deploy detail",
		Description: "One deploy: its current state, its last runs, commands sent from the dashboard and, if asked, the tail of the last run's log.",
		InputSchema: schema(map[string]any{
			"agent":       prop("string", "The agent that runs it (see list_deploys)."),
			"deploy":      prop("string", "The deploy's name."),
			"runs":        prop("integer", "How many past runs (default 10, max 50)."),
			"include_log": prop("boolean", "Add the last lines of the latest run's log."),
		}, "agent", "deploy"), run: mcpGetDeploy},
	{Name: "list_failures", Title: "Recent failures",
		Description: "Failed runs in the last hours, newest first, with their error and whether the deploy has recovered since.",
		InputSchema: schema(map[string]any{
			"hours":  prop("integer", "How far back (default 24, max 720)."),
			"client": prop("string", "Only this client."),
			"limit":  prop("integer", "At most this many (default 50, max 200)."),
		}), run: mcpFailures},
	{Name: "list_agents", Title: "List agents",
		Description: "The agents (one per server): online or not, when they were last seen, version, client, environment and how many deploys each has.",
		InputSchema: schema(map[string]any{}), run: mcpAgents},
	{Name: "recent_activity", Title: "Recent activity",
		Description: "The latest events: deploys started and finished, rejected webhooks and refused commands. Heartbeats are left out.",
		InputSchema: schema(map[string]any{
			"agent":  prop("string", "Only this agent."),
			"deploy": prop("string", "Only this deploy."),
			"limit":  prop("integer", "At most this many (default 30, max 200)."),
		}), run: mcpActivity},
}

func init() {
	for i := range mcpTools {
		mcpTools[i].Annotations = readOnly
	}
}

// --- transport -----------------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpWho authenticates /mcp: an OAuth token, the hub's token, the trusted
// proxy header, or nobody needed. Never the dashboard cookie, so another
// site can't use the browser's session.
func (h *hubServer) mcpWho(r *http.Request) (string, error) {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && strings.HasPrefix(tok, oauthAccessPfx) {
		grant, _, name, err := h.store.oauthToken(r.Context(), tok, "access")
		if err != nil || grant == "" {
			return "", err
		}
		return "oauth:" + name, nil
	}
	if h.validBearer(r) {
		return "token", nil
	}
	if h.opts.TrustedHeader != "" {
		if v := strings.TrimSpace(r.Header.Get(h.opts.TrustedHeader)); v != "" {
			return v, nil
		}
	}
	if h.opts.uiToken == "" && h.opts.TrustedHeader == "" {
		return "anonymous", nil
	}
	return "", nil
}

func (h *hubServer) handleMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
	who, err := h.mcpWho(r)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	if who == "" {
		hdr := fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, h.baseURL(r))
		if r.Header.Get("Authorization") != "" {
			hdr += `, error="invalid_token"`
		}
		w.Header().Set("WWW-Authenticate", hdr)
		writeError(w, http.StatusUnauthorized, "invalid or missing token")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "this MCP server answers POST requests only (no SSE stream)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"jsonrpc": "2.0", "id": nil, "error": rpcError{-32700, "parse error: send one JSON-RPC request"}})
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" { // a notification: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := h.mcpCall(r, who, req)
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rerr != nil {
		out["error"] = rerr
	} else {
		out["result"] = result
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *hubServer) mcpCall(r *http.Request, who string, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := mcpVersions[0]
		for _, s := range mcpVersions {
			if s == p.ProtocolVersion {
				v = s
			}
		}
		return map[string]any{"protocolVersion": v, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo": map[string]any{"name": "nimdeploy-hub", "title": "nimdeploy hub", "version": version}, "instructions": mcpInstructions}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	case "tools/call":
		var p struct {
			Name      string  `json:"name"`
			Arguments mcpArgs `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "bad params"}
		}
		for _, t := range mcpTools {
			if t.Name != p.Name {
				continue
			}
			if p.Arguments == nil {
				p.Arguments = mcpArgs{}
			}
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			res, err := t.run(ctx, h, h.baseURL(r), p.Arguments)
			if err != nil {
				var ue userError
				if !errors.As(err, &ue) {
					log.Printf("hub: MCP %s for %s: %v", t.Name, who, err)
					err = errors.New("the hub's database didn't answer; try again")
				}
				return map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": err.Error()}}}, nil
			}
			text, _ := json.MarshalIndent(res, "", " ")
			return map[string]any{"content": []any{map[string]string{"type": "text", "text": string(text)}}, "structuredContent": res}, nil
		}
		return nil, &rpcError{-32602, "unknown tool: " + p.Name}
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

// userError is a problem with the request, told to the model as is.
type userError string

func (e userError) Error() string { return string(e) }

// --- tools ---------------------------------------------------------------------------------------

type mcpDeploy struct {
	Agent       string            `json:"agent"`
	Deploy      string            `json:"deploy"`
	Client      string            `json:"client,omitempty"`
	Env         string            `json:"env,omitempty"`
	Status      string            `json:"status"`
	When        *time.Time        `json:"when,omitempty"`
	Duration    string            `json:"duration,omitempty"`
	Repository  string            `json:"repository,omitempty"`
	Branch      string            `json:"branch,omitempty"`
	Tag         string            `json:"tag,omitempty"`
	Commit      string            `json:"commit,omitempty"`
	Message     string            `json:"commit_message,omitempty"`
	Pusher      string            `json:"pusher,omitempty"`
	Trigger     string            `json:"trigger,omitempty"`
	Error       string            `json:"error,omitempty"`
	NextRun     *time.Time        `json:"next_run,omitempty"`
	AgentOnline bool              `json:"agent_online"`
	Removed     bool              `json:"removed,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	URL         string            `json:"url"`
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

func deployOf(base string, v hubView) mcpDeploy {
	st := v.State
	d := mcpDeploy{Agent: v.Agent, Deploy: v.Deploy, Client: v.Client, Env: v.Env, Status: statusOf(st.Status), When: timePtr(v.When),
		Duration: st.Duration, Repository: firstNonEmpty(st.Repository, v.Repository), Branch: firstNonEmpty(st.Branch, v.Branch), Tag: st.Tag,
		Commit: shortCommit(st.Commit), Message: clip(st.Message, 200), Pusher: st.Pusher, Trigger: st.Trigger, Error: clip(st.Error, 500),
		NextRun: st.NextRun, AgentOnline: v.AgentOnline, Removed: !v.InInventory, Labels: v.Labels, URL: base + deployHref(v.Agent, v.Deploy)}
	return d
}

func statusOf(s string) string {
	if s == "" {
		return StatusNever
	}
	return s
}

func mcpOverview(ctx context.Context, h *hubServer, base string, a mcpArgs) (any, error) {
	views, agents, err := h.views(ctx)
	if err != nil {
		return nil, err
	}
	days := a.num("days", 7, 30)
	daily, err := h.store.daily(ctx, days)
	if err != nil {
		return nil, err
	}
	byStatus := map[string]int{}
	type cell struct {
		Client  string   `json:"client"`
		Env     string   `json:"env"`
		Deploys int      `json:"deploys"`
		Failing []string `json:"failing,omitempty"`
	}
	cells := map[[2]string]*cell{}
	for _, v := range views {
		if !v.InInventory {
			continue
		}
		byStatus[statusOf(v.State.Status)]++
		k := [2]string{v.Client, v.Env}
		if cells[k] == nil {
			cells[k] = &cell{Client: firstNonEmpty(v.Client, "(no client)"), Env: v.Env}
		}
		cells[k].Deploys++
		if v.State.Status == StatusFailed {
			cells[k].Failing = append(cells[k].Failing, v.Deploy)
		}
	}
	groups := make([]cell, 0, len(cells))
	for _, c := range cells {
		groups = append(groups, *c)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Client != groups[j].Client {
			return strings.ToLower(groups[i].Client) < strings.ToLower(groups[j].Client)
		}
		return groups[i].Env < groups[j].Env
	})
	now := time.Now()
	agentCount := map[string]int{}
	for _, ag := range agents {
		if ag.RevokedAt == "" {
			agentCount[ag.stateText(now)]++
		}
	}
	type day struct {
		Day     string `json:"day"`
		Success int    `json:"success"`
		Failed  int    `json:"failed"`
		Other   int    `json:"other,omitempty"`
	}
	runs := make([]day, 0, len(daily))
	var ok, bad int
	for _, d := range daily {
		runs = append(runs, day{d.Day.Format("2006-01-02"), d.Success, d.Failed, d.Other})
		ok, bad = ok+d.Success, bad+d.Failed
	}
	rate := ""
	if ok+bad > 0 {
		rate = fmt.Sprintf("%.1f%%", float64(ok)*100/float64(ok+bad))
	}
	return map[string]any{"hub_version": version, "now": now.UTC().Format(time.RFC3339), "deploys_by_status": byStatus, "agents": agentCount,
		"attention_items": len(h.attention("en", hubData{views: views, agents: agents})), "groups": groups,
		"runs_per_day": runs, "success_rate": rate, "dashboard": base + "/"}, nil
}

func mcpAttention(ctx context.Context, h *hubServer, base string, _ mcpArgs) (any, error) {
	views, agents, err := h.views(ctx)
	if err != nil {
		return nil, err
	}
	type item struct {
		Severity string `json:"severity"`
		Title    string `json:"title"`
		Detail   string `json:"detail,omitempty"`
		Ago      string `json:"ago,omitempty"`
		URL      string `json:"url"`
	}
	items := []item{}
	for _, it := range h.attention("en", hubData{views: views, agents: agents}) {
		items = append(items, item{Severity: map[string]string{"bad": "critical", "warn": "warning"}[it.Sev], Title: it.Title, Detail: it.Desc, Ago: it.Ago, URL: base + it.Href})
	}
	return map[string]any{"items": items, "count": len(items)}, nil
}

func mcpListDeploys(ctx context.Context, h *hubServer, base string, a mcpArgs) (any, error) {
	views, _, err := h.views(ctx)
	if err != nil {
		return nil, err
	}
	client, env, agent, status, query := a.str("client"), a.str("env"), a.str("agent"), a.str("status"), strings.ToLower(a.str("query"))
	limit := a.num("limit", 100, 500)
	out := []mcpDeploy{}
	total := 0
	for _, v := range views {
		if (client != "" && !strings.EqualFold(v.Client, client)) || (env != "" && !strings.EqualFold(v.Env, env)) ||
			(agent != "" && v.Agent != agent) || (status != "" && statusOf(v.State.Status) != status) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(strings.Join([]string{v.Deploy, v.Client, v.Env, v.Agent, v.Repository, v.Branch}, " ")), query) {
			continue
		}
		total++
		if len(out) < limit {
			out = append(out, deployOf(base, v))
		}
	}
	return map[string]any{"deploys": out, "total": total, "truncated": total > len(out)}, nil
}

func (h *hubServer) findDeploy(ctx context.Context, agent, deploy string) (hubView, error) {
	if agent == "" || deploy == "" {
		return hubView{}, userError("agent and deploy are required: find them with list_deploys")
	}
	views, _, err := h.views(ctx)
	if err != nil {
		return hubView{}, err
	}
	var near []string
	for _, v := range views {
		if v.Agent == agent && v.Deploy == deploy {
			return v, nil
		}
		if v.Deploy == deploy || strings.EqualFold(v.Deploy, deploy) {
			near = append(near, v.Agent+"/"+v.Deploy)
		}
	}
	msg := fmt.Sprintf("no deploy %q on agent %q", deploy, agent)
	if len(near) > 0 {
		msg += "; did you mean " + strings.Join(near, ", ") + "?"
	}
	return hubView{}, userError(msg)
}

func mcpGetDeploy(ctx context.Context, h *hubServer, base string, a mcpArgs) (any, error) {
	v, err := h.findDeploy(ctx, a.str("agent"), a.str("deploy"))
	if err != nil {
		return nil, err
	}
	type run struct {
		Status     string     `json:"status"`
		Trigger    string     `json:"trigger,omitempty"`
		StartedAt  *time.Time `json:"started_at,omitempty"`
		FinishedAt *time.Time `json:"finished_at,omitempty"`
		Duration   string     `json:"duration,omitempty"`
		Commit     string     `json:"commit,omitempty"`
		Message    string     `json:"commit_message,omitempty"`
		Tag        string     `json:"tag,omitempty"`
		Pusher     string     `json:"pusher,omitempty"`
		Error      string     `json:"error,omitempty"`
		Phases     any        `json:"phases,omitempty"`
		Ansible    any        `json:"ansible,omitempty"`
	}
	states, err := h.store.runs(ctx, v.Agent, v.Deploy, a.num("runs", 10, 50))
	if err != nil {
		return nil, err
	}
	runs := make([]run, 0, len(states))
	for _, s := range states {
		r := run{Status: statusOf(s.Status), Trigger: s.Trigger, StartedAt: s.StartedAt, FinishedAt: s.FinishedAt, Duration: s.Duration,
			Commit: shortCommit(s.Commit), Message: clip(s.Message, 200), Tag: s.Tag, Pusher: s.Pusher, Error: clip(s.Error, 1000)}
		if len(s.Phases) > 0 {
			r.Phases = s.Phases
		}
		if s.Ansible != nil {
			r.Ansible = s.Ansible
		}
		runs = append(runs, r)
	}
	type command struct {
		Action    string    `json:"action"`
		Commit    string    `json:"commit,omitempty"`
		By        string    `json:"by"`
		CreatedAt time.Time `json:"created_at"`
		Status    string    `json:"status"`
		Result    string    `json:"result,omitempty"`
	}
	cmds, err := h.store.commandsFor(ctx, v.Agent, v.Deploy, 10)
	if err != nil {
		return nil, err
	}
	commands := make([]command, 0, len(cmds))
	for _, c := range cmds {
		commands = append(commands, command{c.Action, shortCommit(c.Commit), c.By, c.CreatedAt, c.Status, clip(c.Result, 300)})
	}
	out := map[string]any{"deploy": deployOf(base, v), "provider": v.Provider, "schedule": v.Schedule, "runs": runs, "commands": commands}
	if a.flag("include_log") {
		evs, err := h.store.events(ctx, v.Agent, v.Deploy, hubEventDeployDone, 1)
		if err != nil {
			return nil, err
		}
		if len(evs) > 0 {
			out["log_tail"] = evs[0].LogTail
		} else {
			out["log_tail"] = []string{}
		}
	}
	return out, nil
}

func mcpFailures(ctx context.Context, h *hubServer, base string, a mcpArgs) (any, error) {
	since := time.Now().Add(-time.Duration(a.num("hours", 24, 720)) * time.Hour)
	limit := a.num("limit", 50, 200)
	client := a.str("client")
	evs, err := h.store.events(ctx, "", "", hubEventDeployDone, 1000)
	if err != nil {
		return nil, err
	}
	views, _, err := h.views(ctx)
	if err != nil {
		return nil, err
	}
	current := map[[2]string]hubView{}
	for _, v := range views {
		current[[2]string{v.Agent, v.Deploy}] = v
	}
	type failure struct {
		Agent     string    `json:"agent"`
		Deploy    string    `json:"deploy"`
		Client    string    `json:"client,omitempty"`
		Env       string    `json:"env,omitempty"`
		Time      time.Time `json:"time"`
		Commit    string    `json:"commit,omitempty"`
		Error     string    `json:"error,omitempty"`
		Recovered bool      `json:"recovered"` // its latest run went fine
		URL       string    `json:"url"`
	}
	out := []failure{}
	for _, e := range evs {
		if e.Time.Before(since) || len(out) >= limit {
			break
		}
		if e.State == nil || e.State.Status != StatusFailed {
			continue
		}
		v := current[[2]string{e.Agent, e.Deploy}]
		c := firstNonEmpty(v.Client, e.Labels["client"])
		if client != "" && !strings.EqualFold(c, client) {
			continue
		}
		out = append(out, failure{Agent: e.Agent, Deploy: e.Deploy, Client: c, Env: firstNonEmpty(v.Env, e.Labels["environment"]), Time: e.Time,
			Commit: shortCommit(e.State.Commit), Error: clip(firstNonEmpty(e.State.Error, e.Reason), 500),
			Recovered: v.State.Status == StatusSuccess, URL: base + deployHref(e.Agent, e.Deploy)})
	}
	return map[string]any{"since": since.UTC().Format(time.RFC3339), "failures": out, "count": len(out)}, nil
}

func mcpAgents(ctx context.Context, h *hubServer, base string, _ mcpArgs) (any, error) {
	views, agents, err := h.views(ctx)
	if err != nil {
		return nil, err
	}
	deploys := map[string]int{}
	for _, v := range views {
		if v.InInventory {
			deploys[v.Agent]++
		}
	}
	type agent struct {
		Name            string     `json:"name"`
		State           string     `json:"state"`
		Client          string     `json:"client,omitempty"`
		Env             string     `json:"env,omitempty"`
		Host            string     `json:"host,omitempty"`
		Version         string     `json:"version,omitempty"`
		LastSeen        *time.Time `json:"last_seen,omitempty"`
		Outbox          int64      `json:"events_waiting,omitempty"`
		Deploys         int        `json:"deploys"`
		AcceptsCommands bool       `json:"accepts_commands"`
		CertNotAfter    *time.Time `json:"cert_expires,omitempty"`
	}
	now := time.Now()
	out := []agent{}
	for _, ag := range agents {
		if ag.RevokedAt != "" {
			continue
		}
		out = append(out, agent{Name: ag.Name, State: ag.stateText(now), Client: ag.ClientName(), Env: ag.EnvName(), Host: ag.Host,
			Version: ag.Version, LastSeen: timePtr(ag.LastSeen), Outbox: ag.Outbox, Deploys: deploys[ag.Name],
			AcceptsCommands: ag.AcceptsCommands, CertNotAfter: timePtr(ag.CertNotAfter)})
	}
	return map[string]any{"agents": out, "hub_version": version, "url": base + "/agents"}, nil
}

func mcpActivity(ctx context.Context, h *hubServer, base string, a mcpArgs) (any, error) {
	limit := a.num("limit", 30, 200)
	evs, err := h.store.events(ctx, a.str("agent"), a.str("deploy"), "", 1000)
	if err != nil {
		return nil, err
	}
	type event struct {
		Time   time.Time `json:"time"`
		Type   string    `json:"type"`
		Agent  string    `json:"agent"`
		Deploy string    `json:"deploy,omitempty"`
		Status string    `json:"status,omitempty"`
		Commit string    `json:"commit,omitempty"`
		Detail string    `json:"detail,omitempty"`
	}
	out := []event{}
	for _, e := range evs {
		if e.Type == hubEventHeartbeat {
			continue
		}
		if len(out) >= limit {
			break
		}
		ev := event{Time: e.Time, Type: e.Type, Agent: e.Agent, Deploy: e.Deploy, Detail: clip(e.Reason, 300)}
		if e.State != nil {
			ev.Status, ev.Commit = e.State.Status, shortCommit(e.State.Commit)
			ev.Detail = clip(firstNonEmpty(e.State.Error, e.Reason), 300)
		}
		out = append(out, ev)
	}
	return map[string]any{"events": out, "url": base + "/activity"}, nil
}
