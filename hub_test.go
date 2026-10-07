package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testHubToken = "hub-ui-token-0123456789"

func newTestHub(t *testing.T) (*hubServer, *httptest.Server) {
	t.Helper()
	store := testHubStore(t)
	h := &hubServer{store: store, opts: &hubOptions{uiToken: testHubToken, RetainDays: 90}, started: time.Now()}
	srv := httptest.NewServer(h.routes())
	t.Cleanup(srv.Close)
	return h, srv
}

func hubGet(t *testing.T, srv *httptest.Server, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

// TestHubEndToEnd: a real agent deploys, its events reach the hub signed,
// and the dashboard, API and metrics show them.
func TestHubEndToEnd(t *testing.T) {
	h, srv := newTestHub(t)
	ctx := context.Background()
	token, err := h.store.addAgent(ctx, "stage-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_HUB_TOKEN", token)
	e := newEnv(t, `echo deploying; echo "step 2"; exit 1`, `[server]
api_token_env = "TEST_API_TOKEN"
[labels]
client = "Squirrel Media"
environment = "stage"
[hub]
url = "`+srv.URL+`"
agent = "stage-1"
token_env = "TEST_HUB_TOKEN"
heartbeat = "10s"
`)
	e.push(t, pushOpts{delivery: "h1"})
	e.waitIdle(t)

	var deps []hubDeployRow
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		deps, _ = h.store.deploys(ctx)
		if len(deps) == 1 && deps[0].State.Status == StatusFailed && deps[0].Repository != "" {
			break // finished event and the first heartbeat (inventory) arrived
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(deps) != 1 || deps[0].State.Status != StatusFailed || deps[0].Labels["client"] != "Squirrel Media" || deps[0].Repository != "acme/agency" {
		t.Fatalf("hub deploys %+v", deps)
	}
	evs, _ := h.store.events(ctx, "stage-1", "agency", hubEventDeployDone, 1)
	if len(evs) != 1 || !strings.Contains(strings.Join(evs[0].LogTail, "\n"), "step 2") {
		t.Fatalf("finished event %+v", evs)
	}
	agents, _ := h.store.agents(ctx)
	if len(agents) != 1 || !agents[0].online(time.Now()) || agents[0].Version != version {
		t.Fatalf("agents %+v", agents)
	}

	// Dashboard: login required, then the matrix and the detail page.
	if code, _ := hubGet(t, srv, "/", ""); code != http.StatusSeeOther {
		t.Errorf("anonymous dashboard: %d", code)
	}
	code, body := hubGet(t, srv, "/", testHubToken)
	if code != 200 || !strings.Contains(body, "Squirrel Media") || !strings.Contains(body, `class="chip s-failed`) {
		t.Errorf("overview %d:\n%s", code, body)
	}
	code, body = hubGet(t, srv, "/d/stage-1/agency", testHubToken)
	if code != 200 || !strings.Contains(body, "step 2") || !strings.Contains(body, "Squirrel Media · stage · agency") {
		t.Errorf("detail %d:\n%s", code, body)
	}
	if code, body := hubGet(t, srv, "/agents", testHubToken); code != 200 || !strings.Contains(body, "a-online") {
		t.Errorf("agents page %d:\n%s", code, body)
	}
	if code, _ := hubGet(t, srv, "/?client=Nobody", testHubToken); code != 200 {
		t.Errorf("filtered: %d", code)
	}

	// API and metrics.
	code, body = hubGet(t, srv, "/api/v1/deploys?label=environment=stage", testHubToken)
	var api []hubDeployJSON
	_ = json.Unmarshal([]byte(body), &api)
	if code != 200 || len(api) != 1 || !api[0].AgentOnline || api[0].State.Status != StatusFailed {
		t.Errorf("api %d %s", code, body)
	}
	if _, body := hubGet(t, srv, "/api/v1/deploys?label=environment=production", testHubToken); strings.TrimSpace(body) != "[]" {
		t.Errorf("filtered api %s", body)
	}
	if code, _ := hubGet(t, srv, "/api/v1/deploys", ""); code != http.StatusUnauthorized {
		t.Errorf("api without token: %d", code)
	}
	_, body = hubGet(t, srv, "/metrics", testHubToken)
	for _, want := range []string{
		`nimdeploy_hub_agent_up{agent="stage-1",version="` + version + `"} 1`,
		`nimdeploy_hub_deploy_info{agent="stage-1",deploy="agency",client="Squirrel Media",environment="stage"} 1`,
		`nimdeploy_hub_deploy_status{agent="stage-1",deploy="agency",status="failed"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s:\n%s", want, body)
		}
	}

	// Login form sets a session cookie that opens the dashboard.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.PostForm(srv.URL+"/login", url.Values{"token": {testHubToken}, "next": {"//evil.example"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" || len(resp.Cookies()) != 1 {
		t.Fatalf("login %d %v", resp.StatusCode, resp.Header)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/agents", nil)
	req.AddCookie(resp.Cookies()[0])
	if resp, _ := c.Do(req); resp.StatusCode != 200 {
		t.Errorf("with session cookie: %d", resp.StatusCode)
	}
	// A cookie doesn't allow managing agents: that needs the bearer token.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/v1/agents", strings.NewReader(`{"name":"x"}`))
	req.AddCookie(resp.Cookies()[0])
	if resp, _ := c.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("add agent with cookie: %d", resp.StatusCode)
	}
	resp, err = c.PostForm(srv.URL+"/login", url.Values{"token": {"wrong"}})
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("wrong login: %d", resp.StatusCode)
		}
	}

	// Revoking the agent stops its events.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/agents/stage-1", nil)
	req.Header.Set("Authorization", "Bearer "+testHubToken)
	if resp, _ := c.Do(req); resp.StatusCode != 200 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if err := e.runner.hub.post(ctx, hubBatch{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("revoked agent: %v", err)
	}
}

func TestHubIngestAuth(t *testing.T) {
	h, srv := newTestHub(t)
	token, _ := h.store.addAgent(context.Background(), "a1")
	body := []byte(`{"events":[{"id":"x1","type":"deploy.started","time":"2026-10-07T10:00:00Z","deploy":"web","state":{"deploy":"web","status":"running"}},{"id":"x2","type":"future.thing","time":"2026-10-07T10:00:00Z"}]}`)
	post := func(agent, tok string, ts int64, b []byte) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+hubEventsPath, strings.NewReader(string(b)))
		s := strconv.FormatInt(ts, 10)
		req.Header.Set(hubAgentHeader, agent)
		req.Header.Set(hubTimestampHeader, s)
		req.Header.Set(hubSignatureHeader, signHub([]byte(tok), s, b))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	now := time.Now().Unix()
	cases := []struct {
		name, agent, token string
		ts                 int64
		want               int
	}{
		{"wrong token", "a1", "nope", now, 401},
		{"unknown agent", "a2", token, now, 401},
		{"old timestamp", "a1", token, now - 3600, 401},
		{"bad agent name", "a 1", token, now, 401},
		{"ok", "a1", token, now, 200},
	}
	for _, c := range cases {
		if got := post(c.agent, c.token, c.ts, body); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	if got := post("a1", token, now, []byte(`{bad`)); got != 400 {
		t.Errorf("bad JSON: %d", got)
	}
	evs, _ := h.store.events(context.Background(), "", "", "", 0)
	if len(evs) != 1 || evs[0].ID != "x1" {
		t.Errorf("unknown event types must be skipped: %+v", evs)
	}
}

func TestHubHelpers(t *testing.T) {
	if safeNext("//evil.com") != "/" || safeNext("https://x") != "/" || safeNext("/\\x") != "/" || safeNext("/d/a/b?x=1") != "/d/a/b?x=1" {
		t.Error("safeNext")
	}
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	if err := verifyHubSignature("k", ts, []byte("b"), signHub([]byte("k"), ts, []byte("b")), now); err != nil {
		t.Error(err)
	}
	if err := verifyHubSignature("k", ts, []byte("b2"), signHub([]byte("k"), ts, []byte("b")), now); err == nil {
		t.Error("tampered body accepted")
	}
	ev, ok := sanitizeHubEvent(hubEvent{ID: "1", Type: hubEventDeployDone, Time: now,
		State:   &State{Log: "/var/log/x", Params: []Param{{Name: "p"}}, Labels: map[string]string{"ok": "1", "Bad": "x", "deploy": "y"}},
		LogTail: make([]string, 900)})
	if !ok || ev.State.Log != "" || ev.State.Params != nil || len(ev.State.Labels) != 1 || len(ev.LogTail) != hubMaxLogLines {
		t.Errorf("sanitize %+v", ev.State)
	}
	if _, ok := sanitizeHubEvent(hubEvent{ID: "", Type: hubEventDeployDone, Time: now}); ok {
		t.Error("event without id accepted")
	}
	m := buildMatrix([]hubView{
		{Client: "B", Env: "production"}, {Client: "A", Env: "stage"}, {Client: "", Env: ""}, {Client: "A", Env: "dev"},
	})
	if strings.Join(m.Clients, ",") != "A,B," || strings.Join(m.Envs, ",") != "dev,stage,production," {
		t.Errorf("matrix %q %q", m.Clients, m.Envs)
	}
	if redactURL("http://user:pw@db:8080") != "http://***@db:8080" {
		t.Error(redactURL("http://user:pw@db:8080"))
	}
}
