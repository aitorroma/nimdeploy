package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestHubMCPOAuth: an AI client finds the hub's OAuth metadata, registers,
// someone logged in allows it, and its token reads deploys through /mcp
// until the connection is revoked.
func TestHubMCPOAuth(t *testing.T) {
	h, srv := newTestHub(t)
	ctx := context.Background()
	if _, err := h.store.addAgent(ctx, "prod-1"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Add(-time.Hour)
	t1 := t0.Add(time.Minute)
	labels := map[string]string{"client": "Acme", "environment": "prod"}
	failed := &State{Deploy: "web", Status: StatusFailed, Commit: "abcdef0123456789", Labels: labels, StartedAt: &t0, FinishedAt: &t1, Error: "exit status 1"}
	if err := h.store.ingest(ctx, "prod-1", []hubEvent{
		{ID: "e1", Type: hubEventDeployDone, Time: t1, Deploy: "web", State: failed, LogTail: []string{"building", "boom"}},
	}); err != nil {
		t.Fatal(err)
	}

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	api := &http.Client{}

	// Without a token, /mcp points at the metadata.
	resp, err := api.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), srv.URL+"/.well-known/oauth-protected-resource") {
		t.Fatalf("unauthenticated /mcp: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	var meta struct {
		Resource string   `json:"resource"`
		Servers  []string `json:"authorization_servers"`
	}
	getJSON(t, api, srv.URL+"/.well-known/oauth-protected-resource", &meta)
	if meta.Resource != srv.URL+"/mcp" || len(meta.Servers) != 1 || meta.Servers[0] != srv.URL {
		t.Fatalf("resource metadata %+v", meta)
	}
	var as struct {
		R string `json:"registration_endpoint"`
		A string `json:"authorization_endpoint"`
		T string `json:"token_endpoint"`
	}
	getJSON(t, api, srv.URL+"/.well-known/oauth-authorization-server", &as)
	if as.R != srv.URL+"/oauth/register" || as.A != srv.URL+"/oauth/authorize" || as.T != srv.URL+"/oauth/token" {
		t.Fatalf("server metadata %+v", as)
	}

	// Registration: only https or loopback redirects.
	for _, bad := range []string{`{"redirect_uris":["http://evil.example/cb"]}`, `{"redirect_uris":[]}`, `{"redirect_uris":["javascript:alert(1)"]}`} {
		resp, _ := api.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(bad))
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("registration %s accepted: %d", bad, resp.StatusCode)
		}
	}
	const redirect = "https://chat.example.com/callback"
	resp, err = api.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(`{"client_name":"ChatGPT","redirect_uris":["`+redirect+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		ID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || !strings.HasPrefix(reg.ID, "ndc_") {
		t.Fatalf("register: %d %+v", resp.StatusCode, reg)
	}

	verifier := "verifier-0123456789-0123456789-0123456789-abc"
	sum := sha256.Sum256([]byte(verifier))
	authQ := url.Values{"client_id": {reg.ID}, "redirect_uri": {redirect}, "response_type": {"code"}, "state": {"xyz"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}

	// Not logged in: to the login page, and back here afterwards.
	resp, _ = browser.Get(srv.URL + "/oauth/authorize?" + authQ.Encode())
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("authorize without session: %d %s", resp.StatusCode, loc)
	}
	resp, _ = browser.PostForm(srv.URL+"/login", url.Values{"token": {testHubToken}, "next": {"/oauth/authorize?" + authQ.Encode()}})
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Location"), "/oauth/authorize?") {
		t.Fatalf("login next: %s", resp.Header.Get("Location"))
	}

	// An unregistered redirect is refused here, not followed.
	bad := url.Values{}
	for k, v := range authQ {
		bad[k] = v
	}
	bad.Set("redirect_uri", "https://evil.example/cb")
	resp, _ = browser.Get(srv.URL + "/oauth/authorize?" + bad.Encode())
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad redirect: %d", resp.StatusCode)
	}

	resp, _ = browser.Get(srv.URL + "/oauth/authorize?" + authQ.Encode())
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "ChatGPT") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "https://chat.example.com") {
		t.Fatalf("consent page: %d %s", resp.StatusCode, page)
	}
	var csrf string
	for _, c := range jar.Cookies(mustURL(srv.URL)) {
		if c.Name == "nd_csrf" {
			csrf = c.Value
		}
	}
	form := url.Values{"grant_name": {"ChatGPT deploys"}, "decision": {"allow"}}
	for k, v := range authQ {
		form[k] = v
	}
	// Without the CSRF token the consent doesn't count.
	resp, _ = browser.PostForm(srv.URL+"/oauth/authorize", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("consent without CSRF: %d", resp.StatusCode)
	}
	form.Set("csrf", csrf)
	resp, _ = browser.PostForm(srv.URL+"/oauth/authorize", form)
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	code := back.Query().Get("code")
	if resp.StatusCode != http.StatusFound || back.Host != "chat.example.com" || back.Query().Get("state") != "xyz" || code == "" {
		t.Fatalf("consent: %d %s", resp.StatusCode, back)
	}

	exchange := func(f url.Values) (int, map[string]any) {
		resp, err := api.PostForm(srv.URL+"/oauth/token", f)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if st, _ := exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {reg.ID}, "code_verifier": {"wrong"}}); st != http.StatusBadRequest {
		t.Fatalf("wrong verifier: %d", st)
	}
	// The code was spent on the failed try: a client has to start again.
	if st, _ := exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {reg.ID}, "code_verifier": {verifier}}); st != http.StatusBadRequest {
		t.Fatalf("reused code: %d", st)
	}
	resp, _ = browser.PostForm(srv.URL+"/oauth/authorize", form)
	resp.Body.Close()
	back, _ = url.Parse(resp.Header.Get("Location"))
	st, tok := exchange(url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {reg.ID},
		"redirect_uri": {redirect}, "code_verifier": {verifier}})
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if st != http.StatusOK || !strings.HasPrefix(access, oauthAccessPfx) || !strings.HasPrefix(refresh, oauthRefreshPfx) {
		t.Fatalf("token: %d %v", st, tok)
	}

	// The token reads through MCP…
	call := func(token, method string, params any) (int, map[string]any) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := api.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	st, out := call(access, "initialize", map[string]any{"protocolVersion": "2025-03-26"})
	if st != http.StatusOK || out["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize: %d %v", st, out)
	}
	st, out = call(access, "tools/list", nil)
	if tools := out["result"].(map[string]any)["tools"].([]any); st != http.StatusOK || len(tools) != len(mcpTools) {
		t.Fatalf("tools/list: %d %v", st, out)
	}
	text := func(out map[string]any) string {
		res, _ := out["result"].(map[string]any)
		content, _ := res["content"].([]any)
		if len(content) == 0 {
			t.Fatalf("no content: %v", out)
		}
		return content[0].(map[string]any)["text"].(string)
	}
	_, out = call(access, "tools/call", map[string]any{"name": "list_deploys", "arguments": map[string]any{"status": "failed", "client": "acme"}})
	if s := text(out); !strings.Contains(s, `"deploy": "web"`) || !strings.Contains(s, "abcdef012345") || strings.Contains(s, "abcdef0123456789") {
		t.Fatalf("list_deploys: %s", s)
	}
	_, out = call(access, "tools/call", map[string]any{"name": "get_deploy", "arguments": map[string]any{"agent": "prod-1", "deploy": "web", "include_log": true}})
	if s := text(out); !strings.Contains(s, "boom") || !strings.Contains(s, "exit status 1") {
		t.Fatalf("get_deploy: %s", s)
	}
	_, out = call(access, "tools/call", map[string]any{"name": "get_deploy", "arguments": map[string]any{"agent": "other", "deploy": "web"}})
	if res := out["result"].(map[string]any); res["isError"] != true || !strings.Contains(text(out), "prod-1/web") {
		t.Fatalf("get_deploy on the wrong agent: %v", out)
	}
	for _, name := range []string{"overview", "attention", "list_failures", "list_agents", "recent_activity"} {
		_, out = call(access, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}})
		if res := out["result"].(map[string]any); res["isError"] == true || !strings.Contains(text(out), "web") && name != "list_agents" {
			t.Errorf("%s: %v", name, out)
		}
	}
	if _, out = call(access, "tools/call", map[string]any{"name": "nope"}); out["error"] == nil {
		t.Error("unknown tool accepted")
	}
	// …but opens nothing else: not the REST API, not the dashboard.
	if code, _ := hubGet(t, srv, "/api/v1/deploys", access); code != http.StatusUnauthorized {
		t.Errorf("OAuth token on the REST API: %d", code)
	}
	// The hub's own token works on /mcp too.
	if st, _ := call(testHubToken, "ping", nil); st != http.StatusOK {
		t.Errorf("hub token on /mcp: %d", st)
	}

	// Refresh rotates both tokens; the old refresh token is spent.
	st, tok = exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {reg.ID}})
	access2, _ := tok["access_token"].(string)
	if st != http.StatusOK || access2 == "" || access2 == access {
		t.Fatalf("refresh: %d %v", st, tok)
	}
	if st, _ := exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}); st != http.StatusBadRequest {
		t.Fatalf("spent refresh token: %d", st)
	}

	// Revoking the connection cuts every token at once.
	grants, err := h.store.oauthGrants(ctx)
	if err != nil || len(grants) != 1 || grants[0].Name != "ChatGPT deploys" || grants[0].Redirect != "chat.example.com" || grants[0].CreatedBy != "admin" {
		t.Fatalf("grants %+v %v", grants, err)
	}
	if code, body := hubGet(t, srv, "/agents", testHubToken); code != http.StatusOK || !strings.Contains(body, "ChatGPT deploys") || !strings.Contains(body, srv.URL+"/mcp") {
		t.Fatalf("agents page: %d", code)
	}
	if err := h.store.oauthRevoke(ctx, grants[0].ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := call(access2, "ping", nil); st != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", st)
	}
	if st, _ := exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}}); st != http.StatusBadRequest {
		t.Fatalf("revoked refresh: %d", st)
	}
}

func getJSON(t *testing.T, c *http.Client, u string, v any) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func mustURL(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}

func TestOKRedirect(t *testing.T) {
	for u, want := range map[string]bool{
		"https://chatgpt.com/connector_platform_oauth_redirect": true,
		"http://127.0.0.1:33418/callback":                       true,
		"http://localhost/cb":                                   true,
		"http://example.com/cb":                                 false,
		"https://user:pw@example.com/cb":                        false,
		"https://example.com/cb#frag":                           false,
		"myapp://cb":                                            false,
	} {
		if okRedirect(u) != want {
			t.Errorf("okRedirect(%q) != %v", u, want)
		}
	}
}
