package main

import (
	"context"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The Notifications page: rules and destinations, edited in the drawer.

type ruleView struct {
	ID, Name, Lang string
	Enabled        bool
	Events         []string
	Filters        []string
	Channels       []string
}

type channelView struct {
	ID, Name, Kind, KindLabel, Target, Last, LastCls string
}

type opt struct {
	Value, Label string
	On           bool
}

type channelForm struct {
	Lang, CSRF, Error, ID, Name, Kind string
	Kinds                             []opt
	Editing                           bool
	Config                            channelConfig
	To                                string
}

func (h *hubServer) pageNotifications(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	d, err := h.load(r.Context())
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	rules, chans, err := h.notifyData(r.Context())
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	names := map[string]string{}
	var cv []channelView
	for _, c := range chans {
		names[c.ID] = c.Name
		v := channelView{ID: c.ID, Name: c.Name, Kind: c.Kind, KindLabel: hubT(lang, "kind."+c.Kind), Target: c.target(),
			Last: hubT(lang, "nf.never"), LastCls: "never"}
		switch {
		case c.LastAt.IsZero():
		case c.LastError != "":
			v.Last, v.LastCls = hubT(lang, "nf.last_err", agoIn(lang, time.Since(c.LastAt)), c.LastError), "failed"
		default:
			v.Last, v.LastCls = hubT(lang, "nf.last_ok", agoIn(lang, time.Since(c.LastAt))), "success"
		}
		cv = append(cv, v)
	}
	var rv []ruleView
	for _, ru := range rules {
		v := ruleView{ID: ru.ID, Name: ru.Name, Lang: firstNonEmpty(ru.Lang, "en"), Enabled: ru.Enabled}
		for _, e := range ru.Events {
			v.Events = append(v.Events, hubT(lang, "ev."+e))
		}
		var kinds []string
		for _, k := range ru.Kinds {
			kinds = append(kinds, hubT(lang, "nf.kind_"+k))
		}
		for _, f := range []struct {
			key  string
			vals []string
		}{{"nf.f_clients", ru.Clients}, {"nf.f_envs", ru.Envs}, {"nf.watch", kinds}, {"nf.f_agents", ru.Agents}, {"nf.f_jobs", ru.Deploys}} {
			if len(f.vals) > 0 {
				v.Filters = append(v.Filters, hubT(lang, f.key)+": "+strings.Join(f.vals, ", "))
			}
		}
		if len(v.Filters) == 0 {
			v.Filters = []string{hubT(lang, "nf.filters_all")}
		}
		for _, id := range ru.Channels {
			if n, ok := names[id]; ok {
				v.Channels = append(v.Channels, n)
			}
		}
		rv = append(rv, v)
	}
	h.page(w, r, who, "notifications", d, "", "", []crumb{{Label: hubT(lang, "nav.notify")}},
		map[string]any{"Rules": rv, "Channels": cv, "NoLinks": h.notifier == nil || h.notifier.baseURL == ""})
}

func (h *hubServer) notifyData(ctx context.Context) ([]notifyRule, []notifyChannel, error) {
	rules, err := h.store.notifyRules(ctx)
	if err != nil {
		return nil, nil, err
	}
	chans, err := h.store.notifyChannels(ctx)
	return rules, chans, err
}

// --- rules: a wizard in five steps ------------------------------------------------------------
//
// 1 clients, 2 environments, 3 what to watch (deploys or scheduled tasks, and
// optionally some servers and jobs), 4 events, 5 destinations and name. Each
// step only lists what fits the previous ones, so a hub with many clients
// and repositories stays readable. Values of the other steps travel as
// hidden fields; nothing is stored until the last step.

const ruleSteps = 5

// ruleStepFields are the form fields each step shows; the rest go hidden.
var ruleStepFields = map[int][]string{1: {"client"}, 2: {"env"}, 3: {"kind", "agent", "deploy"}, 4: {"event"},
	5: {"channel", "name", "lang", "enabled"}}

type kv struct{ Name, Value string }

type stepView struct {
	N        int
	Label    string
	On, Done bool
}

type jobOpt struct {
	Value, Name, KindLabel, Kind, Detail string
	On                                   bool
}

type jobGroup struct {
	Agent, Where string
	On           bool
	Jobs         []jobOpt
}

type ruleForm struct {
	Lang, CSRF, Error, ID, Name, RuleLang, NamePH string
	Enabled, Narrowed                             bool
	Step                                          int
	Steps                                         []stepView
	Hidden                                        []kv
	Clients, Envs, Kinds                          []opt
	Groups                                        []jobGroup
	RunEvents, ServerEvents, Channels             []opt
	Summary                                       []string
}

func (h *hubServer) uiRuleForm(w http.ResponseWriter, r *http.Request, _ string) {
	ru := notifyRule{Enabled: true, Lang: hubLang(r), Events: []string{notifyDeployFailed, notifyDeployRecovered, notifyAgentOffline}}
	if id := r.PathValue("id"); id != "" {
		rules, err := h.store.notifyRules(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		i := slices.IndexFunc(rules, func(x notifyRule) bool { return x.ID == id })
		if i < 0 {
			http.NotFound(w, r)
			return
		}
		ru = rules[i]
	}
	h.renderRuleStep(w, r, ru, 1, "")
}

func cleanValues(vals []string) []string {
	var out []string
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" && len(v) <= 200 && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func ruleFromForm(r *http.Request) notifyRule {
	ru := notifyRule{ID: r.FormValue("id"), Name: strings.TrimSpace(r.FormValue("name")), Lang: r.FormValue("lang"),
		Enabled: r.FormValue("enabled") == "on", Clients: cleanValues(r.Form["client"]), Envs: cleanValues(r.Form["env"]),
		Agents: cleanValues(r.Form["agent"]), Deploys: cleanValues(r.Form["deploy"]), Channels: cleanValues(r.Form["channel"])}
	for _, e := range cleanValues(r.Form["event"]) {
		if slices.Contains(notifyEvents, e) {
			ru.Events = append(ru.Events, e)
		}
	}
	for _, k := range cleanValues(r.Form["kind"]) {
		if k == jobDeploy || k == jobTask {
			ru.Kinds = append(ru.Kinds, k)
		}
	}
	if len(ru.Kinds) == 2 {
		ru.Kinds = nil // both is no filter
	}
	if ru.Lang != "es" {
		ru.Lang = "en"
	}
	return ru
}

// uiRuleStep moves between steps (go=1..5) or saves the rule (go=save).
func (h *hubServer) uiRuleStep(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ru := ruleFromForm(r)
	from, _ := strconv.Atoi(r.FormValue("step"))
	to := r.FormValue("go")
	if n, err := strconv.Atoi(to); err == nil && n >= 1 && n <= ruleSteps {
		if n > from && from == 4 && len(ru.Events) == 0 {
			h.renderRuleStep(w, r, ru, 4, hubT(lang, "nf.err_events"))
			return
		}
		h.renderRuleStep(w, r, ru, n, "")
		return
	}
	switch {
	case len(ru.Events) == 0:
		h.renderRuleStep(w, r, ru, 4, hubT(lang, "nf.err_events"))
		return
	case len(ru.Channels) == 0:
		h.renderRuleStep(w, r, ru, 5, hubT(lang, "nf.err_dests"))
		return
	case len(ru.Name) > 100:
		h.renderRuleStep(w, r, ru, 5, hubT(lang, "nf.err_name"))
		return
	}
	if ru.Name == "" {
		ru.Name = h.suggestRuleName(lang, ru)
	}
	if ru.ID == "" {
		ru.ID = newNotifyID()
	}
	if err := h.store.saveRule(r.Context(), ru); err != nil {
		h.renderRuleStep(w, r, ru, 5, err.Error())
		return
	}
	h.audit(r, who, "notification rule %q saved", ru.Name)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// ruleScope is what the hub knows, narrowed by the rule's clients and environments.
type ruleScope struct {
	clients, envs []string
	agents        []hubAgentRow
	views         []hubView
}

func (h *hubServer) ruleScope(ctx context.Context, lang string, ru notifyRule) ruleScope {
	var sc ruleScope
	d, err := h.load(ctx)
	if err != nil {
		return sc
	}
	in := func(list []string, v string) bool {
		return len(list) == 0 || slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, v) })
	}
	clients, envs := map[string]bool{}, map[string]bool{}
	cs, _ := d.clientsOf()
	for _, c := range cs {
		if c != "" {
			clients[c] = true
		}
	}
	for _, v := range d.views {
		if v.Client != "" {
			clients[v.Client] = true
		}
		if in(ru.Clients, v.Client) && v.Env != "" {
			envs[v.Env] = true
		}
	}
	agentIn := map[string]bool{}
	for _, a := range d.agents {
		if a.RevokedAt != "" {
			continue
		}
		if in(ru.Clients, a.ClientName()) && a.EnvName() != "" {
			envs[a.EnvName()] = true
		}
		if in(ru.Clients, a.ClientName()) && in(ru.Envs, a.EnvName()) {
			agentIn[a.Name] = true
		}
	}
	for _, v := range d.views {
		if in(ru.Clients, v.Client) && in(ru.Envs, v.Env) && v.InInventory {
			agentIn[v.Agent] = true
			sc.views = append(sc.views, v)
		}
	}
	for _, a := range d.agents {
		if agentIn[a.Name] && a.RevokedAt == "" {
			sc.agents = append(sc.agents, a)
		}
	}
	sc.clients, sc.envs = sortedKeys(clients), sortedKeys(envs)
	return sc
}

func (h *hubServer) renderRuleStep(w http.ResponseWriter, r *http.Request, ru notifyRule, step int, errMsg string) {
	lang := hubLang(r)
	ctx := r.Context()
	sc := h.ruleScope(ctx, lang, ru)
	f := ruleForm{Lang: lang, CSRF: csrfToken(w, r), Error: errMsg, ID: ru.ID, Name: ru.Name, RuleLang: firstNonEmpty(ru.Lang, lang),
		Enabled: ru.Enabled, Step: step}
	for n := 1; n <= ruleSteps; n++ {
		f.Steps = append(f.Steps, stepView{N: n, Label: hubT(lang, "nf.step"+strconv.Itoa(n)), On: n == step, Done: n < step})
	}
	// Everything not on this step travels hidden.
	values := map[string][]string{"client": ru.Clients, "env": ru.Envs, "kind": ru.Kinds, "agent": ru.Agents, "deploy": ru.Deploys,
		"event": ru.Events, "channel": ru.Channels, "name": {ru.Name}, "lang": {ru.Lang}}
	if ru.Enabled {
		values["enabled"] = []string{"on"}
	}
	for _, name := range []string{"client", "env", "kind", "agent", "deploy", "event", "channel", "name", "lang", "enabled"} {
		if slices.Contains(ruleStepFields[step], name) {
			continue
		}
		for _, v := range values[name] {
			if v != "" {
				f.Hidden = append(f.Hidden, kv{name, v})
			}
		}
	}
	opts := func(all, on []string) []opt {
		out := make([]opt, 0, len(all))
		for _, v := range all {
			out = append(out, opt{Value: v, Label: v, On: slices.ContainsFunc(on, func(x string) bool { return strings.EqualFold(x, v) })})
		}
		return out
	}
	switch step {
	case 1:
		f.Clients = opts(sc.clients, ru.Clients)
	case 2:
		f.Envs = opts(sc.envs, ru.Envs)
	case 3:
		for _, k := range []string{jobDeploy, jobTask} {
			f.Kinds = append(f.Kinds, opt{Value: k, Label: hubT(lang, "nf.kind_"+k), On: len(ru.Kinds) == 0 || slices.Contains(ru.Kinds, k)})
		}
		byAgent := map[string]*jobGroup{}
		for _, a := range sc.agents {
			g := &jobGroup{Agent: a.Name, Where: agentWhere(lang, a), On: slices.Contains(ru.Agents, a.Name)}
			byAgent[a.Name] = g
		}
		for _, v := range sc.views {
			g := byAgent[v.Agent]
			if g == nil {
				continue
			}
			kind := jobKind(v.Provider)
			detail := v.Repository
			if kind == jobTask || detail == "" {
				detail = v.Schedule
			}
			value := v.Agent + "/" + v.Deploy
			g.Jobs = append(g.Jobs, jobOpt{Value: value, Name: v.Deploy, Kind: kind, KindLabel: hubT(lang, "nf.kind1_"+kind), Detail: detail,
				On: slices.Contains(ru.Deploys, value) || slices.Contains(ru.Deploys, v.Deploy)})
		}
		for _, a := range sc.agents {
			f.Groups = append(f.Groups, *byAgent[a.Name])
		}
		f.Narrowed = len(ru.Agents) > 0 || len(ru.Deploys) > 0
	case 4:
		for _, e := range notifyEvents {
			o := opt{Value: e, Label: hubT(lang, "ev."+e), On: slices.Contains(ru.Events, e)}
			if strings.HasPrefix(e, "agent.") || strings.HasPrefix(e, "cert.") {
				f.ServerEvents = append(f.ServerEvents, o)
			} else {
				f.RunEvents = append(f.RunEvents, o)
			}
		}
	case 5:
		if chans, err := h.store.notifyChannels(ctx); err == nil {
			for _, c := range chans {
				f.Channels = append(f.Channels, opt{Value: c.ID, Label: c.Name + " · " + hubT(lang, "kind."+c.Kind),
					On: slices.Contains(ru.Channels, c.ID)})
			}
		}
		f.NamePH = h.suggestRuleName(lang, ru)
		f.Summary = ruleSummary(lang, ru)
	}
	h.renderTemplate(w, http.StatusOK, "drawer-rule", f)
}

func ruleSummary(lang string, ru notifyRule) []string {
	all := func(vals []string, key string) string {
		if len(vals) == 0 {
			return hubT(lang, key)
		}
		return strings.Join(vals, ", ")
	}
	kinds := hubT(lang, "nf.kinds_all")
	if len(ru.Kinds) == 1 {
		kinds = hubT(lang, "nf.kind_"+ru.Kinds[0])
	}
	var events []string
	for _, e := range ru.Events {
		events = append(events, hubT(lang, "ev."+e))
	}
	out := []string{
		hubT(lang, "nf.f_clients") + ": " + all(ru.Clients, "nf.all_m"),
		hubT(lang, "nf.f_envs") + ": " + all(ru.Envs, "nf.all_m"),
		hubT(lang, "nf.watch") + ": " + kinds,
	}
	if len(ru.Agents) > 0 {
		out = append(out, hubT(lang, "nf.f_agents")+": "+strings.Join(ru.Agents, ", "))
	}
	if len(ru.Deploys) > 0 {
		out = append(out, hubT(lang, "nf.f_jobs")+": "+strings.Join(ru.Deploys, ", "))
	}
	return append(out, hubT(lang, "nf.events")+": "+all(events, "nf.none"))
}

// suggestRuleName names a rule after what it covers: "Acme · production — failures".
func (h *hubServer) suggestRuleName(lang string, ru notifyRule) string {
	var parts []string
	if len(ru.Clients) > 0 && len(ru.Clients) <= 2 {
		parts = append(parts, strings.Join(ru.Clients, ", "))
	}
	if len(ru.Envs) > 0 && len(ru.Envs) <= 2 {
		parts = append(parts, strings.Join(ru.Envs, ", "))
	}
	where := strings.Join(parts, " · ")
	if where == "" {
		where = hubT(lang, "nf.everything")
	}
	if len(ru.Kinds) == 1 {
		where += " · " + hubT(lang, "nf.kind_"+ru.Kinds[0])
	}
	return clip(where, 100)
}

func (h *hubServer) uiToggleRule(w http.ResponseWriter, r *http.Request, who string) {
	on := r.FormValue("on") == "1"
	if err := h.store.setRuleEnabled(r.Context(), r.PathValue("id"), on); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	h.audit(r, who, "notification rule %s enabled=%v", r.PathValue("id"), on)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

func (h *hubServer) uiDeleteRule(w http.ResponseWriter, r *http.Request, who string) {
	if err := h.store.deleteRule(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	h.audit(r, who, "notification rule %s deleted", r.PathValue("id"))
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// --- destinations ----------------------------------------------------------------------------

func (h *hubServer) channelByID(r *http.Request, id string) (notifyChannel, bool, error) {
	chans, err := h.store.notifyChannels(r.Context())
	if err != nil {
		return notifyChannel{}, false, err
	}
	i := slices.IndexFunc(chans, func(c notifyChannel) bool { return c.ID == id })
	if i < 0 {
		return notifyChannel{}, false, nil
	}
	return chans[i], true, nil
}

func (h *hubServer) uiChannelForm(w http.ResponseWriter, r *http.Request, _ string) {
	c := notifyChannel{Kind: "slack"}
	if id := r.PathValue("id"); id != "" {
		var ok bool
		var err error
		if c, ok, err = h.channelByID(r, id); err != nil || !ok {
			http.NotFound(w, r)
			return
		}
	}
	h.renderChannelForm(w, r, c, c.ID != "", "")
}

func (h *hubServer) renderChannelForm(w http.ResponseWriter, r *http.Request, c notifyChannel, editing bool, errMsg string) {
	lang := hubLang(r)
	f := channelForm{Lang: lang, CSRF: csrfToken(w, r), Error: errMsg, ID: c.ID, Name: c.Name, Kind: c.Kind, Editing: editing,
		To: strings.Join(c.Config.To, ", ")}
	// Secrets never go back to the browser: editing shows them empty, and empty keeps them.
	f.Config = channelConfig{ChatID: c.Config.ChatID, SMTPHost: c.Config.SMTPHost, SMTPPort: c.Config.SMTPPort, SMTPTLS: c.Config.SMTPTLS,
		SMTPUser: c.Config.SMTPUser, From: c.Config.From}
	for _, k := range notifyKinds {
		f.Kinds = append(f.Kinds, opt{Value: k, Label: hubT(lang, "kind."+k), On: k == c.Kind})
	}
	h.renderTemplate(w, http.StatusOK, "drawer-channel", f)
}

func (h *hubServer) uiSaveChannel(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	c := notifyChannel{ID: r.FormValue("id"), Name: strings.TrimSpace(r.FormValue("name")), Kind: r.FormValue("kind")}
	var old notifyChannel
	editing := false
	if c.ID != "" {
		var err error
		if old, editing, err = h.channelByID(r, c.ID); err != nil || !editing {
			http.NotFound(w, r)
			return
		}
	}
	port, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("smtp_port")))
	c.Config = channelConfig{URL: strings.TrimSpace(r.FormValue("url")), BotToken: strings.TrimSpace(r.FormValue("bot_token")),
		ChatID: strings.TrimSpace(r.FormValue("chat_id")), SMTPHost: strings.TrimSpace(r.FormValue("smtp_host")), SMTPPort: port,
		SMTPTLS: r.FormValue("smtp_tls"), SMTPUser: strings.TrimSpace(r.FormValue("smtp_user")), SMTPPass: r.FormValue("smtp_password"),
		From: strings.TrimSpace(r.FormValue("from"))}
	for _, a := range strings.FieldsFunc(r.FormValue("to"), func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == ' ' }) {
		c.Config.To = append(c.Config.To, a)
	}
	if editing && old.Kind == c.Kind {
		// Empty secret fields keep what was stored.
		if c.Config.URL == "" {
			c.Config.URL = old.Config.URL
		}
		if c.Config.BotToken == "" {
			c.Config.BotToken = old.Config.BotToken
		}
		if c.Config.SMTPPass == "" {
			c.Config.SMTPPass = old.Config.SMTPPass
		}
	}
	if c.Name == "" || len(c.Name) > 100 {
		h.renderChannelForm(w, r, c, editing, hubT(lang, "nf.err_name"))
		return
	}
	if err := c.Config.validate(c.Kind); err != nil {
		h.renderChannelForm(w, r, c, editing, err.Error())
		return
	}
	if c.ID == "" {
		c.ID = newNotifyID()
	}
	if err := h.store.saveChannel(r.Context(), c); err != nil {
		h.renderChannelForm(w, r, c, editing, err.Error())
		return
	}
	h.audit(r, who, "notification destination %q (%s) saved", c.Name, c.Kind)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

func (h *hubServer) uiDeleteChannel(w http.ResponseWriter, r *http.Request, who string) {
	if err := h.store.deleteChannel(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	h.audit(r, who, "notification destination %s deleted", r.PathValue("id"))
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// uiTestChannel sends a test message now, without retries, and says how it went.
func (h *hubServer) uiTestChannel(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	c, ok, err := h.channelByID(r, r.PathValue("id"))
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	nf := h.notifier
	if nf == nil {
		nf = newHubNotifier(h.store, "")
	}
	nt := hubNotice{Event: "test", Time: time.Now()}
	if nf.baseURL != "" {
		nt.Link = nf.baseURL + "/notifications"
	}
	err = nf.deliver(c, nt, lang)
	_ = h.store.channelResult(r.Context(), c.ID, err)
	h.audit(r, who, "notification destination %q tested", c.Name)
	data := map[string]any{"OK": err == nil, "Text": hubT(lang, "nf.test_ok")}
	if err != nil {
		data["Text"] = hubT(lang, "nf.test_fail", err.Error())
	}
	h.renderTemplate(w, http.StatusOK, "chan-test", data)
}

// audit logs a change made from the dashboard, with who made it.
func (h *hubServer) audit(r *http.Request, who, format string, args ...any) {
	log.Printf("hub: "+format+" by %s from %s", append(args, h.actor(r, who), clientAddr(r))...)
}
