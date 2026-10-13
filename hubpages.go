package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The dashboard: server-rendered pages, htmx for navigation, live panels,
// the add-agent drawer and commands. Everything a page shows is computed
// here; the templates in hubui.go only lay it out.

// ---------- page frame ----------

type hubPage struct {
	Lang, Who, Version, Page, CSRF, Path string
	Auth                                 bool
	Side                                 sideData
	Crumbs                               []crumb
	Data                                 any
}

type crumb struct{ Label, Href string }

type sideData struct {
	Clients               []sideClient
	AttnCount, AgentCount int
	Langs                 []langLink
}

type sideClient struct {
	Name, Key, Color, Initials, Href string
	Open, On                         bool
	Failing                          int
	Envs                             []sideEnv
}

type sideEnv struct {
	Name, Href, Dot string
	On              bool
	Pips            []pip
}

type pip struct{ Cls, Title string }

type langLink struct {
	Code, Href string
	On         bool
}

// hubData is what one request reads from the store, shared by the sidebar and the page.
type hubData struct {
	views  []hubView
	agents []hubAgentRow
	recent map[[2]string][]string
}

func (h *hubServer) load(ctx context.Context) (hubData, error) {
	views, agents, err := h.views(ctx)
	if err != nil {
		return hubData{}, err
	}
	recent, err := h.store.recentStatuses(ctx, 10)
	if err != nil {
		return hubData{}, err
	}
	return hubData{views: views, agents: agents, recent: recent}, nil
}

var avatarColors = []string{"#e4b250", "#7cb0ea", "#56c487", "#f2837a", "#c7a6f0", "#6fd0c6"}

func colorFor(name string) string {
	sum := 0
	for _, r := range name {
		sum += int(r)
	}
	return avatarColors[sum%len(avatarColors)]
}

func initialsOf(name string) string {
	f := strings.FieldsFunc(name, func(r rune) bool { return r == ' ' || r == '-' || r == '_' || r == '.' })
	out := ""
	for _, w := range f {
		out += strings.ToUpper(string([]rune(w)[0]))
		if len([]rune(out)) == 2 {
			break
		}
	}
	if out == "" {
		return "?"
	}
	return out
}

func statusCls(s string) string {
	switch s {
	case StatusSuccess:
		return "ok"
	case StatusFailed, StatusInterrupted:
		return "bad"
	case StatusRunning:
		return "run"
	case StatusWaiting, "queued":
		return "wait"
	}
	return ""
}

func clientHref(c, env string) string {
	if c == "" {
		c = "-"
	}
	u := "/c/" + url.PathEscape(c)
	if env != "" {
		u += "/" + url.PathEscape(env)
	}
	return u
}

func deployHref(agent, deploy string) string {
	return "/d/" + url.PathEscape(agent) + "/" + url.PathEscape(deploy)
}

// clientsOf lists clients (sorted, unlabelled last) and their environments.
func (d hubData) clientsOf() ([]string, map[string][]string) {
	envs := map[string]map[string]bool{}
	add := func(c, e string) {
		if envs[c] == nil {
			envs[c] = map[string]bool{}
		}
		if e != "" {
			envs[c][e] = true
		}
	}
	for _, v := range d.views {
		add(v.Client, v.Env)
	}
	for _, a := range d.agents {
		if a.RevokedAt == "" {
			add(a.ClientName(), a.EnvName())
		}
	}
	var clients []string
	out := map[string][]string{}
	for c, set := range envs {
		clients = append(clients, c)
		for e := range set {
			out[c] = append(out[c], e)
		}
		sortEnvs(out[c])
	}
	sort.Slice(clients, func(i, j int) bool {
		if (clients[i] == "") != (clients[j] == "") {
			return clients[j] == ""
		}
		return strings.ToLower(clients[i]) < strings.ToLower(clients[j])
	})
	return clients, out
}

func agentDot(a *hubAgentRow, now time.Time) string {
	switch {
	case a == nil:
		return ""
	case a.LastSeen.IsZero():
		return "pending"
	case a.online(now):
		return "up"
	}
	return "down"
}

// agentWhere names an agent's client and environment for a subtitle.
func agentWhere(lang string, a hubAgentRow) string {
	c := firstNonEmpty(a.ClientName(), hubT(lang, "no_client"))
	if e := a.EnvName(); e != "" {
		return c + " · " + e
	}
	return c
}

// errLine reports whether a log line reads as an error. The run's own header lines, such as the command, never do.
func errLine(s string) bool {
	if i := strings.IndexByte(s, ' '); i > 0 {
		if _, err := time.Parse(time.RFC3339, s[:i]); err == nil {
			s = s[i+1:]
		}
	}
	if strings.HasPrefix(s, "command=") {
		return false
	}
	return strings.Contains(s, "ERROR") || strings.HasPrefix(s, "fatal") || strings.Contains(s, "error=")
}

func (h *hubServer) side(r *http.Request, d hubData, curClient, curEnv string) sideData {
	lang := hubLang(r)
	now := time.Now()
	clients, envs := d.clientsOf()
	var out sideData
	// Overview-style pages pass no client; only client and deploy pages mark one, even the unnamed one.
	inTree := strings.HasPrefix(r.URL.Path, "/c/") || strings.HasPrefix(r.URL.Path, "/d/")
	for _, c := range clients {
		name := c
		if name == "" {
			name = hubT(lang, "no_client")
		}
		sc := sideClient{Name: name, Key: strings.ToLower(name), Color: colorFor(name), Initials: initialsOf(name), Href: clientHref(c, ""),
			On: inTree && curClient == c && curEnv == "", Open: inTree && curClient == c || len(clients) <= 3}
		for _, e := range envs[c] {
			se := sideEnv{Name: e, Href: clientHref(c, e), On: inTree && curClient == c && curEnv == e}
			for i := range d.agents {
				a := &d.agents[i]
				if a.RevokedAt == "" && a.ClientName() == c && a.EnvName() == e {
					se.Dot = agentDot(a, now)
				}
			}
			for _, v := range d.views {
				if v.Client == c && v.Env == e && v.InInventory {
					se.Pips = append(se.Pips, pip{Cls: statusCls(v.State.Status), Title: v.Deploy + ": " + hubT(lang, "st."+orNever(v.State.Status))})
					sc.Key += " " + strings.ToLower(v.Deploy)
				}
			}
			sc.Envs = append(sc.Envs, se)
		}
		for _, v := range d.views {
			if v.Client == c && v.State.Status == StatusFailed && v.InInventory {
				sc.Failing++
			}
		}
		out.Clients = append(out.Clients, sc)
	}
	out.AttnCount = len(h.attention(lang, d))
	for _, a := range d.agents {
		if a.RevokedAt == "" {
			out.AgentCount++
		}
	}
	for _, l := range hubLangs {
		out.Langs = append(out.Langs, langLink{Code: l, Href: "/lang/" + l + "?next=" + url.QueryEscape(r.URL.RequestURI()), On: l == lang})
	}
	return out
}

func orNever(s string) string {
	if s == "" {
		return StatusNever
	}
	return s
}

// csrfToken returns the request's CSRF token, setting its cookie when new.
func csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("nd_csrf"); err == nil && len(c.Value) == 32 {
		return c.Value
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: "nd_csrf", Value: tok, Path: "/", HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode})
	return tok
}

// checkCSRF guards dashboard changes: the token from the page must match the cookie.
func checkCSRF(r *http.Request) bool {
	c, err := r.Cookie("nd_csrf")
	got := r.Header.Get("X-CSRF-Token")
	return err == nil && got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(c.Value)) == 1
}

// page renders a full page (or, for htmx requests, the same page: htmx picks what it needs).
func (h *hubServer) page(w http.ResponseWriter, r *http.Request, who, name string, d hubData, curClient, curEnv string, crumbs []crumb, data any) {
	p := hubPage{Lang: hubLang(r), Who: who, Version: version, Page: name, CSRF: csrfToken(w, r), Path: r.URL.RequestURI(),
		Auth: h.opts.uiToken != "", Side: h.side(r, d, curClient, curEnv), Crumbs: crumbs, Data: data}
	h.renderTemplate(w, http.StatusOK, "page", p)
}

func (h *hubServer) renderTemplate(w http.ResponseWriter, code int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(code)
	if err := hubTemplates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("hub: template %s: %v", name, err)
	}
}

// render is kept for the login page.
func (h *hubServer) render(w http.ResponseWriter, code int, page string, data map[string]any) {
	h.renderTemplate(w, code, page, data)
}

func (h *hubServer) renderError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("hub: %v", err)
	h.renderTemplate(w, http.StatusServiceUnavailable, "error", map[string]any{"Lang": hubLang(r), "Error": hubT(hubLang(r), "err.db", err.Error())})
}

// ---------- attention ----------

type attnItem struct{ Sev, Title, Desc, Ago, Href string }

func (h *hubServer) attention(lang string, d hubData) []attnItem {
	now := time.Now()
	var out []attnItem
	for _, v := range d.views {
		if v.State.Status == StatusFailed && v.InInventory {
			out = append(out, attnItem{Sev: "bad", Title: hubT(lang, "attn.failed", v.Deploy),
				Desc: strings.Join(nonEmpty(v.Client, v.Env, v.State.Error), " · "), Ago: agoShort(lang, now.Sub(v.When)), Href: deployHref(v.Agent, v.Deploy)})
		}
	}
	for _, a := range d.agents {
		if a.RevokedAt != "" {
			continue
		}
		switch {
		case a.LastSeen.IsZero():
			out = append(out, attnItem{Sev: "warn", Title: hubT(lang, "attn.pending", a.Name), Desc: hubT(lang, "attn.pending_d", agentWhere(lang, a)), Href: "/agents#agent-" + a.Name})
		case !a.online(now):
			out = append(out, attnItem{Sev: "bad", Title: hubT(lang, "attn.offline", a.Name), Desc: hubT(lang, "attn.offline_d", a.ClientName(), agoIn(lang, now.Sub(a.LastSeen)), a.Outbox),
				Ago: agoShort(lang, now.Sub(a.LastSeen)), Href: "/agents#agent-" + a.Name})
		}
		if a.Version != "" && a.Version != version && version != "dev" && olderVersion(a.Version, version) {
			out = append(out, attnItem{Sev: "warn", Title: hubT(lang, "attn.old", a.Name, a.Version), Desc: hubT(lang, "attn.old_d", version), Href: "/agents#agent-" + a.Name})
		}
		if a.RequireCert && !a.CertNotAfter.IsZero() && time.Until(a.CertNotAfter) < certExpiringSoon {
			out = append(out, attnItem{Sev: "warn", Title: hubT(lang, "attn.cert", a.Name, a.CertNotAfter.Format("2006-01-02")), Desc: hubT(lang, "attn.cert_d"), Href: "/agents#agent-" + a.Name})
		}
	}
	return out
}

func nonEmpty(s ...string) []string {
	var out []string
	for _, x := range s {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// olderVersion compares "v0.9.1"-style versions.
func olderVersion(a, b string) bool {
	parse := func(v string) [3]int {
		var n [3]int
		for i, p := range strings.SplitN(strings.TrimPrefix(strings.SplitN(v, "-", 2)[0], "v"), ".", 3) {
			n[i], _ = strconv.Atoi(p)
		}
		return n
	}
	x, y := parse(a), parse(b)
	for i := range x {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}

// ---------- overview ----------

type kpi struct {
	Label, Value, Unit, Delta, DeltaCls, Cls string
	Spark                                    template.HTML
}

type mxCell struct {
	Empty         bool
	Apps, Failing string
	Chips         []chip
}

type chip struct {
	Name, Href, Cls, Title string
	Stale                  bool
}

type mxRow struct {
	Client, Color, Initials, Href string
	Cells                         []mxCell
}

type tlItem struct {
	Ago, Node, Deploy, Href, Client, Sub string
	What                                 template.HTML
}

type envLink struct {
	Label, Href string
	On          bool
}

type overviewData struct {
	Sub      string
	EnvLinks []envLink
	KPIs     []kpi
	Chart    template.HTML
	Envs     []string
	Rows     []mxRow
	Attn     []attnItem
	Timeline []tlItem
}

func (h *hubServer) pageOverview(w http.ResponseWriter, r *http.Request, who string) {
	ctx := r.Context()
	lang := hubLang(r)
	d, err := h.load(ctx)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	daily, err := h.store.daily(ctx, 14)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	events, err := h.store.events(ctx, "", "", "", 60)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	envF := r.URL.Query().Get("env")
	clients, _ := d.clientsOf()
	now := time.Now()
	var allEnvs []string
	seen := map[string]bool{}
	for _, v := range d.views {
		if v.Env != "" && !seen[v.Env] {
			seen[v.Env] = true
			allEnvs = append(allEnvs, v.Env)
		}
	}
	sortEnvs(allEnvs)
	data := overviewData{}
	apps, servers := 0, 0
	for _, v := range d.views {
		if v.InInventory {
			apps++
		}
	}
	for _, a := range d.agents {
		if a.RevokedAt == "" {
			servers++
		}
	}
	data.Sub = hubT(lang, "ov.sub", apps, len(clients), servers)
	data.EnvLinks = append(data.EnvLinks, envLink{Label: hubT(lang, "ov.all_envs"), Href: "/", On: envF == ""})
	for _, e := range allEnvs {
		data.EnvLinks = append(data.EnvLinks, envLink{Label: e, Href: "/?env=" + url.QueryEscape(e), On: envF == e})
	}

	// KPIs
	sum := func(days []dayCount, f func(dayCount) int) int {
		n := 0
		for _, x := range days {
			n += f(x)
		}
		return n
	}
	all := func(x dayCount) int { return x.Success + x.Failed + x.Other }
	last7, prev7 := daily[7:], daily[:7]
	total7, totalPrev := sum(last7, all), sum(prev7, all)
	fails7 := sum(last7, func(x dayCount) int { return x.Failed })
	ok7 := sum(last7, func(x dayCount) int { return x.Success })
	failing, online, offline, pending := 0, 0, 0, 0
	var durs []time.Duration
	for _, v := range d.views {
		if !v.InInventory || (envF != "" && v.Env != envF) {
			continue
		}
		if v.State.Status == StatusFailed {
			failing++
		}
		if dur, err := time.ParseDuration(v.State.Duration); err == nil && v.State.Status == StatusSuccess {
			durs = append(durs, dur)
		}
	}
	for _, a := range d.agents {
		switch {
		case a.RevokedAt != "":
		case a.LastSeen.IsZero():
			pending++
		case a.online(now):
			online++
		default:
			offline++
		}
	}
	series := func(f func(dayCount) float64) []float64 {
		out := make([]float64, len(daily))
		for i, x := range daily {
			out[i] = f(x)
		}
		return out
	}
	delta := "—"
	if totalPrev > 0 {
		delta = fmt.Sprintf("%+d%%", (total7-totalPrev)*100/totalPrev)
	}
	rate := "—"
	if total7 > 0 {
		rate = fmt.Sprintf("%d%%", ok7*100/total7)
	}
	median := "—"
	if len(durs) > 0 {
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		median = formatDuration(durs[len(durs)/2])
	}
	data.KPIs = []kpi{
		{Label: hubT(lang, "kpi.deploys7"), Value: strconv.Itoa(total7), Delta: hubT(lang, "kpi.vs_prev", delta), Spark: sparkSVG(series(func(x dayCount) float64 { return float64(all(x)) }), "var(--accent)")},
		{Label: hubT(lang, "kpi.failing"), Value: strconv.Itoa(failing), Cls: map[bool]string{true: "bad"}[failing > 0], Delta: hubT(lang, "kpi.failed_week", fails7),
			DeltaCls: map[bool]string{true: "up-bad"}[fails7 > 0], Spark: sparkSVG(series(func(x dayCount) float64 { return float64(x.Failed) }), "var(--bad)")},
		{Label: hubT(lang, "kpi.success"), Value: rate, Delta: hubT(lang, "kpi.median", median), Spark: sparkSVG(series(func(x dayCount) float64 {
			if all(x) == 0 {
				return 1
			}
			return float64(x.Success) / float64(all(x))
		}), "var(--ok)")},
		{Label: hubT(lang, "kpi.online"), Value: strconv.Itoa(online), Unit: " / " + strconv.Itoa(online+offline+pending), Delta: hubT(lang, "kpi.offline_pending", offline, pending),
			DeltaCls: map[bool]string{true: "up-bad"}[offline > 0]},
	}
	data.Chart = barsSVG(lang, daily)

	// matrix
	data.Envs = allEnvs
	if envF != "" {
		data.Envs = []string{envF}
	}
	for _, c := range clients {
		name := c
		if name == "" {
			name = hubT(lang, "no_client")
		}
		row := mxRow{Client: name, Color: colorFor(name), Initials: initialsOf(name), Href: clientHref(c, "")}
		for _, e := range data.Envs {
			cell := mxCell{}
			n, bad := 0, 0
			for _, v := range d.views {
				if v.Client != c || v.Env != e || !v.InInventory {
					continue
				}
				n++
				if v.State.Status == StatusFailed {
					bad++
				}
				cell.Chips = append(cell.Chips, chip{Name: v.Deploy, Href: deployHref(v.Agent, v.Deploy), Cls: statusCls(v.State.Status), Stale: !v.AgentOnline,
					Title: v.Agent + " · " + hubT(lang, "st."+orNever(v.State.Status))})
			}
			cell.Empty = n == 0
			cell.Apps = hubT(lang, "mx.apps", n)
			if bad > 0 {
				cell.Failing = hubT(lang, "mx.failing", bad)
			}
			row.Cells = append(row.Cells, cell)
		}
		data.Rows = append(data.Rows, row)
	}
	data.Attn = h.attention(lang, d)
	data.Timeline = timelineItems(lang, d, events, 6)
	h.page(w, r, who, "overview", d, "", "", []crumb{{Label: hubT(lang, "nav.overview")}}, data)
}

func timelineItems(lang string, d hubData, events []hubEventRow, limit int) []tlItem {
	clientOf := map[[2]string]string{}
	for _, v := range d.views {
		clientOf[[2]string{v.Agent, v.Deploy}] = v.Client
	}
	now := time.Now()
	var out []tlItem
	for _, e := range events {
		it := tlItem{Ago: agoShort(lang, now.Sub(e.Time)), Deploy: e.Deploy, Href: deployHref(e.Agent, e.Deploy), Client: firstNonEmpty(clientOf[[2]string{e.Agent, e.Deploy}], e.Agent)}
		switch e.Type {
		case hubEventDeployDone:
			st := ""
			if e.State != nil {
				st = e.State.Status
				it.Sub = strings.Join(nonEmpty(refLabel(e.State.Tag, e.State.Commit), e.State.Message, e.State.Duration), " · ")
			}
			it.Node, it.What = statusCls(st), badgeHTML(lang, st)
		case hubEventDeployStart:
			it.Node, it.What = "run", template.HTML(template.HTMLEscapeString(hubT(lang, "ev.started")))
			if e.State != nil {
				it.Sub = strings.Join(nonEmpty(refLabel(e.State.Tag, e.State.Commit), e.State.Pusher), " · ")
			}
		case hubEventRejected:
			it.Node, it.What, it.Sub = "wait", template.HTML(template.HTMLEscapeString(hubT(lang, "ev.rejected"))), e.Reason
		default:
			continue
		}
		out = append(out, it)
		if len(out) == limit {
			break
		}
	}
	return out
}

func badgeHTML(lang, status string) template.HTML {
	s := orNever(status)
	return template.HTML(`<span class="badge s-` + template.HTMLEscapeString(s) + `">` + template.HTMLEscapeString(hubT(lang, "st."+s)) + `</span>`)
}

// ---------- client ----------

type appRow struct {
	Name, Kind, Href, Stripe, Commit, Msg, Schedule, By, ByInit, Ago, Took, HistTitle string
	Badge                                                                             template.HTML
	Hist                                                                              []string
}

type agentLine struct{ Name, Href, Dot, Text string }

type envCol struct {
	Name, Tag, Count string
	Prod             bool
	Apps             []appRow
	Agents           []agentLine
}

type clientData struct {
	Name, Color, Initials, Env, Summary, AddHref string
	Labels                                       []string
	Cols                                         []envCol
}

func histOf(recent []string) []string {
	out := make([]string, 10)
	for i := range out {
		j := 9 - i // oldest first; recent is newest first
		if j < len(recent) {
			out[i] = statusCls(recent[j])
		} else {
			out[i] = "none"
		}
	}
	return out
}

func kindOf(v hubView) string {
	switch {
	case v.State.Ansible != nil:
		return "ansible"
	case v.Schedule != "" && v.Provider == providerSchedule:
		return "schedule"
	}
	return v.Provider
}

func (h *hubServer) appRowOf(lang string, d hubData, v hubView) appRow {
	now := time.Now()
	row := appRow{Name: v.Deploy, Kind: kindOf(v), Href: deployHref(v.Agent, v.Deploy), Stripe: statusCls(v.State.Status), Badge: badgeHTML(lang, v.State.Status),
		Commit: refLabel(v.State.Tag, v.State.Commit), Msg: v.State.Message, Schedule: v.Schedule, By: v.State.Pusher, ByInit: initialsOf(v.State.Pusher),
		Took: v.State.Duration, Hist: histOf(d.recent[[2]string{v.Agent, v.Deploy}]), HistTitle: hubT(lang, "app.last_runs")}
	if v.State.Pusher == "" {
		row.ByInit = ""
	}
	if v.State.StartedAt != nil {
		row.Ago = hubT(lang, "app.ago", agoIn(lang, now.Sub(v.When)))
	} else {
		row.Ago = hubT(lang, "app.no_runs")
	}
	return row
}

func (h *hubServer) pageClient(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	c, _ := url.PathUnescape(r.PathValue("client"))
	if c == "-" {
		c = ""
	}
	env, _ := url.PathUnescape(r.PathValue("env"))
	d, err := h.load(r.Context())
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	_, envs := d.clientsOf()
	cols := envs[c]
	if env != "" {
		cols = []string{env}
	}
	name := c
	if name == "" {
		name = hubT(lang, "no_client")
	}
	data := clientData{Name: name, Color: colorFor(name), Initials: initialsOf(name), Env: env, AddHref: "/ui/agents/new?client=" + url.QueryEscape(c)}
	if c != "" {
		data.Labels = append(data.Labels, "client="+c)
	}
	n, bad := 0, 0
	now := time.Now()
	for _, e := range cols {
		data.Labels = append(data.Labels, "environment="+e)
		col := envCol{Name: e, Prod: e == "production" || e == "prod", Tag: e}
		if col.Prod {
			col.Tag = "prod"
		}
		for _, v := range d.views {
			if v.Client == c && v.Env == e && v.InInventory {
				col.Apps = append(col.Apps, h.appRowOf(lang, d, v))
				n++
				if v.State.Status == StatusFailed {
					bad++
				}
			}
		}
		col.Count = hubT(lang, "mx.apps", len(col.Apps))
		for i := range d.agents {
			a := &d.agents[i]
			if a.RevokedAt != "" || a.ClientName() != c || a.EnvName() != e {
				continue
			}
			line := agentLine{Name: a.Name, Href: "/agents#agent-" + a.Name, Dot: agentDot(a, now)}
			switch line.Dot {
			case "pending":
				line.Text = hubT(lang, "cl.waiting_first")
			case "up":
				line.Text = strings.Join(nonEmpty(a.Host, a.Version, hubT(lang, "cl.seen", agoIn(lang, now.Sub(a.LastSeen)))), " · ")
			default:
				line.Text = hubT(lang, "cl.offline_for", agoIn(lang, now.Sub(a.LastSeen)))
			}
			col.Agents = append(col.Agents, line)
		}
		data.Cols = append(data.Cols, col)
	}
	data.Summary = hubT(lang, "cl.apps_failing", n, bad)
	crumbs := []crumb{{Label: hubT(lang, "side.clients")}, {Label: name}}
	if env != "" {
		crumbs = []crumb{{Label: name, Href: clientHref(c, "")}, {Label: env}}
	}
	h.page(w, r, who, "client", d, c, env, crumbs, data)
}

// ---------- deploy ----------

type phaseBar struct {
	Name, Cls, Took string
	Left, Width     string
}

type hostBox struct{ Host, Outcome string }

type runRow struct {
	Ago, Commit, Took, By string
	Badge                 template.HTML
	Current               bool
}

type cmdRow struct {
	Action, By, Ago, Status, StatusCls, Result string
}

type deployData struct {
	Agent, Deploy, Client, Env, AgentDot, Kind string
	Hist                                       []string
	HistTitle                                  string
	Badge                                      template.HTML
	Facts                                      [][2]string
	Error                                      string
	Phases                                     []phaseBar
	PhaseMid, PhaseEnd                         string
	Hosts                                      []hostBox
	HostsSub                                   string
	Log                                        []string
	Runs                                       []runRow
	Labels                                     []string
	RunCmd                                     string
	CanCommand, CanRollback                    bool
	NoCommands                                 string
	Commands                                   []cmdRow
	ActionsHref                                string
}

func fmtSeconds(ms int64) string {
	s := float64(ms) / 1000
	switch {
	case s < 10:
		return strconv.FormatFloat(s, 'f', 1, 64) + "s"
	case s < 60:
		return strconv.Itoa(int(s+.5)) + "s"
	}
	return fmt.Sprintf("%dm%02ds", int(s)/60, int(s)%60)
}

func (h *hubServer) pageDeploy(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	ctx := r.Context()
	agent, deploy := r.PathValue("agent"), r.PathValue("deploy")
	d, err := h.load(ctx)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	var v *hubView
	for i := range d.views {
		if d.views[i].Agent == agent && d.views[i].Deploy == deploy {
			v = &d.views[i]
		}
	}
	if v == nil {
		h.renderTemplate(w, http.StatusNotFound, "error", map[string]any{"Lang": lang, "Error": hubT(lang, "err.notfound", deploy, agent)})
		return
	}
	a, _, _ := h.store.agent(ctx, agent)
	now := time.Now()
	st := v.State
	data := deployData{Agent: agent, Deploy: deploy, Client: v.Client, Env: v.Env, AgentDot: agentDot(&a, now), Kind: kindOf(*v),
		Hist: histOf(d.recent[[2]string{agent, deploy}]), HistTitle: hubT(lang, "app.last_runs"), Badge: badgeHTML(lang, st.Status), Error: st.Error,
		RunCmd: "sudo -iu deploy nimdeploy run -f " + deploy, ActionsHref: deployHref(agent, deploy)}
	last := hubT(lang, "dp.never")
	if st.StartedAt != nil {
		last = hubT(lang, "app.ago", agoIn(lang, now.Sub(v.When)))
		if st.Duration != "" {
			last += " · " + st.Duration
		}
	}
	commit := "—"
	switch {
	case st.Commit != "" || st.Tag != "":
		commit = strings.TrimSpace(refLabel(st.Tag, st.Commit) + " " + st.Message)
	case v.Schedule != "":
		commit = v.Schedule + " · " + hubT(lang, "dp.scheduled")
	}
	source := v.Provider
	if v.Repository != "" {
		source = v.Repository + " · " + v.Branch
	}
	data.Facts = [][2]string{{hubT(lang, "dp.last_run"), last}, {hubT(lang, "dp.commit"), commit}, {hubT(lang, "dp.source"), firstNonEmpty(source, "—")},
		{hubT(lang, "dp.by"), firstNonEmpty(st.Pusher, st.Trigger, "—")}}
	if n := len(st.Phases); n > 0 {
		var total int64
		for _, p := range st.Phases {
			total = max(total, p.StartMS+p.DurationMS)
		}
		total = max(total, 1)
		for _, p := range st.Phases {
			cls := "ok"
			switch {
			case p.Failed:
				cls = "bad"
			case p.Name == "queue.wait" || p.Name == "ci":
				cls = "dim"
			}
			data.Phases = append(data.Phases, phaseBar{Name: p.Name, Cls: cls, Took: fmtSeconds(p.DurationMS),
				Left:  strconv.FormatFloat(float64(p.StartMS)*100/float64(total), 'f', 2, 64),
				Width: strconv.FormatFloat(max(float64(p.DurationMS)*100/float64(total), .6), 'f', 2, 64)})
		}
		data.PhaseMid, data.PhaseEnd = fmtSeconds(total/2), fmtSeconds(total)
	}
	if an := st.Ansible; an != nil {
		results := append([]AnsibleHost(nil), an.HostResults...)
		rank := map[string]int{"failed": 0, "unreachable": 1, "changed": 2, "ok": 3}
		sort.SliceStable(results, func(i, j int) bool { return rank[results[i].Outcome] < rank[results[j].Outcome] })
		for _, hr := range results {
			data.Hosts = append(data.Hosts, hostBox{Host: hr.Host, Outcome: hr.Outcome})
		}
		data.HostsSub = hubT(lang, "dp.hosts_sub", an.Hosts, an.HostsChanged, an.HostsFailed+an.HostsUnreachable)
	}
	if st.Status != StatusSuccess {
		if evs, err := h.store.events(ctx, agent, deploy, hubEventDeployDone, 1); err == nil && len(evs) == 1 {
			data.Log = evs[0].LogTail
		}
	}
	if runs, err := h.store.runs(ctx, agent, deploy, 10); err == nil {
		for i, run := range runs {
			rr := runRow{Badge: badgeHTML(lang, run.Status), Commit: refLabel(run.Tag, run.Commit), Took: run.Duration, By: run.Pusher, Current: i == 0}
			if run.FinishedAt != nil {
				rr.Ago = agoShort(lang, now.Sub(*run.FinishedAt))
			}
			data.Runs = append(data.Runs, rr)
		}
	}
	for _, k := range sortedKeys(v.Labels) {
		data.Labels = append(data.Labels, k+"="+v.Labels[k])
	}
	data.CanCommand = a.AcceptsCommands && a.RevokedAt == ""
	_, git := providers[v.Provider]
	data.CanRollback = data.CanCommand && git
	if !data.CanCommand {
		data.NoCommands = hubT(lang, "dp.no_commands_agent", agent)
	}
	data.Commands = h.commandRows(ctx, lang, agent, deploy)
	crumbs := []crumb{{Label: firstNonEmpty(v.Client, "—"), Href: clientHref(v.Client, "")}, {Label: firstNonEmpty(v.Env, "—"), Href: clientHref(v.Client, v.Env)}, {Label: deploy}}
	h.page(w, r, who, "deploy", d, v.Client, v.Env, crumbs, data)
}

func (h *hubServer) commandRows(ctx context.Context, lang, agent, deploy string) []cmdRow {
	cmds, err := h.store.commandsFor(ctx, agent, deploy, 5)
	if err != nil {
		return nil
	}
	now := time.Now()
	var out []cmdRow
	for _, c := range cmds {
		cls := map[string]string{"done": statusCls(c.Result), "refused": "bad", "expired": "bad", "running": "run", "sent": "wait", "pending": "wait"}[c.Status]
		label := hubT(lang, "dp.run_again")
		if c.Action == hubCommandActionRB {
			label = hubT(lang, "dp.rollback")
		}
		res := c.Result
		if c.Status == "done" {
			res = hubT(lang, "st."+orNever(c.Result))
		}
		out = append(out, cmdRow{Action: label, By: c.By, Ago: agoShort(lang, now.Sub(c.CreatedAt)), Status: hubT(lang, "cmd."+c.Status), StatusCls: cls, Result: res})
	}
	return out
}

// ---------- agents ----------

type agentCard struct {
	Name, Where, Color, Initials, State, StateLabel string
	Host, Version, LastSeen, UpSince, Waiting, Cert string
	CertNote, CertTitle                             string
	Old, Pending, Commands                          bool
	Outbox                                          bool
	Beats                                           []bool
	CommandsLabel                                   string
}

func (h *hubServer) agentCards(ctx context.Context, lang string, agents []hubAgentRow) []agentCard {
	beats, _ := h.store.beats(ctx, 30)
	now := time.Now()
	var out []agentCard
	for _, a := range agents {
		if a.RevokedAt != "" {
			continue
		}
		c := agentCard{Name: a.Name, Where: agentWhere(lang, a), Color: colorFor(a.ClientName()),
			Initials: initialsOf(firstNonEmpty(a.ClientName(), a.Name)), Host: a.Host, Version: a.Version, Commands: a.AcceptsCommands, Beats: beats[a.Name]}
		if c.Beats == nil {
			c.Beats = make([]bool, 30)
		}
		switch agentDot(&a, now) {
		case "pending":
			c.State, c.Pending = "pending", true
		case "up":
			c.State = "online"
		default:
			c.State = "offline"
		}
		c.StateLabel = hubT(lang, "st."+c.State)
		if !a.LastSeen.IsZero() {
			c.LastSeen = hubT(lang, "app.ago", agoIn(lang, now.Sub(a.LastSeen)))
		}
		if !a.StartedAt.IsZero() {
			c.UpSince = agoIn(lang, now.Sub(a.StartedAt))
		}
		c.Waiting = hubT(lang, "ag.nothing")
		if a.Outbox > 0 {
			c.Waiting = hubT(lang, "ag.events_to_send", a.Outbox)
			c.Outbox = true
		}
		c.Cert = hubT(lang, "ag.no_cert")
		if a.RequireCert {
			c.Cert = hubT(lang, "ag.cert_until", a.CertNotAfter.Format("2006-01-02"))
			switch {
			case a.CertAutoRenew:
				c.CertNote = hubT(lang, "ag.cert_auto")
			case !a.LastSeen.IsZero():
				c.CertNote, c.CertTitle = hubT(lang, "ag.cert_manual"), hubT(lang, "ag.cert_manual_d")
			}
			if !a.CertRenewedAt.IsZero() {
				c.CertTitle = hubT(lang, "ag.cert_renewed", hubT(lang, "app.ago", agoIn(lang, now.Sub(a.CertRenewedAt))))
			}
		}
		c.CommandsLabel = hubT(lang, map[bool]string{true: "ag.commands_on", false: "ag.commands_off"}[a.AcceptsCommands])
		c.Old = a.Version != "" && version != "dev" && olderVersion(a.Version, version)
		out = append(out, c)
	}
	return out
}

func (h *hubServer) pageAgents(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	d, err := h.load(r.Context())
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	h.page(w, r, who, "agents", d, "", "", []crumb{{Label: hubT(lang, "nav.agents")}}, map[string]any{"Cards": h.agentCards(r.Context(), lang, d.agents)})
}

func (h *hubServer) pageActivity(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	d, err := h.load(r.Context())
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	events, err := h.store.events(r.Context(), "", "", "", 200)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	h.page(w, r, who, "activity", d, "", "", []crumb{{Label: hubT(lang, "nav.activity")}}, map[string]any{"Timeline": timelineItems(lang, d, events, 100)})
}

func (h *hubServer) pageAttention(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	d, err := h.load(r.Context())
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	h.page(w, r, who, "attention", d, "", "", []crumb{{Label: hubT(lang, "nav.attention")}}, map[string]any{"Attn": h.attention(lang, d)})
}

// ---------- language ----------

func (h *hubServer) handleLang(w http.ResponseWriter, r *http.Request) {
	l := r.PathValue("lang")
	for _, x := range hubLangs {
		if x == l {
			http.SetCookie(w, &http.Cookie{Name: "lang", Value: l, Path: "/", MaxAge: 365 * 86400, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r)})
		}
	}
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
}
