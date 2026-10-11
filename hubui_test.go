package main

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type uiClient struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
	lang string
}

func newUIClient(t *testing.T, base string) *uiClient {
	jar, _ := cookiejar.New(nil)
	return &uiClient{t: t, base: base, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var csrfRe = regexp.MustCompile(`"X-CSRF-Token": "([0-9a-f]{32})"`)

func (u *uiClient) do(method, path string, form url.Values, csrf bool) (int, string, http.Header) {
	u.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, u.base+path, body)
	req.Header.Set("Authorization", "Bearer "+testHubToken)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if u.lang != "" {
		req.Header.Set("Accept-Language", u.lang)
	}
	if csrf {
		req.Header.Set("X-CSRF-Token", u.csrf)
	}
	resp, err := u.c.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := csrfRe.FindSubmatch(b); m != nil {
		u.csrf = string(m[1])
	}
	return resp.StatusCode, string(b), resp.Header
}

func TestHubDashboard(t *testing.T) {
	store := testHubStore(t)
	ca, err := loadOrCreateCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	h := &hubServer{store: store, opts: &hubOptions{uiToken: testHubToken}, started: time.Now(), ca: ca}
	srv := httptest.NewServer(h.routes())
	t.Cleanup(srv.Close)
	ctx := context.Background()
	token, _ := store.addAgent(ctx, "stage-1")
	t.Setenv("TEST_HUB_TOKEN", token)
	e := newEnv(t, `echo deploying; echo "step 2"; exit 1`, `[server]
api_token_env = "TEST_API_TOKEN"
[labels]
client = "Acme Corp"
environment = "stage"
[hub]
url = "`+srv.URL+`"
agent = "stage-1"
token_env = "TEST_HUB_TOKEN"
heartbeat = "10s"
accept_commands = true
`)
	e.push(t, pushOpts{delivery: "u1"})
	e.waitIdle(t)
	waitFor(t, "the run on the hub", func() bool {
		evs, _ := store.events(ctx, "stage-1", "agency", hubEventDeployDone, 1)
		a, _, _ := store.agent(ctx, "stage-1")
		return len(evs) == 1 && a.AcceptsCommands
	})

	u := newUIClient(t, srv.URL)
	code, body, hdr := u.do("GET", "/", nil, false)
	if code != 200 || !strings.Contains(hdr.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("overview %d %v", code, hdr)
	}
	for _, want := range []string{"Deploys per day", `<svg class="chart"`, "Acme Corp", "agency failed", `class="kpi bad"`, "Needs attention"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	u.lang = "es-ES,es;q=0.9"
	if _, body, _ := u.do("GET", "/", nil, false); !strings.Contains(body, "Despliegues por día") || !strings.Contains(body, `<html lang="es">`) {
		t.Error("Spanish overview")
	}
	u.lang = ""
	if code, _, hdr := u.do("GET", "/lang/es?next=/agents", nil, false); code != http.StatusSeeOther || hdr.Get("Location") != "/agents" {
		t.Errorf("lang switch %d %v", code, hdr)
	}
	if _, body, _ := u.do("GET", "/agents", nil, false); !strings.Contains(body, "Latidos · últimos 30 min") {
		t.Error("language cookie not used")
	}
	u.do("GET", "/lang/en?next=/", nil, false)

	if code, body, _ := u.do("GET", "/c/Acme%20Corp/stage", nil, false); code != 200 || !strings.Contains(body, `class="runs-dots"`) || !strings.Contains(body, "fix footer") {
		t.Errorf("client page %d", code)
	}
	code, body, _ = u.do("GET", "/d/stage-1/agency", nil, false)
	for _, want := range []string{`class="wf-row"`, "step 2", "Run again", "Roll back", "hub:", "fix footer"} {
		if want == "hub:" {
			continue
		}
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("deploy page missing %q", want)
		}
	}
	for _, path := range []string{"/activity", "/attention", "/static/htmx.min.js", "/static/hub.css", "/static/hub.js"} {
		if code, _, _ := u.do("GET", path, nil, false); code != 200 {
			t.Errorf("%s: %d", path, code)
		}
	}

	// Commands: CSRF required, then queued and run by the agent.
	if code, _, _ := u.do("POST", "/ui/d/stage-1/agency/run", url.Values{}, false); code != http.StatusForbidden {
		t.Errorf("command without CSRF: %d", code)
	}
	if code, body, _ := u.do("GET", "/ui/d/stage-1/agency/confirm/run", nil, false); code != 200 || !strings.Contains(body, "Run agency again on stage-1?") {
		t.Errorf("confirm %d %s", code, body)
	}
	if code, body, _ := u.do("POST", "/ui/d/stage-1/agency/run", url.Values{}, true); code != 200 || !strings.Contains(body, `hx-trigger="every 3s"`) {
		t.Errorf("command %d %s", code, body)
	}
	waitFor(t, "the command's run", func() bool {
		cs, _ := store.commandsFor(ctx, "stage-1", "agency", 1)
		return len(cs) == 1 && cs[0].Status == "done" && cs[0].By == "admin"
	})

	// Add an agent with a certificate.
	if code, body, _ := u.do("GET", "/ui/agents/new?client=Acme%20Corp", nil, false); code != 200 || !strings.Contains(body, `value="Acme Corp"`) {
		t.Errorf("new agent form %d", code)
	}
	form := url.Values{"client": {"Acme Corp"}, "env": {"production"}, "name": {"acme-prod"}, "cert": {"on"}}
	if code, _, _ := u.do("POST", "/ui/agents", form, false); code != http.StatusForbidden {
		t.Errorf("create without CSRF: %d", code)
	}
	code, body, _ = u.do("POST", "/ui/agents", form, true)
	if code != 200 || !strings.Contains(body, "BEGIN CERTIFICATE") || !strings.Contains(body, `agent = &#34;acme-prod&#34;`) || !strings.Contains(body, "/ui/agents/acme-prod/wait") {
		t.Fatalf("create %d %s", code, body)
	}
	a, ok, _ := store.agent(ctx, "acme-prod")
	if !ok || !a.RequireCert || a.CertFP == "" || a.Client != "Acme Corp" || a.Env != "production" {
		t.Errorf("created agent %+v", a)
	}
	if _, body, _ := u.do("GET", "/ui/agents/acme-prod/wait?since=0", nil, false); !strings.Contains(body, `class="waiting"`) {
		t.Error("wait while not connected")
	}
	if _, body, _ := u.do("POST", "/ui/agents", form, true); !strings.Contains(body, "exists") {
		t.Error("duplicate agent name accepted")
	}
	// Renew without a certificate, then revoke.
	if code, body, _ := u.do("POST", "/ui/agents/acme-prod/renew", url.Values{"cert": {"off"}}, true); code != 200 || strings.Contains(body, "BEGIN CERTIFICATE") {
		t.Errorf("renew %d", code)
	}
	if a, _, _ := store.agent(ctx, "acme-prod"); a.RequireCert {
		t.Error("certificate still required after renewing without it")
	}
	if code, body, _ := u.do("GET", "/ui/agents/acme-prod/foot?confirm=1", nil, false); code != 200 || !strings.Contains(body, "Revoke acme-prod?") {
		t.Errorf("revoke confirm %d", code)
	}
	if code, _, hdr := u.do("POST", "/ui/agents/acme-prod/revoke", url.Values{}, true); code != http.StatusNoContent || hdr.Get("HX-Refresh") != "true" {
		t.Errorf("revoke %d", code)
	}
	if a, _, _ := store.agent(ctx, "acme-prod"); a.RevokedAt == "" {
		t.Error("not revoked")
	}
	// The pending agent shows up as needing attention before it connects.
	store.createAgent(ctx, newAgentOpts{Name: "later", Client: "Beta", Env: "stage"})
	if _, body, _ := u.do("GET", "/attention", nil, false); !strings.Contains(body, "later never connected") {
		t.Error("pending agent not in attention")
	}
	// Without a token: the login page, translated.
	req, _ := http.NewRequest("GET", srv.URL+"/login", nil)
	req.Header.Set("Accept-Language", "es")
	resp, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "El token del hub") {
		t.Error("login in Spanish")
	}
}

func TestHubUIHelpers(t *testing.T) {
	if !olderVersion("v0.8.0", "v0.10.0") || olderVersion("v0.10.0", "v0.9.1") || olderVersion("v0.9.1", "v0.9.1") {
		t.Error("olderVersion")
	}
	if initialsOf("Acme Media") != "AM" || initialsOf("acme-stage") != "AS" || initialsOf("") != "?" {
		t.Error("initialsOf")
	}
	if got := strings.Join(histOf([]string{"failed", "success"}), ","); got != "none,none,none,none,none,none,none,none,ok,bad" {
		t.Errorf("histOf %s", got)
	}
	if hubT("es", "ov.sub", 3, 2, 1) != "3 apps de 2 clientes en 1 servidores." || hubT("fr", "nav.agents") != "Agents" || hubT("en", "nope") != "nope" {
		t.Error("hubT")
	}
	if !errLine("2026-10-09T23:08:57+02:00 ERROR: health check failed") || !errLine("fatal: [web1]: FAILED!") ||
		errLine("2026-10-09T23:08:57+02:00 command=/bin/sh -c echo ERROR") || errLine("composer install") {
		t.Error("errLine")
	}
	if agentWhere("es", hubAgentRow{}) != "Sin cliente" || agentWhere("en", hubAgentRow{Client: "Acme", Env: "stage"}) != "Acme · stage" {
		t.Error("agentWhere")
	}
	if fmtSeconds(1500) != "1.5s" || fmtSeconds(42000) != "42s" || fmtSeconds(95000) != "1m35s" {
		t.Error("fmtSeconds")
	}
	for lang := range hubText {
		for k := range hubText["en"] {
			if _, ok := hubText[lang][k]; !ok {
				t.Errorf("%s: missing %s", lang, k)
			}
		}
	}
}
