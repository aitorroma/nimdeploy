package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// "nimdeploy hub" runs the central console: agents (nimdeploy on each
// server, with [hub] in their config) send their events here, signed with
// their own token; the hub stores them in libSQL and shows every client,
// environment and server in one dashboard. It is configured with flags or
// environment variables only, so it fits a container.

const (
	hubMaxBody        = 8 << 20
	hubMaxEvents      = 500
	hubMaxLogLines    = 500
	hubMaxLineLen     = 2000
	hubSessionCookie  = "nimdeploy_hub"
	hubSessionMaxAge  = 7 * 24 * time.Hour
	hubDefaultRetain  = 90
	hubDefaultListen  = "127.0.0.1:9100"
	hubTokenEnv       = "NIMDEPLOY_HUB_TOKEN"
	hubDBTokenEnvName = "LIBSQL_AUTH_TOKEN"
)

type hubOptions struct {
	Listen        string
	DatabaseURL   string
	databaseToken string
	uiToken       string
	TrustedHeader string
	RetainDays    int
	NoAuth        bool
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// hubFlags declares the options shared by every hub subcommand.
func hubFlags(fs *flag.FlagSet, serve bool) *hubOptions {
	o := &hubOptions{}
	fs.StringVar(&o.DatabaseURL, "database", os.Getenv("NIMDEPLOY_HUB_DATABASE_URL"),
		"libSQL URL: http://sqld:8080 or libsql://<db>.turso.io ($NIMDEPLOY_HUB_DATABASE_URL); its token comes from $"+hubDBTokenEnvName)
	if serve {
		fs.StringVar(&o.Listen, "listen", envOr("NIMDEPLOY_HUB_LISTEN", hubDefaultListen), "address to listen on ($NIMDEPLOY_HUB_LISTEN)")
		fs.StringVar(&o.TrustedHeader, "trusted-header", os.Getenv("NIMDEPLOY_HUB_TRUSTED_HEADER"),
			"header set by an authenticating proxy (e.g. Cf-Access-Authenticated-User-Email): requests carrying it can see the dashboard ($NIMDEPLOY_HUB_TRUSTED_HEADER)")
		days, _ := strconv.Atoi(envOr("NIMDEPLOY_HUB_RETAIN_DAYS", strconv.Itoa(hubDefaultRetain)))
		fs.IntVar(&o.RetainDays, "retain-days", days, "delete events older than this, 0 keeps them ($NIMDEPLOY_HUB_RETAIN_DAYS)")
		fs.BoolVar(&o.NoAuth, "no-auth", false, "allow a dashboard without $"+hubTokenEnv+" or -trusted-header on a non-local address")
	}
	return o
}

func (o *hubOptions) open(ctx context.Context) (*hubStore, error) {
	if o.DatabaseURL == "" {
		return nil, errors.New("set -database or $NIMDEPLOY_HUB_DATABASE_URL (a libSQL server: sqld or Turso)")
	}
	o.databaseToken = os.Getenv(hubDBTokenEnvName)
	db, err := newLibsqlClient(o.DatabaseURL, o.databaseToken)
	if err != nil {
		return nil, err
	}
	s := &hubStore{db: db}
	// sqld may still be starting next to us (compose, a pod): retry for a while.
	var lastErr error
	for i := 0; i < 30; i++ {
		if lastErr = s.migrate(ctx); lastErr == nil {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("database %s: %w", o.DatabaseURL, lastErr)
}

const hubUsage = `usage: nimdeploy hub <command> [flags]

  serve                    run the hub (dashboard, API and the agents' endpoint)
  agent add <name>         create an agent, or give it a new token; prints the token
  agent list               agents, their version and when they were last seen
  agent revoke <name>      the agent's events are refused from now on
  healthcheck              exit 0 if the local hub and its database answer
                           (for container health checks)

The database comes from -database / $NIMDEPLOY_HUB_DATABASE_URL (and
$LIBSQL_AUTH_TOKEN); the dashboard token from $NIMDEPLOY_HUB_TOKEN.
`

func cliHub(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, hubUsage)
		return 2
	}
	switch args[0] {
	case "serve":
		fs := flag.NewFlagSet("hub serve", flag.ExitOnError)
		o := hubFlags(fs, true)
		_ = fs.Parse(args[1:])
		return hubServe(o)
	case "agent":
		if len(args) < 2 {
			fmt.Fprint(os.Stderr, hubUsage)
			return 2
		}
		fs := flag.NewFlagSet("hub agent "+args[1], flag.ExitOnError)
		o := hubFlags(fs, false)
		_ = fs.Parse(args[2:])
		return cliHubAgent(o, args[1], fs.Args())
	case "healthcheck":
		addr := envOr("NIMDEPLOY_HUB_LISTEN", hubDefaultListen)
		if len(args) > 1 {
			addr = args[1]
		}
		if host, port, err := net.SplitHostPort(addr); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		c := &http.Client{Timeout: 5 * time.Second}
		resp, err := c.Get("http://" + addr + "/healthz")
		if err != nil {
			fmt.Fprintln(os.Stderr, "hub:", err)
			return 1
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintln(os.Stderr, "hub: healthz", resp.Status)
			return 1
		}
		return 0
	case "-h", "--help", "help":
		fmt.Print(hubUsage)
		return 0
	}
	fmt.Fprint(os.Stderr, hubUsage)
	return 2
}

func cliHubAgent(o *hubOptions, cmd string, args []string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := o.open(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hub:", err)
		return 1
	}
	switch {
	case cmd == "add" && len(args) == 1:
		token, err := s.addAgent(ctx, args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, "hub:", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "Agent %s created. In its server's config:\n\n  [hub]\n  url = \"https://<this hub>\"\n  agent = %q\n  token_env = \"NIMDEPLOY_HUB_TOKEN\"\n\nand in its secrets.env (shown once):\n\n", args[0], args[0])
		fmt.Printf("NIMDEPLOY_HUB_TOKEN=%s\n", token)
		return 0
	case cmd == "revoke" && len(args) == 1:
		if err := s.revokeAgent(ctx, args[0]); err != nil {
			fmt.Fprintln(os.Stderr, "hub:", err)
			return 1
		}
		fmt.Printf("agent %s revoked\n", args[0])
		return 0
	case cmd == "list" && len(args) == 0:
		agents, err := s.agents(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hub:", err)
			return 1
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "AGENT\tSTATE\tHOST\tVERSION\tLAST SEEN\tOUTBOX")
		now := time.Now()
		for _, a := range agents {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", a.Name, a.stateText(now), dash(a.Host), dash(a.Version), dash(agoText(a.LastSeen, now)), a.Outbox)
		}
		_ = tw.Flush()
		return 0
	}
	fmt.Fprint(os.Stderr, hubUsage)
	return 2
}

func (a hubAgentRow) stateText(now time.Time) string {
	switch {
	case a.RevokedAt != "":
		return "revoked"
	case a.LastSeen.IsZero():
		return "never seen"
	case a.online(now):
		return "online"
	}
	return "offline"
}

// StateText is stateText for templates.
func (a hubAgentRow) StateText() string { return a.stateText(time.Now()) }

func agoText(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func hubServe(o *hubOptions) int {
	o.uiToken = os.Getenv(hubTokenEnv)
	if o.uiToken != "" && len(o.uiToken) < 16 {
		log.Printf("hub: $%s must be at least 16 characters", hubTokenEnv)
		return 1
	}
	if o.uiToken == "" && o.TrustedHeader == "" && !o.NoAuth && !isLoopbackListen(o.Listen) {
		log.Printf("hub: the dashboard would be open to anyone who reaches %s: set $%s, -trusted-header, or -no-auth", o.Listen, hubTokenEnv)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, err := o.open(ctx)
	if err != nil {
		log.Printf("hub: %v", err)
		return 1
	}
	h := &hubServer{store: store, opts: o, started: time.Now()}

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if err := store.prune(ctx, o.RetainDays); err != nil && ctx.Err() == nil {
				log.Printf("hub: prune: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	srv := &http.Server{Addr: o.Listen, Handler: h.routes(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: time.Minute, WriteTimeout: time.Minute, IdleTimeout: 2 * time.Minute}
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		log.Printf("hub: listen %s: %v", o.Listen, err)
		return 1
	}
	auth := "token"
	switch {
	case o.uiToken != "" && o.TrustedHeader != "":
		auth = "token or " + o.TrustedHeader
	case o.TrustedHeader != "":
		auth = o.TrustedHeader
	case o.uiToken == "":
		auth = "none"
	}
	log.Printf("nimdeploy %s hub listening on %s (database %s, dashboard auth: %s, retain %d days)", version, o.Listen, redactURL(o.DatabaseURL), auth, o.RetainDays)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("hub: %v", err)
		return 1
	}
	return 0
}

func isLoopbackListen(addr string) bool {
	host, _, _ := net.SplitHostPort(addr)
	ip, err := netip.ParseAddr(host)
	return host == "localhost" || (err == nil && ip.IsLoopback())
}

func redactURL(raw string) string {
	if i := strings.Index(raw, "@"); i >= 0 {
		if j := strings.Index(raw, "://"); j >= 0 && j < i {
			return raw[:j+3] + "***" + raw[i:]
		}
	}
	return raw
}

type hubServer struct {
	store   *hubStore
	opts    *hubOptions
	started time.Time
}

func (h *hubServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+hubEventsPath, h.handleEvents)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := h.store.db.Query(ctx, "SELECT 1"); err != nil {
			writeError(w, http.StatusServiceUnavailable, "database: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})

	mux.HandleFunc("GET /login", h.handleLoginPage)
	mux.HandleFunc("POST /login", h.handleLogin)
	mux.HandleFunc("POST /logout", h.handleLogout)

	mux.HandleFunc("GET /{$}", h.ui(h.pageOverview))
	mux.HandleFunc("GET /agents", h.ui(h.pageAgents))
	mux.HandleFunc("GET /d/{agent}/{deploy}", h.ui(h.pageDeploy))
	mux.HandleFunc("GET /static/hub.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = io.WriteString(w, hubCSS)
	})

	mux.HandleFunc("GET /api/v1/deploys", h.api(h.apiDeploys))
	mux.HandleFunc("GET /api/v1/agents", h.api(h.apiAgents))
	mux.HandleFunc("GET /api/v1/events", h.api(h.apiEvents))
	mux.HandleFunc("POST /api/v1/agents", h.bearerOnly(h.apiAddAgent))
	mux.HandleFunc("DELETE /api/v1/agents/{name}", h.bearerOnly(h.apiRevokeAgent))
	mux.HandleFunc("GET /metrics", h.api(h.handleMetrics))
	return hubHeaders(mux)
}

func hubHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// --- agents' endpoint -------------------------------------------------------------------------

func verifyHubSignature(token, ts string, body []byte, sig string, now time.Time) error {
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("bad timestamp")
	}
	if skew := now.Sub(time.Unix(n, 0)); skew > hubMaxClockSkew || skew < -hubMaxClockSkew {
		return fmt.Errorf("timestamp off by %s: check the server's clock", skew.Round(time.Second))
	}
	if !hmac.Equal([]byte(sig), []byte(signHub([]byte(token), ts, body))) {
		return errors.New("bad signature")
	}
	return nil
}

func (h *hubServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	agent := r.Header.Get(hubAgentHeader)
	if !agentNameRe.MatchString(agent) {
		writeError(w, http.StatusUnauthorized, "missing or bad "+hubAgentHeader)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hubMaxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	token, err := h.store.agentToken(r.Context(), agent)
	if err != nil {
		if strings.HasPrefix(err.Error(), "libsql") {
			log.Printf("hub: %v", err)
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		log.Printf("hub: refused events from %s (%s): unknown or revoked agent", agent, clientAddr(r))
		writeError(w, http.StatusUnauthorized, "unknown or revoked agent")
		return
	}
	if err := verifyHubSignature(token, r.Header.Get(hubTimestampHeader), body, r.Header.Get(hubSignatureHeader), time.Now()); err != nil {
		log.Printf("hub: refused events from %s (%s): %v", agent, clientAddr(r), err)
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	var batch hubBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if len(batch.Events) > hubMaxEvents {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("at most %d events per request", hubMaxEvents))
		return
	}
	events := make([]hubEvent, 0, len(batch.Events))
	for _, ev := range batch.Events {
		if ev, ok := sanitizeHubEvent(ev); ok {
			events = append(events, ev)
		}
	}
	if err := h.store.ingest(r.Context(), agent, events); err != nil {
		log.Printf("hub: storing events from %s: %v", agent, err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": len(events)})
}

func clientAddr(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.TrimSpace(strings.Split(ip, ",")[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return truncate(s, n)
}

// sanitizeHubEvent bounds what an agent can store; unknown types are
// skipped so older hubs accept newer agents.
func sanitizeHubEvent(ev hubEvent) (hubEvent, bool) {
	switch ev.Type {
	case hubEventDeployStart, hubEventDeployDone, hubEventRejected, hubEventHeartbeat:
	default:
		return ev, false
	}
	if ev.ID == "" || len(ev.ID) > 100 || ev.Time.IsZero() {
		return ev, false
	}
	ev.Deploy = clip(ev.Deploy, 128)
	ev.Host = clip(ev.Host, 255)
	ev.Version = clip(ev.Version, 64)
	ev.Reason = clip(ev.Reason, 2000)
	if len(ev.LogTail) > hubMaxLogLines {
		ev.LogTail = ev.LogTail[len(ev.LogTail)-hubMaxLogLines:]
	}
	for i, l := range ev.LogTail {
		ev.LogTail[i] = clip(l, hubMaxLineLen)
	}
	if ev.State != nil {
		st := *ev.State
		st.Log = ""
		st.Params = nil
		st.Error = clip(st.Error, 2000)
		st.Labels = cleanLabels(st.Labels)
		ev.State = &st
	}
	if inv := ev.Inventory; inv != nil {
		inv.Labels = cleanLabels(inv.Labels)
		if len(inv.Deploys) > 1000 {
			inv.Deploys = inv.Deploys[:1000]
		}
		for i := range inv.Deploys {
			d := &inv.Deploys[i]
			d.Name = clip(d.Name, 128)
			d.Labels = cleanLabels(d.Labels)
			d.State.Log, d.State.Params = "", nil
			d.State.Labels = d.Labels
		}
	}
	return ev, true
}

// cleanLabels keeps the labels a valid config could have.
func cleanLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if len(out) < maxLabels && validateLabels("", map[string]string{k: v}) == nil {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- dashboard auth -------------------------------------------------------------------------------

func (h *hubServer) sessionValue(exp int64) string {
	mac := hmac.New(sha256.New, []byte(h.opts.uiToken))
	fmt.Fprintf(mac, "nimdeploy-hub-session.%d", exp)
	return fmt.Sprintf("%d.%s", exp, hex.EncodeToString(mac.Sum(nil)))
}

func (h *hubServer) validSession(r *http.Request) bool {
	c, err := r.Cookie(hubSessionCookie)
	if err != nil || h.opts.uiToken == "" {
		return false
	}
	expText, _, _ := strings.Cut(c.Value, ".")
	exp, err := strconv.ParseInt(expText, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(h.sessionValue(exp)))
}

func (h *hubServer) validBearer(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && h.opts.uiToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(h.opts.uiToken)) == 1
}

// viewer says who may see the dashboard: "" means nobody.
func (h *hubServer) viewer(r *http.Request) string {
	if h.opts.TrustedHeader != "" {
		if v := strings.TrimSpace(r.Header.Get(h.opts.TrustedHeader)); v != "" {
			return v
		}
	}
	if h.validBearer(r) || h.validSession(r) {
		return "token"
	}
	if h.opts.uiToken == "" && h.opts.TrustedHeader == "" {
		return "anonymous"
	}
	return ""
}

func (h *hubServer) ui(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who := h.viewer(r)
		if who == "" {
			if h.opts.uiToken == "" {
				http.Error(w, "forbidden: this hub only accepts requests through its authenticating proxy", http.StatusForbidden)
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		next(w, r, who)
	}
}

func (h *hubServer) api(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.viewer(r) == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		next(w, r)
	}
}

// bearerOnly guards changes: a browser session or proxy header isn't enough,
// so a page on another site can't make the browser do them.
func (h *hubServer) bearerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.opts.uiToken == "" {
			writeError(w, http.StatusForbidden, "disabled: set $"+hubTokenEnv+" on the hub to manage agents through the API")
			return
		}
		if !h.validBearer(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		next(w, r)
	}
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func (h *hubServer) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if h.opts.uiToken == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.render(w, http.StatusOK, "login", map[string]any{"Next": safeNext(r.URL.Query().Get("next"))})
}

func (h *hubServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if h.opts.uiToken == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	next := safeNext(r.FormValue("next"))
	if subtle.ConstantTimeCompare([]byte(r.FormValue("token")), []byte(h.opts.uiToken)) != 1 {
		log.Printf("hub: failed login from %s", clientAddr(r))
		time.Sleep(time.Second)
		h.render(w, http.StatusUnauthorized, "login", map[string]any{"Next": next, "Error": "Wrong token"})
		return
	}
	exp := time.Now().Add(hubSessionMaxAge).Unix()
	http.SetCookie(w, &http.Cookie{Name: hubSessionCookie, Value: h.sessionValue(exp), Path: "/", MaxAge: int(hubSessionMaxAge.Seconds()),
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (h *hubServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: hubSessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- views --------------------------------------------------------------------------------------------

// hubView is a deploy as the dashboard shows it.
type hubView struct {
	hubDeployRow
	AgentOnline bool
	Client, Env string
	When        time.Time
}

func (v hubView) Title() string { return labelTitle(v.Labels, v.Deploy) }

type hubFilter struct {
	Client, Env, Agent, Status string
}

func (f hubFilter) match(v hubView) bool {
	return (f.Client == "" || v.Client == f.Client) && (f.Env == "" || v.Env == f.Env) &&
		(f.Agent == "" || v.Agent == f.Agent) && (f.Status == "" || v.State.Status == f.Status)
}

func (f hubFilter) Active() bool { return f != (hubFilter{}) }

func (h *hubServer) views(ctx context.Context) ([]hubView, []hubAgentRow, error) {
	deps, err := h.store.deploys(ctx)
	if err != nil {
		return nil, nil, err
	}
	agents, err := h.store.agents(ctx)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	online := map[string]bool{}
	for _, a := range agents {
		online[a.Name] = a.RevokedAt == "" && a.online(now)
	}
	out := make([]hubView, 0, len(deps))
	for _, d := range deps {
		if !d.InInventory && d.State.Status == "" {
			continue
		}
		v := hubView{hubDeployRow: d, AgentOnline: online[d.Agent], Client: d.Labels["client"], Env: d.Labels["environment"], When: d.UpdatedAt}
		if v.State.FinishedAt != nil {
			v.When = *v.State.FinishedAt
		} else if v.State.StartedAt != nil {
			v.When = *v.State.StartedAt
		}
		out = append(out, v)
	}
	return out, agents, nil
}

var envOrder = map[string]int{"dev": 1, "development": 1, "test": 2, "testing": 2, "qa": 3, "stage": 4, "staging": 4,
	"preprod": 5, "pre": 5, "uat": 5, "prod": 6, "production": 6}

func sortEnvs(envs []string) {
	sort.SliceStable(envs, func(i, j int) bool {
		a, b := envOrder[strings.ToLower(envs[i])], envOrder[strings.ToLower(envs[j])]
		if a == 0 {
			a = 99
		}
		if b == 0 {
			b = 99
		}
		if a != b {
			return a < b
		}
		return envs[i] < envs[j]
	})
}

type hubMatrix struct {
	Clients []string
	Envs    []string
	Cells   map[string]map[string][]hubView // client → env → deploys
}

func buildMatrix(views []hubView) hubMatrix {
	m := hubMatrix{Cells: map[string]map[string][]hubView{}}
	clients, envs := map[string]bool{}, map[string]bool{}
	for _, v := range views {
		clients[v.Client], envs[v.Env] = true, true
		if m.Cells[v.Client] == nil {
			m.Cells[v.Client] = map[string][]hubView{}
		}
		m.Cells[v.Client][v.Env] = append(m.Cells[v.Client][v.Env], v)
	}
	for c := range clients {
		m.Clients = append(m.Clients, c)
	}
	for e := range envs {
		m.Envs = append(m.Envs, e)
	}
	sort.Slice(m.Clients, func(i, j int) bool { // unlabelled last
		if (m.Clients[i] == "") != (m.Clients[j] == "") {
			return m.Clients[j] == ""
		}
		return strings.ToLower(m.Clients[i]) < strings.ToLower(m.Clients[j])
	})
	sortEnvs(m.Envs)
	if len(m.Envs) > 0 && m.Envs[0] == "" { // unlabelled last
		m.Envs = append(m.Envs[1:], "")
	}
	return m
}

func (h *hubServer) pageOverview(w http.ResponseWriter, r *http.Request, who string) {
	views, agents, err := h.views(r.Context())
	if err != nil {
		h.renderError(w, err)
		return
	}
	q := r.URL.Query()
	f := hubFilter{Client: q.Get("client"), Env: q.Get("environment"), Agent: q.Get("agent"), Status: q.Get("status")}
	var shown []hubView
	failing, running := 0, 0
	for _, v := range views {
		if !f.match(v) {
			continue
		}
		shown = append(shown, v)
		switch v.State.Status {
		case StatusFailed:
			failing++
		case StatusRunning, StatusWaiting:
			running++
		}
	}
	now := time.Now()
	online, offline := 0, 0
	for _, a := range agents {
		if a.RevokedAt != "" || a.LastSeen.IsZero() {
			continue
		}
		if a.online(now) {
			online++
		} else {
			offline++
		}
	}
	events, err := h.store.events(r.Context(), f.Agent, "", "", 40)
	if err != nil {
		h.renderError(w, err)
		return
	}
	labelsOf := map[[2]string]map[string]string{}
	for _, v := range views {
		labelsOf[[2]string{v.Agent, v.Deploy}] = v.Labels
	}
	var timeline []hubEventRow
	for _, e := range events {
		if l := labelsOf[[2]string{e.Agent, e.Deploy}]; l != nil {
			e.Labels = l
		}
		if (f.Client == "" || e.Labels["client"] == f.Client) && (f.Env == "" || e.Labels["environment"] == f.Env) {
			timeline = append(timeline, e)
		}
		if len(timeline) == 15 {
			break
		}
	}
	allDeps := make([]hubDeployRow, len(views))
	for i, v := range views {
		allDeps[i] = v.hubDeployRow
	}
	agentNames := make([]string, 0, len(agents))
	for _, a := range agents {
		agentNames = append(agentNames, a.Name)
	}
	envs := labelValues(allDeps, "environment")
	sortEnvs(envs)
	h.render(w, http.StatusOK, "overview", map[string]any{
		"Who": who, "Filter": f, "Matrix": buildMatrix(shown), "Deploys": shown, "Timeline": timeline,
		"Clients": labelValues(allDeps, "client"), "Envs": envs, "AgentNames": agentNames,
		"Statuses": []string{StatusFailed, StatusRunning, StatusSuccess, StatusSkipped, StatusInterrupted, StatusNever},
		"Failing":  failing, "Running": running, "Online": online, "Offline": offline, "Total": len(shown),
	})
}

func (h *hubServer) pageAgents(w http.ResponseWriter, r *http.Request, who string) {
	views, agents, err := h.views(r.Context())
	if err != nil {
		h.renderError(w, err)
		return
	}
	count := map[string]int{}
	for _, v := range views {
		if v.InInventory {
			count[v.Agent]++
		}
	}
	h.render(w, http.StatusOK, "agents", map[string]any{"Who": who, "Agents": agents, "Count": count})
}

func (h *hubServer) pageDeploy(w http.ResponseWriter, r *http.Request, who string) {
	agent, deploy := r.PathValue("agent"), r.PathValue("deploy")
	views, _, err := h.views(r.Context())
	if err != nil {
		h.renderError(w, err)
		return
	}
	var view *hubView
	for i := range views {
		if views[i].Agent == agent && views[i].Deploy == deploy {
			view = &views[i]
		}
	}
	if view == nil {
		h.render(w, http.StatusNotFound, "error", map[string]any{"Who": who, "Error": "No deploy " + deploy + " on " + agent})
		return
	}
	events, err := h.store.events(r.Context(), agent, deploy, "", 100)
	if err != nil {
		h.renderError(w, err)
		return
	}
	h.render(w, http.StatusOK, "deploy", map[string]any{"Who": who, "D": view, "Events": events})
}

func (h *hubServer) renderError(w http.ResponseWriter, err error) {
	log.Printf("hub: %v", err)
	h.render(w, http.StatusServiceUnavailable, "error", map[string]any{"Error": "The database is not answering: " + err.Error()})
}

func (h *hubServer) render(w http.ResponseWriter, code int, page string, data map[string]any) {
	data["Version"] = version
	data["Page"] = page
	data["Now"] = time.Now()
	data["Auth"] = h.opts.uiToken != ""
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(code)
	if err := hubTemplates.ExecuteTemplate(w, page, data); err != nil {
		log.Printf("hub: template %s: %v", page, err)
	}
}

// --- JSON API --------------------------------------------------------------------------------------------

type hubDeployJSON struct {
	Agent       string            `json:"agent"`
	AgentOnline bool              `json:"agent_online"`
	Deploy      string            `json:"deploy"`
	Provider    string            `json:"provider,omitempty"`
	Repository  string            `json:"repository,omitempty"`
	Branch      string            `json:"branch,omitempty"`
	Schedule    string            `json:"schedule,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	State       State             `json:"state"`
	Removed     bool              `json:"removed,omitempty"` // no longer in the agent's config
}

func (h *hubServer) apiDeploys(w http.ResponseWriter, r *http.Request) {
	filter, err := parseLabelFilter(r.URL.Query()["label"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	views, _, err := h.views(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	out := []hubDeployJSON{}
	agent := r.URL.Query().Get("agent")
	for _, v := range views {
		if (agent != "" && v.Agent != agent) || !filter.match(v.Labels) {
			continue
		}
		out = append(out, hubDeployJSON{Agent: v.Agent, AgentOnline: v.AgentOnline, Deploy: v.Deploy, Provider: v.Provider,
			Repository: v.Repository, Branch: v.Branch, Schedule: v.Schedule, Labels: v.Labels, State: v.State, Removed: !v.InInventory})
	}
	writeJSON(w, http.StatusOK, out)
}

type hubAgentJSON struct {
	Name      string            `json:"name"`
	State     string            `json:"state"`
	Host      string            `json:"host,omitempty"`
	Version   string            `json:"version,omitempty"`
	LastSeen  *time.Time        `json:"last_seen,omitempty"`
	StartedAt *time.Time        `json:"started_at,omitempty"`
	Outbox    int64             `json:"outbox"`
	Labels    map[string]string `json:"labels,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (h *hubServer) apiAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := h.store.agents(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	now := time.Now()
	out := []hubAgentJSON{}
	for _, a := range agents {
		out = append(out, hubAgentJSON{Name: a.Name, State: a.stateText(now), Host: a.Host, Version: a.Version,
			LastSeen: timePtr(a.LastSeen), StartedAt: timePtr(a.StartedAt), Outbox: a.Outbox, Labels: a.Labels})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *hubServer) apiEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	events, err := h.store.events(r.Context(), q.Get("agent"), q.Get("deploy"), q.Get("type"), limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	out := make([]hubEvent, 0, len(events))
	for _, e := range events {
		out = append(out, e.hubEvent)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *hubServer) apiAddAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "want {\"name\": \"...\"}")
		return
	}
	token, err := h.store.addAgent(r.Context(), req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("hub: agent %s created (new token) from %s", req.Name, clientAddr(r))
	writeJSON(w, http.StatusOK, map[string]string{"name": req.Name, "token": token})
}

func (h *hubServer) apiRevokeAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.store.revokeAgent(r.Context(), name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	log.Printf("hub: agent %s revoked from %s", name, clientAddr(r))
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "state": "revoked"})
}

// --- metrics ------------------------------------------------------------------------------------------------

func promLabels(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", pairs[i], pairs[i+1]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func (h *hubServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	views, agents, err := h.views(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	var b strings.Builder
	family := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	ts := func(t time.Time) string { return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64) }
	now := time.Now()

	family("nimdeploy_hub_build_info", "gauge", "Version of the hub, always 1.")
	fmt.Fprintf(&b, "nimdeploy_hub_build_info%s 1\n", promLabels("version", version))
	family("nimdeploy_hub_agent_up", "gauge", "1 if the agent sent a heartbeat recently.")
	for _, a := range agents {
		if a.RevokedAt != "" {
			continue
		}
		up := 0
		if a.online(now) {
			up = 1
		}
		fmt.Fprintf(&b, "nimdeploy_hub_agent_up%s %d\n", promLabels("agent", a.Name, "version", a.Version), up)
	}
	family("nimdeploy_hub_agent_last_seen_timestamp_seconds", "gauge", "Last time the agent reached the hub (Unix time).")
	family("nimdeploy_hub_agent_outbox_events", "gauge", "Events waiting on the agent to be sent.")
	for _, a := range agents {
		if a.RevokedAt == "" && !a.LastSeen.IsZero() {
			fmt.Fprintf(&b, "nimdeploy_hub_agent_last_seen_timestamp_seconds%s %s\n", promLabels("agent", a.Name), ts(a.LastSeen))
			fmt.Fprintf(&b, "nimdeploy_hub_agent_outbox_events%s %d\n", promLabels("agent", a.Name), a.Outbox)
		}
	}

	family("nimdeploy_hub_deploy_info", "gauge", "Labels of each deploy, always 1: join on agent and deploy.")
	for _, v := range views {
		if !v.InInventory {
			continue
		}
		pairs := []string{"agent", v.Agent, "deploy", v.Deploy}
		for _, k := range sortedKeys(v.Labels) {
			if k != "agent" {
				pairs = append(pairs, k, v.Labels[k])
			}
		}
		fmt.Fprintf(&b, "nimdeploy_hub_deploy_info%s 1\n", promLabels(pairs...))
	}
	family("nimdeploy_hub_deploy_status", "gauge", "Current status of each deploy: 1 for the status it is in.")
	family("nimdeploy_hub_deploy_last_finished_timestamp_seconds", "gauge", "When the last deploy finished (Unix time).")
	for _, v := range views {
		if !v.InInventory {
			continue
		}
		for _, s := range []string{StatusSuccess, StatusFailed, StatusRunning, StatusWaiting, StatusSkipped, StatusInterrupted, StatusNever} {
			n := 0
			if v.State.Status == s {
				n = 1
			}
			fmt.Fprintf(&b, "nimdeploy_hub_deploy_status%s %d\n", promLabels("agent", v.Agent, "deploy", v.Deploy, "status", s), n)
		}
		if v.State.FinishedAt != nil {
			fmt.Fprintf(&b, "nimdeploy_hub_deploy_last_finished_timestamp_seconds%s %s\n", promLabels("agent", v.Agent, "deploy", v.Deploy), ts(*v.State.FinishedAt))
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}
