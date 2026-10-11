package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Notifications from the hub. A destination (channel) is where messages go:
// Slack, Discord, Telegram, a JSON webhook or email. A rule picks events,
// filters them by client, environment, agent and deploy (empty: any), and
// sends them to one or more destinations, in English or Spanish.

const (
	notifyDeployFailed    = "deploy.failed"
	notifyDeployRecovered = "deploy.recovered"
	notifyDeploySuccess   = "deploy.success"
	notifyDeployRejected  = "deploy.rejected"
	notifyAgentOffline    = "agent.offline"
	notifyAgentBack       = "agent.back"
	notifyCertExpiring    = "cert.expiring"
	notifyCommandRefused  = "command.refused"

	notifyWatchEvery = 30 * time.Second
	notifyAttempts   = 3
)

var notifyEvents = []string{notifyDeployFailed, notifyDeployRecovered, notifyDeploySuccess, notifyDeployRejected,
	notifyAgentOffline, notifyAgentBack, notifyCertExpiring, notifyCommandRefused}

var notifyKinds = []string{"slack", "discord", "telegram", "webhook", "email"}

// notifyRetryDelays are the waits before the 2nd and 3rd attempt; tests shorten them.
var notifyRetryDelays = []time.Duration{5 * time.Second, 30 * time.Second}

type notifyChannel struct {
	ID, Name, Kind string
	Config         channelConfig
	CreatedAt      time.Time
	LastAt         time.Time
	LastError      string
}

// channelConfig holds a destination's settings, secrets included. It is
// stored in the hub's database and never shown back in the dashboard.
type channelConfig struct {
	URL      string   `json:"url,omitempty"` // slack, discord, webhook
	BotToken string   `json:"bot_token,omitempty"`
	ChatID   string   `json:"chat_id,omitempty"`
	SMTPHost string   `json:"smtp_host,omitempty"`
	SMTPPort int      `json:"smtp_port,omitempty"`
	SMTPTLS  string   `json:"smtp_tls,omitempty"`
	SMTPUser string   `json:"smtp_user,omitempty"`
	SMTPPass string   `json:"smtp_password,omitempty"`
	From     string   `json:"from,omitempty"`
	To       []string `json:"to,omitempty"`
}

func (c *channelConfig) validate(kind string) error {
	switch kind {
	case "slack", "discord", "webhook":
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("the URL must start with https:// (or http://)")
		}
	case "telegram":
		if c.BotToken == "" || c.ChatID == "" {
			return errors.New("the bot token and the chat id are required")
		}
	case "email":
		if len(c.To) == 0 {
			return errors.New("at least one recipient")
		}
		for _, a := range c.To {
			if _, err := mail.ParseAddress(a); err != nil {
				return fmt.Errorf("recipient %q: %w", a, err)
			}
		}
		if _, err := c.smtp(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	return nil
}

func (c channelConfig) smtp() (*SMTPConfig, error) {
	s := &SMTPConfig{Host: c.SMTPHost, Port: c.SMTPPort, TLS: c.SMTPTLS, From: c.From}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("SMTP: %w", err)
	}
	s.user, s.password = c.SMTPUser, c.SMTPPass
	return s, nil
}

// target says where a destination sends, without its secrets.
func (c notifyChannel) target() string {
	switch c.Kind {
	case "telegram":
		return "chat " + c.Config.ChatID
	case "email":
		to := make([]string, len(c.Config.To))
		for i, a := range c.Config.To {
			to[i] = maskEmail(a)
		}
		return strings.Join(to, ", ")
	}
	u, err := url.Parse(c.Config.URL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/…"
}

type notifyRule struct {
	ID, Name, Lang                                   string
	Enabled                                          bool
	Events, Clients, Envs, Agents, Deploys, Channels []string
	CreatedAt                                        time.Time
}

func (r notifyRule) matches(n hubNotice) bool {
	if !r.Enabled || !slices.Contains(r.Events, n.Event) {
		return false
	}
	in := func(list []string, v string) bool {
		return len(list) == 0 || slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, v) })
	}
	if !in(r.Clients, n.Client) || !in(r.Envs, n.Env) || !in(r.Agents, n.Agent) {
		return false
	}
	// Agent events have no deploy: a deploy filter doesn't hold them back.
	return n.Deploy == "" || in(r.Deploys, n.Deploy)
}

// hubNotice is one thing worth telling: what happened, where, and the details
// to render in the rule's language.
type hubNotice struct {
	Event, Agent, Deploy, Client, Env string
	State                             *State
	Reason                            string
	LogTail                           []string
	LastSeen, CertNotAfter            time.Time
	Link                              string
	Time                              time.Time
}

func (n hubNotice) title(lang string) string {
	if n.Event == "test" {
		return hubT(lang, "nt.test")
	}
	where := strings.Join(nonEmpty(n.Client, n.Env), " · ")
	if where != "" {
		where = " · " + where
	}
	switch n.Event {
	case notifyAgentOffline, notifyAgentBack, notifyCertExpiring:
		if n.Event == notifyCertExpiring {
			return hubT(lang, "nt."+n.Event, n.Agent, n.CertNotAfter.Format("2006-01-02")) + where
		}
		return hubT(lang, "nt."+n.Event, n.Agent) + where
	}
	return hubT(lang, "nt."+n.Event, n.Deploy) + where + " (" + n.Agent + ")"
}

func (n hubNotice) lines(lang string) []string {
	var out []string
	if st := n.State; st != nil {
		var parts []string
		if st.Repository != "" {
			parts = append(parts, strings.Trim(st.Repository+"@"+st.Branch, "@"))
		}
		if st.Commit != "" {
			c := shortSHA(st.Commit)
			if st.Message != "" {
				c += " " + st.Message
			}
			parts = append(parts, c)
		}
		if st.Pusher != "" {
			parts = append(parts, hubT(lang, "nt.by", st.Pusher, st.Trigger))
		}
		if len(parts) > 0 {
			out = append(out, strings.Join(parts, " · "))
		}
		if st.Duration != "" {
			out = append(out, hubT(lang, "nt.duration", st.Duration))
		}
		if st.Status != StatusSuccess && st.Error != "" {
			out = append(out, hubT(lang, "nt.error", st.Error))
		}
		if st.Ansible != nil {
			out = append(out, "ansible: "+st.Ansible.text())
		}
	}
	if n.Reason != "" && (n.State == nil || n.State.Error != n.Reason) {
		out = append(out, hubT(lang, "nt.reason", n.Reason))
	}
	if n.Event == notifyAgentOffline && !n.LastSeen.IsZero() {
		out = append(out, hubT(lang, "nt.last_seen", agoIn(lang, time.Since(n.LastSeen))))
	}
	return out
}

// text is the message body: title, details, link and, for failures, the log
// tail (in a code block for Slack and Discord).
func (n hubNotice) text(lang string, fences bool) string {
	var b strings.Builder
	b.WriteString(n.title(lang))
	for _, l := range n.lines(lang) {
		b.WriteString("\n" + l)
	}
	if n.Link != "" {
		b.WriteString("\n" + n.Link)
	}
	if len(n.LogTail) > 0 {
		tail := strings.Join(n.LogTail, "\n")
		if room := maxMessageChars - b.Len() - 10; len(tail) > room {
			tail = "…" + tail[max(0, len(tail)-room):]
		}
		if fences {
			fmt.Fprintf(&b, "\n```\n%s\n```", tail)
		} else {
			fmt.Fprintf(&b, "\n\n%s", tail)
		}
	}
	return b.String()
}

// hubNotifier sends notices to the destinations of the rules they match. It
// never blocks the request that produced them.
type hubNotifier struct {
	store       *hubStore
	baseURL     string // the hub's public URL, for links; empty: no links
	client      *http.Client
	telegramAPI string
	sendMail    func(*SMTPConfig, *outgoing) error
	wg          sync.WaitGroup
}

func newHubNotifier(store *hubStore, baseURL string) *hubNotifier {
	return &hubNotifier{store: store, baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{Timeout: notifyTimeout},
		telegramAPI: "https://api.telegram.org", sendMail: sendSMTP}
}

// wait is for tests and shutdown: it returns when every notice was sent.
func (n *hubNotifier) wait() { n.wg.Wait() }

func (n *hubNotifier) dispatch(notices []hubNotice) {
	if n == nil || len(notices) == 0 {
		return
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rules, err := n.store.notifyRules(ctx)
		var chans []notifyChannel
		if err == nil {
			chans, err = n.store.notifyChannels(ctx)
		}
		cancel()
		if err != nil {
			log.Printf("hub: notifications: %v", err)
			return
		}
		byID := map[string]notifyChannel{}
		for _, c := range chans {
			byID[c.ID] = c
		}
		for _, nt := range notices {
			nt.Link = n.link(nt)
			sent := map[string]bool{} // a destination gets a notice once, whatever rules match
			for _, r := range rules {
				if !r.matches(nt) {
					continue
				}
				for _, id := range r.Channels {
					c, ok := byID[id]
					if !ok || sent[id] {
						continue
					}
					sent[id] = true
					n.wg.Add(1)
					go func() {
						defer n.wg.Done()
						n.deliverWithRetry(c, nt, firstNonEmpty(r.Lang, "en"))
					}()
				}
			}
		}
	}()
}

func (n *hubNotifier) link(nt hubNotice) string {
	if n.baseURL == "" {
		return ""
	}
	if nt.Deploy != "" {
		return n.baseURL + deployHref(nt.Agent, nt.Deploy)
	}
	return n.baseURL + "/agents#agent-" + url.PathEscape(nt.Agent)
}

func (n *hubNotifier) deliverWithRetry(c notifyChannel, nt hubNotice, lang string) {
	var err error
	for i := 0; i < notifyAttempts; i++ {
		if i > 0 {
			time.Sleep(notifyRetryDelays[min(i-1, len(notifyRetryDelays)-1)])
		}
		if err = n.deliver(c, nt, lang); err == nil {
			break
		}
	}
	if err != nil {
		log.Printf("hub: notification %s to %q (%s) failed: %v", nt.Event, c.Name, c.Kind, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if serr := n.store.channelResult(ctx, c.ID, err); serr != nil {
		log.Printf("hub: notifications: %v", serr)
	}
}

func (n *hubNotifier) deliver(c notifyChannel, nt hubNotice, lang string) error {
	poster := &Notifier{client: n.client}
	switch c.Kind {
	case "slack":
		return poster.post(c.Config.URL, map[string]string{"text": nt.text(lang, true)}, nil)
	case "discord":
		return poster.post(c.Config.URL, map[string]string{"content": nt.text(lang, true)}, nil)
	case "telegram":
		return poster.post(fmt.Sprintf("%s/bot%s/sendMessage", n.telegramAPI, c.Config.BotToken), map[string]any{
			"chat_id": c.Config.ChatID, "text": nt.text(lang, false), "disable_web_page_preview": true}, nil)
	case "webhook":
		return poster.post(c.Config.URL, map[string]any{
			"event": nt.Event, "agent": nt.Agent, "deploy": nt.Deploy, "client": nt.Client, "environment": nt.Env,
			"title": nt.title(lang), "lines": nt.lines(lang), "state": nt.State, "log_tail": nt.LogTail,
			"link": nt.Link, "time": nt.Time.UTC()}, nil)
	case "email":
		s, err := c.Config.smtp()
		if err != nil {
			return err
		}
		msg := buildMessage(c.Config.From, c.Config.To, nt.title(lang), nt.text(lang, false), false)
		return n.sendMail(s, &outgoing{From: s.from.Address, To: c.Config.To, Rcpt: c.Config.To, Msg: msg, About: "hub notification"})
	}
	return fmt.Errorf("unknown kind %q", c.Kind)
}

// --- what to tell -------------------------------------------------------------------------------

// noticesFor turns a batch of agent events into notices. It runs before the
// batch is stored, to tell resent events (from the agent's outbox) from new
// ones, and a recovery from another success.
func (h *hubServer) noticesFor(ctx context.Context, a hubAgentRow, events []hubEvent) []hubNotice {
	if h.notifier == nil {
		return nil
	}
	rules, err := h.store.notifyRules(ctx)
	if err != nil || !slices.ContainsFunc(rules, func(r notifyRule) bool { return r.Enabled }) {
		return nil
	}
	var ids []string
	for _, ev := range events {
		ids = append(ids, ev.ID)
	}
	known, err := h.store.knownEvents(ctx, ids)
	if err != nil {
		log.Printf("hub: notifications: %v", err)
		return nil
	}
	var out []hubNotice
	for _, ev := range events {
		base := hubNotice{Agent: a.Name, Deploy: ev.Deploy, Client: a.ClientName(), Env: a.EnvName(), State: ev.State,
			Reason: ev.Reason, Time: ev.Time}
		if ev.State != nil {
			base.Client = firstNonEmpty(ev.State.Labels["client"], base.Client)
			base.Env = firstNonEmpty(ev.State.Labels["environment"], base.Env)
		}
		switch ev.Type {
		case hubEventCommandRefused:
			if st, _ := h.store.commandStatus(ctx, ev.ID); st != "" && st != "refused" {
				base.Event = notifyCommandRefused
				out = append(out, base)
			}
		case hubEventRejected:
			if !known[ev.ID] {
				base.Event = notifyDeployRejected
				out = append(out, base)
			}
		case hubEventDeployDone:
			if known[ev.ID] || ev.State == nil {
				continue
			}
			switch ev.State.Status {
			case StatusSuccess:
				prev, _ := h.store.lastFinishedStatus(ctx, a.Name, ev.Deploy, ev.Time)
				if prev == StatusFailed {
					n := base
					n.Event = notifyDeployRecovered
					out = append(out, n)
				}
				base.Event = notifyDeploySuccess
				out = append(out, base)
			case StatusFailed:
				base.Event = notifyDeployFailed
				base.LogTail = ev.LogTail
				out = append(out, base)
			}
		}
	}
	return out
}

// watchAgents tells when an agent goes offline or comes back, and when its
// certificate is about to expire. Flags in the database keep it to once.
func (h *hubServer) watchAgents(ctx context.Context) {
	t := time.NewTicker(notifyWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := h.checkAgents(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("hub: notifications: %v", err)
		}
	}
}

func (h *hubServer) checkAgents(ctx context.Context, now time.Time) error {
	agents, err := h.store.agents(ctx)
	if err != nil {
		return err
	}
	flags, err := h.store.notifyFlags(ctx)
	if err != nil {
		return err
	}
	var notices []hubNotice
	for _, a := range agents {
		if a.RevokedAt != "" || a.LastSeen.IsZero() {
			continue
		}
		base := hubNotice{Agent: a.Name, Client: a.ClientName(), Env: a.EnvName(), LastSeen: a.LastSeen, Time: now}
		key := "offline:" + a.Name
		switch off := !a.online(now); {
		case off && !flags[key]:
			base.Event = notifyAgentOffline
			notices = append(notices, base)
			err = h.store.setNotifyFlag(ctx, key, true)
		case !off && flags[key]:
			base.Event = notifyAgentBack
			notices = append(notices, base)
			err = h.store.setNotifyFlag(ctx, key, false)
		}
		if err != nil {
			return err
		}
		if a.RequireCert && !a.CertNotAfter.IsZero() && a.CertNotAfter.Sub(now) < certExpiringSoon {
			key := "cert:" + a.Name + ":" + a.CertNotAfter.UTC().Format("2006-01-02")
			if !flags[key] {
				n := base
				n.Event, n.CertNotAfter = notifyCertExpiring, a.CertNotAfter
				notices = append(notices, n)
				if err := h.store.setNotifyFlag(ctx, key, true); err != nil {
					return err
				}
			}
		}
	}
	h.notifier.dispatch(notices)
	return nil
}

// --- storage ---------------------------------------------------------------------------------

func newNotifyID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *hubStore) notifyChannels(ctx context.Context) ([]notifyChannel, error) {
	rows, err := s.db.Query(ctx, `SELECT * FROM notify_channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	out := make([]notifyChannel, 0, len(rows))
	for _, r := range rows {
		c := notifyChannel{ID: r.str("id"), Name: r.str("name"), Kind: r.str("kind"), CreatedAt: parseTime(r.str("created_at")),
			LastAt: parseTime(r.str("last_at")), LastError: r.str("last_error")}
		_ = json.Unmarshal([]byte(r.str("config")), &c.Config)
		out = append(out, c)
	}
	return out, nil
}

func (s *hubStore) saveChannel(ctx context.Context, c notifyChannel) error {
	return s.db.Exec(ctx, sqlStmt{SQL: `INSERT INTO notify_channels (id, name, kind, config, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, kind = excluded.kind, config = excluded.config`,
		Args: []any{c.ID, c.Name, c.Kind, jsonText(c.Config), nowText()}})
}

func (s *hubStore) deleteChannel(ctx context.Context, id string) error {
	return s.db.Exec(ctx, sqlStmt{SQL: `DELETE FROM notify_channels WHERE id = ?`, Args: []any{id}})
}

func (s *hubStore) channelResult(ctx context.Context, id string, sendErr error) error {
	var msg any
	if sendErr != nil {
		msg = clip(sendErr.Error(), 300)
	}
	return s.db.Exec(ctx, sqlStmt{SQL: `UPDATE notify_channels SET last_at = ?, last_error = ? WHERE id = ?`, Args: []any{nowText(), msg, id}})
}

func (s *hubStore) notifyRules(ctx context.Context) ([]notifyRule, error) {
	rows, err := s.db.Query(ctx, `SELECT * FROM notify_rules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	out := make([]notifyRule, 0, len(rows))
	for _, r := range rows {
		n := notifyRule{ID: r.str("id"), Name: r.str("name"), Lang: r.str("lang"), Enabled: r.int("enabled") == 1,
			CreatedAt: parseTime(r.str("created_at"))}
		for col, dst := range map[string]*[]string{"events": &n.Events, "clients": &n.Clients, "envs": &n.Envs,
			"agents": &n.Agents, "deploys": &n.Deploys, "channels": &n.Channels} {
			_ = json.Unmarshal([]byte(r.str(col)), dst)
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *hubStore) saveRule(ctx context.Context, r notifyRule) error {
	list := func(v []string) any {
		if len(v) == 0 {
			return "[]"
		}
		return jsonText(v)
	}
	return s.db.Exec(ctx, sqlStmt{SQL: `INSERT INTO notify_rules (id, name, lang, enabled, events, clients, envs, agents, deploys, channels, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, lang = excluded.lang, enabled = excluded.enabled, events = excluded.events,
			clients = excluded.clients, envs = excluded.envs, agents = excluded.agents, deploys = excluded.deploys, channels = excluded.channels`,
		Args: []any{r.ID, r.Name, r.Lang, r.Enabled, list(r.Events), list(r.Clients), list(r.Envs), list(r.Agents), list(r.Deploys),
			list(r.Channels), nowText()}})
}

func (s *hubStore) setRuleEnabled(ctx context.Context, id string, on bool) error {
	return s.db.Exec(ctx, sqlStmt{SQL: `UPDATE notify_rules SET enabled = ? WHERE id = ?`, Args: []any{on, id}})
}

func (s *hubStore) deleteRule(ctx context.Context, id string) error {
	return s.db.Exec(ctx, sqlStmt{SQL: `DELETE FROM notify_rules WHERE id = ?`, Args: []any{id}})
}

func (s *hubStore) notifyFlags(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.Query(ctx, `SELECT key FROM notify_state`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.str("key")] = true
	}
	return out, nil
}

func (s *hubStore) setNotifyFlag(ctx context.Context, key string, on bool) error {
	if !on {
		return s.db.Exec(ctx, sqlStmt{SQL: `DELETE FROM notify_state WHERE key = ?`, Args: []any{key}})
	}
	return s.db.Exec(ctx, sqlStmt{SQL: `INSERT INTO notify_state (key, updated_at) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`,
		Args: []any{key, nowText()}})
}

// knownEvents says which of these event IDs are already stored.
func (s *hubStore) knownEvents(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.Query(ctx, `SELECT id FROM events WHERE id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.str("id")] = true
	}
	return out, nil
}

// lastFinishedStatus is the status of a deploy's latest finished run before t.
func (s *hubStore) lastFinishedStatus(ctx context.Context, agent, deploy string, before time.Time) (string, error) {
	rows, err := s.db.Query(ctx, `SELECT status FROM events WHERE agent = ? AND deploy = ? AND type = ? AND time < ? ORDER BY time DESC LIMIT 1`,
		agent, deploy, hubEventDeployDone, before.UTC().Format(time.RFC3339Nano))
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0].str("status"), nil
}

func (s *hubStore) commandStatus(ctx context.Context, id string) (string, error) {
	rows, err := s.db.Query(ctx, `SELECT status FROM commands WHERE id = ?`, id)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0].str("status"), nil
}
