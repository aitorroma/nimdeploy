package main

import (
	"context"
	"log"
	"net/http"
	"slices"
	"sort"
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

type ruleForm struct {
	Lang, CSRF, Error, ID, Name, RuleLang string
	Enabled                               bool
	Events, Clients, Envs, Agents         []opt
	Deploys, Channels                     []opt
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
		for _, f := range []struct {
			key  string
			vals []string
		}{{"nf.f_clients", ru.Clients}, {"nf.f_envs", ru.Envs}, {"nf.f_agents", ru.Agents}, {"nf.f_deploys", ru.Deploys}} {
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

// --- rules ---------------------------------------------------------------------------------

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
	h.renderRuleForm(w, r, ru, "")
}

func (h *hubServer) renderRuleForm(w http.ResponseWriter, r *http.Request, ru notifyRule, errMsg string) {
	lang := hubLang(r)
	f := ruleForm{Lang: lang, CSRF: csrfToken(w, r), Error: errMsg, ID: ru.ID, Name: ru.Name, RuleLang: firstNonEmpty(ru.Lang, lang), Enabled: ru.Enabled}
	for _, e := range notifyEvents {
		f.Events = append(f.Events, opt{Value: e, Label: hubT(lang, "ev."+e), On: slices.Contains(ru.Events, e)})
	}
	clients, envs, agents, deploys := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	if d, err := h.load(r.Context()); err == nil {
		for _, a := range d.agents {
			if a.RevokedAt == "" {
				agents[a.Name] = true
			}
		}
		for _, v := range d.views {
			clients[v.Client], envs[v.Env], deploys[v.Deploy] = true, true, true
		}
		cs, _ := d.clientsOf()
		for _, c := range cs {
			clients[c] = true
		}
	}
	// Values a rule already has stay listed even if nothing reports them now.
	for set, vals := range map[*map[string]bool][]string{&clients: ru.Clients, &envs: ru.Envs, &agents: ru.Agents, &deploys: ru.Deploys} {
		for _, v := range vals {
			(*set)[v] = true
		}
	}
	options := func(set map[string]bool, on []string) []opt {
		var keys []string
		for k := range set {
			if k != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		out := make([]opt, 0, len(keys))
		for _, k := range keys {
			out = append(out, opt{Value: k, Label: k, On: slices.Contains(on, k)})
		}
		return out
	}
	f.Clients, f.Envs, f.Agents, f.Deploys = options(clients, ru.Clients), options(envs, ru.Envs), options(agents, ru.Agents), options(deploys, ru.Deploys)
	if chans, err := h.store.notifyChannels(r.Context()); err == nil {
		for _, c := range chans {
			f.Channels = append(f.Channels, opt{Value: c.ID, Label: c.Name + " · " + hubT(lang, "kind."+c.Kind), On: slices.Contains(ru.Channels, c.ID)})
		}
	}
	h.renderTemplate(w, http.StatusOK, "drawer-rule", f)
}

func (h *hubServer) uiSaveRule(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	clean := func(vals []string) []string {
		var out []string
		for _, v := range vals {
			if v = strings.TrimSpace(v); v != "" && len(v) <= 200 && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		return out
	}
	ru := notifyRule{ID: r.FormValue("id"), Name: strings.TrimSpace(r.FormValue("name")), Lang: r.FormValue("lang"), Enabled: r.FormValue("enabled") == "on",
		Clients: clean(r.Form["client"]), Envs: clean(r.Form["env"]), Agents: clean(r.Form["agent"]), Deploys: clean(r.Form["deploy"]),
		Channels: clean(r.Form["channel"])}
	for _, e := range clean(r.Form["event"]) {
		if slices.Contains(notifyEvents, e) {
			ru.Events = append(ru.Events, e)
		}
	}
	if ru.Lang != "es" {
		ru.Lang = "en"
	}
	switch {
	case ru.Name == "" || len(ru.Name) > 100:
		h.renderRuleForm(w, r, ru, hubT(lang, "nf.err_name"))
		return
	case len(ru.Events) == 0:
		h.renderRuleForm(w, r, ru, hubT(lang, "nf.err_events"))
		return
	case len(ru.Channels) == 0:
		h.renderRuleForm(w, r, ru, hubT(lang, "nf.err_dests"))
		return
	}
	if ru.ID == "" {
		ru.ID = newNotifyID()
	}
	if err := h.store.saveRule(r.Context(), ru); err != nil {
		h.renderRuleForm(w, r, ru, err.Error())
		return
	}
	h.audit(r, who, "notification rule %q saved", ru.Name)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
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
