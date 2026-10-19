package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The hub as an OAuth 2.1 authorization server for its own /mcp endpoint, as
// the MCP spec asks: a client (ChatGPT, Claude…) finds the metadata,
// registers itself (Dynamic Client Registration), someone logged in to the
// dashboard allows it, and the client gets a short-lived access token it
// renews with a refresh token. Public clients with PKCE S256 only. The tokens
// only open /mcp, which only reads: they can't change anything in the hub.

const (
	oauthAccessTTL  = time.Hour
	oauthRefreshTTL = 90 * 24 * time.Hour
	oauthCodeTTL    = 5 * time.Minute
	oauthMaxClients = 500
	oauthAccessPfx  = "ndo_"
	oauthRefreshPfx = "ndr_"
	mcpPath         = "/mcp"
)

type oauthClient struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
}

type oauthCode struct {
	client, redirect, challenge, name, who string
	exp                                    time.Time
}

// oauthCodes live in memory: they last minutes and the hub runs one replica.
type oauthCodes struct {
	sync.Mutex
	m map[string]oauthCode
}

func (c *oauthCodes) put(code string, v oauthCode) {
	c.Lock()
	defer c.Unlock()
	if c.m == nil {
		c.m = map[string]oauthCode{}
	}
	now := time.Now()
	for k, x := range c.m {
		if now.After(x.exp) {
			delete(c.m, k)
		}
	}
	c.m[code] = v
}

// take returns a code once: it can't be used twice.
func (c *oauthCodes) take(code string) (oauthCode, bool) {
	c.Lock()
	defer c.Unlock()
	v, ok := c.m[code]
	delete(c.m, code)
	return v, ok && time.Now().Before(v.exp)
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func tokenHash(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func oauthTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// --- store ------------------------------------------------------------------------------------

type oauthGrant struct {
	ID, ClientID, Name, CreatedBy string
	CreatedAt, LastUsed           time.Time
	Redirect                      string // where the client lives, to recognise it
}

func (s *hubStore) oauthAddClient(ctx context.Context, c oauthClient) error {
	rows, err := s.db.Query(ctx, `SELECT count(*) AS n FROM oauth_clients`)
	if err != nil {
		return err
	}
	if len(rows) == 1 && rows[0].int("n") >= oauthMaxClients {
		return errors.New("too many clients registered")
	}
	return s.db.Exec(ctx, sqlStmt{SQL: `INSERT INTO oauth_clients (id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		Args: []any{c.ID, c.Name, jsonText(c.RedirectURIs), nowText()}})
}

func (s *hubStore) oauthClient(ctx context.Context, id string) (oauthClient, bool, error) {
	rows, err := s.db.Query(ctx, `SELECT * FROM oauth_clients WHERE id = ?`, id)
	if err != nil || len(rows) == 0 {
		return oauthClient{}, false, err
	}
	c := oauthClient{ID: rows[0].str("id"), Name: rows[0].str("name")}
	_ = json.Unmarshal([]byte(rows[0].str("redirect_uris")), &c.RedirectURIs)
	return c, true, nil
}

// oauthIssue creates a grant (when grantID is "") or rotates its tokens.
func (s *hubStore) oauthIssue(ctx context.Context, grantID, clientID, name, who, oldRefresh string) (grant, access, refresh string, err error) {
	access, refresh = oauthAccessPfx+randomToken(32), oauthRefreshPfx+randomToken(32)
	now := time.Now()
	var stmts []sqlStmt
	if grantID == "" {
		grantID = "g_" + randomToken(8)
		stmts = append(stmts, sqlStmt{SQL: `INSERT INTO oauth_grants (id, client_id, name, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
			Args: []any{grantID, clientID, name, who, nowText()}})
	}
	if oldRefresh != "" {
		stmts = append(stmts, sqlStmt{SQL: `DELETE FROM oauth_tokens WHERE hash = ?`, Args: []any{tokenHash(oldRefresh)}})
	}
	stmts = append(stmts,
		sqlStmt{SQL: `INSERT INTO oauth_tokens (hash, grant_id, kind, expires_at) VALUES (?, ?, 'access', ?)`,
			Args: []any{tokenHash(access), grantID, oauthTime(now.Add(oauthAccessTTL))}},
		sqlStmt{SQL: `INSERT INTO oauth_tokens (hash, grant_id, kind, expires_at) VALUES (?, ?, 'refresh', ?)`,
			Args: []any{tokenHash(refresh), grantID, oauthTime(now.Add(oauthRefreshTTL))}})
	return grantID, access, refresh, s.db.Exec(ctx, stmts...)
}

// oauthToken finds the live grant a token belongs to: "" when the token is
// unknown, expired, of the other kind or its grant was revoked.
func (s *hubStore) oauthToken(ctx context.Context, token, kind string) (grantID, clientID, name string, err error) {
	rows, err := s.db.Query(ctx, `SELECT g.id AS id, g.client_id AS client_id, g.name AS name, g.last_used AS last_used
		FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id
		WHERE t.hash = ? AND t.kind = ? AND t.expires_at > ? AND g.revoked_at IS NULL`, tokenHash(token), kind, oauthTime(time.Now()))
	if err != nil || len(rows) == 0 {
		return "", "", "", err
	}
	r := rows[0]
	if kind == "access" && time.Since(parseTime(r.str("last_used"))) > 5*time.Minute {
		_ = s.db.Exec(ctx, sqlStmt{SQL: `UPDATE oauth_grants SET last_used = ? WHERE id = ?`, Args: []any{nowText(), r.str("id")}})
	}
	return r.str("id"), r.str("client_id"), r.str("name"), nil
}

func (s *hubStore) oauthGrants(ctx context.Context) ([]oauthGrant, error) {
	rows, err := s.db.Query(ctx, `SELECT g.*, c.redirect_uris AS redirect_uris FROM oauth_grants g LEFT JOIN oauth_clients c ON c.id = g.client_id
		WHERE g.revoked_at IS NULL ORDER BY g.created_at DESC`)
	if err != nil {
		return nil, err
	}
	out := make([]oauthGrant, 0, len(rows))
	for _, r := range rows {
		g := oauthGrant{ID: r.str("id"), ClientID: r.str("client_id"), Name: r.str("name"), CreatedBy: r.str("created_by"),
			CreatedAt: parseTime(r.str("created_at")), LastUsed: parseTime(r.str("last_used"))}
		var uris []string
		if json.Unmarshal([]byte(r.str("redirect_uris")), &uris) == nil && len(uris) > 0 {
			if u, err := url.Parse(uris[0]); err == nil {
				g.Redirect = u.Host
			}
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *hubStore) oauthRevoke(ctx context.Context, grantID string) error {
	rows, err := s.db.Query(ctx, `SELECT id FROM oauth_grants WHERE id = ? AND revoked_at IS NULL`, grantID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errors.New("no such connection")
	}
	return s.db.Exec(ctx, sqlStmt{SQL: `UPDATE oauth_grants SET revoked_at = ? WHERE id = ?`, Args: []any{nowText(), grantID}},
		sqlStmt{SQL: `DELETE FROM oauth_tokens WHERE grant_id = ?`, Args: []any{grantID}})
}

// --- endpoints --------------------------------------------------------------------------------

func (h *hubServer) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", h.oauthResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", h.oauthResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", h.oauthServer)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/mcp", h.oauthServer)
	mux.HandleFunc("GET /.well-known/openid-configuration", h.oauthServer)
	mux.HandleFunc("POST /oauth/register", h.oauthRegister)
	mux.HandleFunc("GET /oauth/authorize", h.ui(h.oauthAuthorize))
	mux.HandleFunc("POST /oauth/authorize", h.ui(h.oauthConsent))
	mux.HandleFunc("POST /oauth/token", h.oauthTokenEndpoint)
	mux.HandleFunc("POST /ui/oauth/{id}/revoke", h.changes(h.uiRevokeGrant))
	mux.HandleFunc("OPTIONS /oauth/{x}", corsPreflight)
	mux.HandleFunc("OPTIONS /.well-known/{x...}", corsPreflight)
}

// baseURL is the hub as clients reach it: -public-url, else the request's host.
func (h *hubServer) baseURL(r *http.Request) string {
	if u := strings.TrimRight(h.opts.PublicURL, "/"); u != "" {
		return u
	}
	return h.hubURL(r)
}

func corsPreflight(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id, Mcp-Protocol-Version")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func oauthJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, code, v)
}

func oauthErr(w http.ResponseWriter, code int, kind, desc string) {
	oauthJSON(w, code, map[string]string{"error": kind, "error_description": desc})
}

func (h *hubServer) oauthResource(w http.ResponseWriter, r *http.Request) {
	base := h.baseURL(r)
	oauthJSON(w, http.StatusOK, map[string]any{"resource": base + mcpPath, "authorization_servers": []string{base},
		"bearer_methods_supported": []string{"header"}, "scopes_supported": []string{"deploys:read"}, "resource_name": "nimdeploy hub"})
}

func (h *hubServer) oauthServer(w http.ResponseWriter, r *http.Request) {
	base := h.baseURL(r)
	oauthJSON(w, http.StatusOK, map[string]any{
		"issuer": base, "authorization_endpoint": base + "/oauth/authorize", "token_endpoint": base + "/oauth/token",
		"registration_endpoint": base + "/oauth/register", "response_types_supported": []string{"code"},
		"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{"deploys:read"},
	})
}

// okRedirect: https anywhere, plain http only back to this machine (desktop clients).
func okRedirect(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return host == "127.0.0.1" || host == "localhost" || host == "::1"
	}
	return false
}

func clipName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60])
	}
	return s
}

func (h *hubServer) oauthRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name         string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "bad JSON")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "between 1 and 10 redirect_uris")
		return
	}
	for _, u := range in.RedirectURIs {
		if len(u) > 2000 || !okRedirect(u) {
			oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri not allowed: "+clip(u, 200))
			return
		}
	}
	c := oauthClient{ID: "ndc_" + randomToken(16), Name: clipName(in.Name), RedirectURIs: in.RedirectURIs}
	if err := h.store.oauthAddClient(r.Context(), c); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	log.Printf("hub: OAuth client %s (%q) registered from %s", c.ID, c.Name, clientAddr(r))
	oauthJSON(w, http.StatusCreated, map[string]any{"client_id": c.ID, "client_name": c.Name, "redirect_uris": c.RedirectURIs,
		"client_id_issued_at": time.Now().Unix(), "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
}

// oauthCheck validates an authorization request. Without a valid redirect
// the problem is shown here; with one, it goes back to the client.
func (h *hubServer) oauthCheck(ctx context.Context, q url.Values) (c oauthClient, redirect, problem string) {
	c, ok, err := h.store.oauthClient(ctx, q.Get("client_id"))
	if err != nil {
		return c, "", "database unavailable"
	}
	if !ok {
		return c, "", "unknown client: add the connector again"
	}
	redirect = q.Get("redirect_uri")
	if redirect == "" && len(c.RedirectURIs) == 1 {
		redirect = c.RedirectURIs[0]
	}
	found := false
	for _, u := range c.RedirectURIs {
		found = found || u == redirect
	}
	if !found {
		return c, "", "the redirect_uri isn't the one registered"
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		return c, redirect, "invalid_request"
	}
	return c, redirect, ""
}

func oauthBack(w http.ResponseWriter, r *http.Request, redirect string, params url.Values) {
	u, _ := url.Parse(redirect)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>nimdeploy · {{.T.title}}</title><link rel="stylesheet" href="/static/hub.css"></head>
<body><main style="max-width:34em;margin:10vh auto;padding:0 16px;font:16px/1.5 system-ui,sans-serif">
{{if .Problem}}<h1>{{.T.cant}}</h1><p>{{.Problem}}</p>{{else}}
<h1>{{.T.title}}</h1>
<p>{{.Intro}}</p>
<ul><li>{{.T.can}}</li><li>{{.T.cannot}}</li></ul>
<form method="post" action="/oauth/authorize">
{{range $k, $v := .Hidden}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<input type="hidden" name="csrf" value="{{.CSRF}}">
<p><label>{{.T.name}}<br><input name="grant_name" value="{{.Name}}" maxlength="60" style="font:inherit;padding:8px;width:100%;box-sizing:border-box"></label></p>
<p><button name="decision" value="allow" class="btn primary" style="font:inherit;padding:10px 16px">{{.T.allow}}</button>
<button name="decision" value="deny" class="btn" style="font:inherit;padding:10px 16px">{{.T.deny}}</button></p>
</form><p style="opacity:.7;font-size:14px">{{.T.revoke}}</p>{{end}}
</main></body></html>`))

var consentText = map[string]map[string]string{
	"en": {"title": "Connect an AI to the hub?", "cant": "Can't connect", "name": "Name of the connection",
		"intro":  "%s (returns to %s) wants to read this hub through MCP.",
		"can":    "It will see deploys, their state and logs, agents and recent activity.",
		"cannot": "It can't run, roll back or change anything.", "allow": "Allow", "deny": "Cancel",
		"revoke": "You can disconnect it at any time in Agents → AI connections."},
	"es": {"title": "¿Conectar una IA al hub?", "cant": "No se puede conectar", "name": "Nombre de la conexión",
		"intro":  "%s (vuelve a %s) quiere leer este hub por MCP.",
		"can":    "Verá los despliegues, su estado y registros, los agentes y la actividad reciente.",
		"cannot": "No puede lanzar, deshacer ni cambiar nada.", "allow": "Permitir", "deny": "Cancelar",
		"revoke": "Puedes desconectarla cuando quieras en Agentes → Conexiones de IA."},
}

func (h *hubServer) renderConsent(w http.ResponseWriter, r *http.Request, code int, data map[string]any, redirect string) {
	lang := hubLang(r)
	t, ok := consentText[lang]
	if !ok {
		lang, t = "en", consentText["en"]
	}
	data["Lang"], data["T"] = lang, t
	formAction := "'self'"
	if u, err := url.Parse(redirect); err == nil && u.Host != "" {
		// Browsers check form-action against the redirect after the post too.
		formAction += " " + u.Scheme + "://" + u.Host
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; style-src-attr 'unsafe-inline'; form-action "+formAction+"; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(code)
	if err := consentPage.Execute(w, data); err != nil {
		log.Printf("hub: consent page: %v", err)
	}
}

func (h *hubServer) oauthAuthorize(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	c, redirect, problem := h.oauthCheck(r.Context(), q)
	if redirect == "" {
		h.renderConsent(w, r, http.StatusBadRequest, map[string]any{"Problem": problem}, "")
		return
	}
	if problem != "" {
		oauthBack(w, r, redirect, url.Values{"error": {problem}, "state": {q.Get("state")}})
		return
	}
	name := c.Name
	if name == "" {
		name = "MCP client"
	}
	ru, _ := url.Parse(redirect)
	hidden := map[string]string{}
	for _, k := range []string{"client_id", "state", "code_challenge", "code_challenge_method", "response_type", "scope", "resource"} {
		hidden[k] = q.Get(k)
	}
	hidden["redirect_uri"] = redirect
	lang := hubLang(r)
	if _, ok := consentText[lang]; !ok {
		lang = "en"
	}
	h.renderConsent(w, r, http.StatusOK, map[string]any{"Name": name, "Hidden": hidden, "CSRF": csrfToken(w, r),
		"Intro": strings.Replace(strings.Replace(consentText[lang]["intro"], "%s", name, 1), "%s", ru.Host, 1)}, redirect)
}

func (h *hubServer) oauthConsent(w http.ResponseWriter, r *http.Request, who string) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	if c, err := r.Cookie("nd_csrf"); err != nil || f.Get("csrf") == "" || subtle.ConstantTimeCompare([]byte(f.Get("csrf")), []byte(c.Value)) != 1 {
		http.Error(w, "missing or invalid CSRF token: reload the page", http.StatusForbidden)
		return
	}
	c, redirect, problem := h.oauthCheck(r.Context(), f)
	if redirect == "" {
		h.renderConsent(w, r, http.StatusBadRequest, map[string]any{"Problem": problem}, "")
		return
	}
	state := f.Get("state")
	if problem != "" {
		oauthBack(w, r, redirect, url.Values{"error": {problem}, "state": {state}})
		return
	}
	if f.Get("decision") != "allow" {
		oauthBack(w, r, redirect, url.Values{"error": {"access_denied"}, "state": {state}})
		return
	}
	name := firstNonEmpty(clipName(f.Get("grant_name")), c.Name, "MCP client")
	code := randomToken(24)
	h.oauth.put(code, oauthCode{client: c.ID, redirect: redirect, challenge: f.Get("code_challenge"), name: name,
		who: h.actor(r, who), exp: time.Now().Add(oauthCodeTTL)})
	oauthBack(w, r, redirect, url.Values{"code": {code}, "state": {state}})
}

func (h *hubServer) oauthTokenEndpoint(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	f := r.PostForm
	ctx := r.Context()
	reply := func(access, refresh string) {
		oauthJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer",
			"expires_in": int(oauthAccessTTL.Seconds()), "refresh_token": refresh, "scope": "deploys:read"})
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		c, ok := h.oauth.take(f.Get("code"))
		if !ok || c.client != f.Get("client_id") || (f.Get("redirect_uri") != "" && f.Get("redirect_uri") != c.redirect) {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "unknown or expired code")
			return
		}
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(c.challenge)) != 1 {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "code_verifier doesn't match")
			return
		}
		_, access, refresh, err := h.store.oauthIssue(ctx, "", c.client, c.name, c.who, "")
		if err != nil {
			oauthErr(w, http.StatusServiceUnavailable, "server_error", "database unavailable")
			return
		}
		log.Printf("hub: AI connection %q allowed by %s", c.name, c.who)
		reply(access, refresh)
	case "refresh_token":
		old := f.Get("refresh_token")
		grant, client, name, err := h.store.oauthToken(ctx, old, "refresh")
		if err != nil {
			oauthErr(w, http.StatusServiceUnavailable, "server_error", "database unavailable")
			return
		}
		if grant == "" || (f.Get("client_id") != "" && f.Get("client_id") != client) {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "unknown, expired or revoked refresh_token")
			return
		}
		_, access, refresh, err := h.store.oauthIssue(ctx, grant, client, name, "", old)
		if err != nil {
			oauthErr(w, http.StatusServiceUnavailable, "server_error", "database unavailable")
			return
		}
		reply(access, refresh)
	default:
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token")
	}
}

// --- dashboard ----------------------------------------------------------------------------------

type grantRow struct{ ID, Name, By, From, Since, Used string }

func (h *hubServer) grantRows(ctx context.Context, lang string) []grantRow {
	grants, err := h.store.oauthGrants(ctx)
	if err != nil {
		log.Printf("hub: AI connections: %v", err)
		return nil
	}
	now := time.Now()
	out := make([]grantRow, 0, len(grants))
	for _, g := range grants {
		row := grantRow{ID: g.ID, Name: g.Name, By: g.CreatedBy, From: g.Redirect, Since: g.CreatedAt.Format("2006-01-02"), Used: hubT(lang, "ai.never")}
		if !g.LastUsed.IsZero() {
			row.Used = agoIn(lang, now.Sub(g.LastUsed))
		}
		out = append(out, row)
	}
	return out
}

func (h *hubServer) uiRevokeGrant(w http.ResponseWriter, r *http.Request, who string) {
	id := r.PathValue("id")
	if err := h.store.oauthRevoke(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	log.Printf("hub: AI connection %s revoked by %s from %s", id, h.actor(r, who), clientAddr(r))
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}
