package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Dashboard actions, answered with htmx fragments: adding and renewing
// agents (token, and a client certificate when required), revoking them,
// and commands (run again, roll back). Every change checks the CSRF token.

type drawerData struct {
	Lang, CSRF, Error string
	Step              int
	// step 1
	Client, Env, Name string
	Clients           []string
	CertAvailable     bool
	RequireCert       bool
	// step 2
	Token, CertPEM, KeyPEM, CertDataURL, KeyDataURL string
	CertNotAfter                                    string
	HubURL, Config, Renew                           string
	WaitHref                                        string
	Connected                                       string
	OpenHref, OpenLabel                             string
}

func (h *hubServer) actor(r *http.Request, who string) string {
	if who == "token" || who == "anonymous" {
		return "admin"
	}
	return who
}

// changes wraps a dashboard change: logged in (via ui) and a valid CSRF token.
func (h *hubServer) changes(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return h.ui(func(w http.ResponseWriter, r *http.Request, who string) {
		if !checkCSRF(r) {
			http.Error(w, "missing or invalid CSRF token: reload the page", http.StatusForbidden)
			return
		}
		next(w, r, who)
	})
}

func (h *hubServer) hubURL(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (h *hubServer) drawerClients(r *http.Request) []string {
	d, err := h.load(r.Context())
	if err != nil {
		return nil
	}
	clients, _ := d.clientsOf()
	out := clients[:0]
	for _, c := range clients {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

func (h *hubServer) uiNewAgent(w http.ResponseWriter, r *http.Request, _ string) {
	d := drawerData{Lang: hubLang(r), CSRF: csrfToken(w, r), Step: 1, Client: r.URL.Query().Get("client"), Env: "stage",
		Clients: h.drawerClients(r), CertAvailable: h.ca != nil}
	h.renderTemplate(w, http.StatusOK, "drawer-new", d)
}

func (h *hubServer) uiCreateAgent(w http.ResponseWriter, r *http.Request, who string) {
	lang := hubLang(r)
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	d := drawerData{Lang: lang, CSRF: csrfToken(w, r), Step: 1, Client: strings.TrimSpace(r.FormValue("client")), Env: strings.TrimSpace(r.FormValue("env")),
		Name: strings.TrimSpace(r.FormValue("name")), RequireCert: r.FormValue("cert") == "on", Clients: h.drawerClients(r), CertAvailable: h.ca != nil}
	fail := func(msg string) {
		d.Error = msg
		h.renderTemplate(w, http.StatusOK, "drawer-new", d)
	}
	if !agentNameRe.MatchString(d.Name) {
		fail(hubT(lang, "new.name_hint"))
		return
	}
	if existing, ok, err := h.store.agent(r.Context(), d.Name); err != nil {
		fail(err.Error())
		return
	} else if ok && existing.RevokedAt == "" {
		fail(map[string]string{"en": "An agent with this name exists: use New token on its card.", "es": "Ya existe un agente con ese nombre: usa Nuevo token en su tarjeta."}[lang])
		return
	}
	if d.RequireCert && h.ca == nil {
		fail(hubT(lang, "new.cert_off"))
		return
	}
	h.issueAndShow(w, r, who, newAgentOpts{Name: d.Name, Client: d.Client, Env: d.Env, RequireCert: d.RequireCert}, false)
}

// issueAndShow creates or renews an agent and shows its token (and certificate) once.
func (h *hubServer) issueAndShow(w http.ResponseWriter, r *http.Request, who string, o newAgentOpts, renew bool) {
	lang := hubLang(r)
	var ic issuedCert
	if o.RequireCert {
		var err error
		if ic, err = h.ca.issue(o.Name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		o.CertFP, o.CertNotAfter = ic.Fingerprint, ic.NotAfter
	}
	token, err := h.store.createAgent(r.Context(), o)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	verb := "created"
	if renew {
		verb = "renewed"
	}
	log.Printf("hub: agent %s %s by %s from %s (certificate: %v)", o.Name, verb, h.actor(r, who), clientAddr(r), o.RequireCert)
	a, _, _ := h.store.agent(r.Context(), o.Name)
	d := drawerData{Lang: lang, Step: 2, Name: o.Name, Client: a.ClientName(), Env: a.EnvName(), Token: token, HubURL: h.hubURL(r),
		WaitHref: "/ui/agents/" + url.PathEscape(o.Name) + "/wait?since=" + fmt.Sprint(time.Now().Unix())}
	if renew {
		d.Renew = "1"
	}
	cfg := fmt.Sprintf("[labels]\nclient = %q\nenvironment = %q\n\n[hub]\nurl = %q\nagent = %q\ntoken_env = \"NIMDEPLOY_HUB_TOKEN\"", d.Client, d.Env, d.HubURL, o.Name)
	if o.RequireCert {
		d.CertPEM, d.KeyPEM = ic.CertPEM, ic.KeyPEM
		d.CertDataURL = "data:application/x-pem-file;charset=utf-8," + url.PathEscape(ic.CertPEM)
		d.KeyDataURL = "data:application/x-pem-file;charset=utf-8," + url.PathEscape(ic.KeyPEM)
		d.CertNotAfter = ic.NotAfter.Format("2006-01-02")
		cfg += "\ncert_file = \"/home/deploy/.config/nimdeploy/hub-client.pem\"\nkey_file = \"/home/deploy/.config/nimdeploy/hub-client.key\""
	}
	d.Config = cfg + "\n# accept_commands = true   # let the hub run deploys again and roll them back"
	h.renderTemplate(w, http.StatusOK, "drawer-issued", d)
}

// uiWaitAgent answers the "waiting for the first heartbeat" poll.
func (h *hubServer) uiWaitAgent(w http.ResponseWriter, r *http.Request, _ string) {
	lang := hubLang(r)
	name := r.PathValue("name")
	var since int64
	fmt.Sscan(r.URL.Query().Get("since"), &since)
	a, ok, err := h.store.agent(r.Context(), name)
	d := drawerData{Lang: lang, Name: name, WaitHref: r.URL.RequestURI()}
	if err == nil && ok && !a.LastSeen.IsZero() && a.LastSeen.Unix() >= since {
		apps := 0
		if dd, err := h.load(r.Context()); err == nil {
			for _, v := range dd.views {
				if v.Agent == name && v.InInventory {
					apps++
				}
			}
		}
		d.Connected = hubT(lang, "new.connected", name, a.Version, apps)
		d.OpenHref = clientHref(a.ClientName(), a.EnvName())
		d.OpenLabel = hubT(lang, "new.open", strings.Join(nonEmpty(a.ClientName(), a.EnvName()), " · "))
	}
	h.renderTemplate(w, http.StatusOK, "drawer-wait", d)
}

func (h *hubServer) uiRenewConfirm(w http.ResponseWriter, r *http.Request, _ string) {
	a, ok, err := h.store.agent(r.Context(), r.PathValue("name"))
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	h.renderTemplate(w, http.StatusOK, "drawer-renew", drawerData{Lang: hubLang(r), CSRF: csrfToken(w, r), Name: a.Name, RequireCert: a.RequireCert, CertAvailable: h.ca != nil})
}

func (h *hubServer) uiRenew(w http.ResponseWriter, r *http.Request, who string) {
	a, ok, err := h.store.agent(r.Context(), r.PathValue("name"))
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	requireCert := a.RequireCert
	if v := r.FormValue("cert"); v != "" {
		requireCert = v == "on"
	}
	if requireCert && h.ca == nil {
		http.Error(w, hubT(hubLang(r), "new.cert_off"), http.StatusBadRequest)
		return
	}
	h.issueAndShow(w, r, who, newAgentOpts{Name: a.Name, RequireCert: requireCert}, true)
}

// uiAgentFoot is an agent card's buttons; ?confirm=1 asks before revoking.
func (h *hubServer) uiAgentFoot(w http.ResponseWriter, r *http.Request, _ string) {
	h.renderTemplate(w, http.StatusOK, "agent-foot", map[string]any{"Lang": hubLang(r), "Name": r.PathValue("name"), "Confirm": r.URL.Query().Get("confirm") == "1"})
}

func loginData(r *http.Request, next, errMsg string) map[string]any {
	lang := hubLang(r)
	return map[string]any{"Next": next, "Error": errMsg, "Hint": hubT(lang, "login.hint"), "Button": hubT(lang, "login.button"), "Lang": lang}
}

func (h *hubServer) uiRevoke(w http.ResponseWriter, r *http.Request, who string) {
	name := r.PathValue("name")
	if err := h.store.revokeAgent(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	log.Printf("hub: agent %s revoked by %s from %s", name, h.actor(r, who), clientAddr(r))
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// ---------- commands ----------

type actionsData struct {
	Lang, CSRF, Agent, Deploy, Href, Action, Question string
	CanCommand, CanRollback                           bool
	NoCommands                                        string
	Commands                                          []cmdRow
	Polling                                           bool
}

func (h *hubServer) actionsFor(w http.ResponseWriter, r *http.Request) (actionsData, bool) {
	agent, deploy := r.PathValue("agent"), r.PathValue("deploy")
	a, ok, err := h.store.agent(r.Context(), agent)
	if err != nil || !ok {
		http.NotFound(w, r)
		return actionsData{}, false
	}
	lang := hubLang(r)
	ad := actionsData{Lang: lang, CSRF: csrfToken(w, r), Agent: agent, Deploy: deploy, Href: deployHref(agent, deploy),
		CanCommand: a.AcceptsCommands && a.RevokedAt == ""}
	if dd, err := h.load(r.Context()); err == nil {
		for _, v := range dd.views {
			if v.Agent == agent && v.Deploy == deploy {
				_, git := providers[v.Provider]
				ad.CanRollback = ad.CanCommand && git
			}
		}
	}
	if !ad.CanCommand {
		ad.NoCommands = hubT(lang, "dp.no_commands_agent", agent)
	}
	ad.Commands = h.commandRows(r.Context(), lang, agent, deploy)
	for _, c := range ad.Commands {
		if c.StatusCls == "wait" || c.StatusCls == "run" {
			ad.Polling = true
		}
	}
	return ad, true
}

func (h *hubServer) uiActions(w http.ResponseWriter, r *http.Request, _ string) {
	if ad, ok := h.actionsFor(w, r); ok {
		h.renderTemplate(w, http.StatusOK, "actions", ad)
	}
}

func (h *hubServer) uiConfirm(w http.ResponseWriter, r *http.Request, _ string) {
	ad, ok := h.actionsFor(w, r)
	if !ok {
		return
	}
	ad.Action = r.PathValue("action")
	switch ad.Action {
	case hubCommandActionRun:
		ad.Question = hubT(ad.Lang, "dp.confirm_run", ad.Deploy, ad.Agent)
	case hubCommandActionRB:
		ad.Question = hubT(ad.Lang, "dp.confirm_rb", ad.Deploy, ad.Agent)
	default:
		http.NotFound(w, r)
		return
	}
	h.renderTemplate(w, http.StatusOK, "actions", ad)
}

func (h *hubServer) uiCommand(w http.ResponseWriter, r *http.Request, who string) {
	action := r.PathValue("action")
	_, err := h.queueCommand(r.Context(), hubCommand{Agent: r.PathValue("agent"), Deploy: r.PathValue("deploy"), Action: action, By: h.actor(r, who)})
	ad, ok := h.actionsFor(w, r)
	if !ok {
		return
	}
	if err != nil {
		ad.NoCommands = err.Error()
	}
	h.renderTemplate(w, http.StatusOK, "actions", ad)
}
