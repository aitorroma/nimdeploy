package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNotifyRuleMatch(t *testing.T) {
	r := notifyRule{Enabled: true, Events: []string{notifyDeployFailed, notifyAgentOffline},
		Clients: []string{"Acme", "Beta"}, Agents: []string{"acme-prod", "beta-prod"}, Deploys: []string{"shop"}}
	for _, c := range []struct {
		n    hubNotice
		want bool
	}{
		{hubNotice{Event: notifyDeployFailed, Client: "acme", Agent: "acme-prod", Deploy: "shop"}, true},
		{hubNotice{Event: notifyDeployFailed, Client: "Beta", Agent: "beta-prod", Deploy: "shop"}, true},
		{hubNotice{Event: notifyDeployFailed, Client: "Acme", Agent: "acme-stage", Deploy: "shop"}, false},
		{hubNotice{Event: notifyDeployFailed, Client: "Acme", Agent: "acme-prod", Deploy: "docs"}, false},
		{hubNotice{Event: notifyDeploySuccess, Client: "Acme", Agent: "acme-prod", Deploy: "shop"}, false},
		{hubNotice{Event: notifyAgentOffline, Client: "Acme", Agent: "acme-prod"}, true}, // no deploy: the deploy filter doesn't apply
		{hubNotice{Event: notifyAgentOffline, Client: "Gamma", Agent: "acme-prod"}, false},
	} {
		if got := r.matches(c.n); got != c.want {
			t.Errorf("%+v: %v, want %v", c.n, got, c.want)
		}
	}
	if (notifyRule{Events: []string{notifyDeployFailed}}).matches(hubNotice{Event: notifyDeployFailed}) {
		t.Error("a paused rule matched")
	}
	if !(notifyRule{Enabled: true, Events: []string{notifyDeployFailed}}).matches(hubNotice{Event: notifyDeployFailed, Client: "x"}) {
		t.Error("a rule without filters must match everything")
	}
}

func TestNotifyValidateAndTarget(t *testing.T) {
	bad := []struct {
		kind string
		c    channelConfig
	}{
		{"slack", channelConfig{URL: "ftp://x"}},
		{"telegram", channelConfig{BotToken: "t"}},
		{"email", channelConfig{To: []string{"ops@example.com"}, SMTPHost: "smtp.example.com", From: "not an address"}},
		{"email", channelConfig{To: []string{"nope"}, SMTPHost: "smtp.example.com", From: "a@example.com"}},
		{"email", channelConfig{To: []string{"ops@example.com"}, SMTPHost: "smtp.example.com", SMTPTLS: "none", From: "a@example.com"}},
		{"pigeon", channelConfig{}},
	}
	for _, b := range bad {
		if err := b.c.validate(b.kind); err == nil {
			t.Errorf("%s %+v accepted", b.kind, b.c)
		}
	}
	ok := channelConfig{To: []string{"ops@example.com"}, SMTPHost: "smtp.example.com", From: "nimdeploy <a@example.com>"}
	if err := ok.validate("email"); err != nil {
		t.Error(err)
	}
	c := notifyChannel{Kind: "slack", Config: channelConfig{URL: "https://hooks.slack.com/services/T0/B0/secret"}}
	if got := c.target(); got != "https://hooks.slack.com/…" || strings.Contains(got, "secret") {
		t.Errorf("target %q", got)
	}
}

func TestNoticeText(t *testing.T) {
	code := 1
	n := hubNotice{Event: notifyDeployFailed, Agent: "acme-prod", Deploy: "shop", Client: "Acme", Env: "production",
		State: &State{Status: StatusFailed, Repository: "acme/shop", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567",
			Message: "fix footer", Pusher: "ana", Trigger: "push", Duration: "12s", Error: "exit status 1", ExitCode: &code},
		LogTail: []string{"step 1", "ERROR: boom"}, Link: "https://hub.example.com/d/acme-prod/shop"}
	en := n.text("en", true)
	for _, want := range []string{"❌ shop failed · Acme · production (acme-prod)", "acme/shop@main · 0123456 fix footer · by ana (push)",
		"duration 12s", "error: exit status 1", "https://hub.example.com/d/acme-prod/shop", "```\nstep 1\nERROR: boom\n```"} {
		if !strings.Contains(en, want) {
			t.Errorf("missing %q in\n%s", want, en)
		}
	}
	if es := n.text("es", false); !strings.Contains(es, "❌ shop ha fallado") || strings.Contains(es, "```") {
		t.Errorf("spanish:\n%s", es)
	}
}

// signedBatch posts events to the hub as the agent would.
func signedBatch(t *testing.T, base, agent, token string, events ...hubEvent) {
	t.Helper()
	body, _ := json.Marshal(hubBatch{Events: events})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, _ := http.NewRequest("POST", base+hubEventsPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hubAgentHeader, agent)
	req.Header.Set(hubTimestampHeader, ts)
	req.Header.Set(hubSignatureHeader, signHub([]byte(token), ts, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("events: %d", resp.StatusCode)
	}
}

type captured struct {
	mu   sync.Mutex
	got  []map[string]any
	fail int // answer 500 this many times first
}

func (c *captured) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail > 0 {
		c.fail--
		http.Error(w, "busy", 500)
		return
	}
	var m map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &m)
	m["_path"] = r.URL.Path
	c.got = append(c.got, m)
}

func (c *captured) take() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.got
	c.got = nil
	return out
}

func TestHubNotifications(t *testing.T) {
	store := testHubStore(t)
	old := notifyRetryDelays
	notifyRetryDelays = []time.Duration{10 * time.Millisecond}
	t.Cleanup(func() { notifyRetryDelays = old })

	hook := &captured{}
	hookSrv := httptest.NewServer(hook)
	t.Cleanup(hookSrv.Close)
	nf := newHubNotifier(store, "https://hub.example.com/")
	nf.telegramAPI = hookSrv.URL
	var mails []*outgoing
	var mailMu sync.Mutex
	nf.sendMail = func(_ *SMTPConfig, m *outgoing) error {
		mailMu.Lock()
		defer mailMu.Unlock()
		mails = append(mails, m)
		return nil
	}
	h := &hubServer{store: store, opts: &hubOptions{uiToken: testHubToken}, started: time.Now(), notifier: nf}
	srv := httptest.NewServer(h.routes())
	t.Cleanup(srv.Close)
	ctx := context.Background()
	token, _ := store.createAgent(ctx, newAgentOpts{Name: "acme-prod", Client: "Acme", Env: "production"})
	other, _ := store.createAgent(ctx, newAgentOpts{Name: "beta-prod", Client: "Beta", Env: "production"})

	// Destinations and a rule, from the dashboard.
	u := newUIClient(t, srv.URL)
	u.do("GET", "/notifications", nil, false)
	if code, _, _ := u.do("POST", "/ui/notify/channels", url.Values{"name": {"x"}, "kind": {"webhook"}, "url": {hookSrv.URL + "/hook"}}, false); code != http.StatusForbidden {
		t.Errorf("destination without CSRF: %d", code)
	}
	if _, body, _ := u.do("POST", "/ui/notify/channels", url.Values{"name": {"bad"}, "kind": {"webhook"}, "url": {"ftp://x"}}, true); !strings.Contains(body, "https://") {
		t.Errorf("bad URL accepted: %s", body)
	}
	for _, f := range []url.Values{
		{"name": {"ops hook"}, "kind": {"webhook"}, "url": {hookSrv.URL + "/hook"}},
		{"name": {"ops chat"}, "kind": {"telegram"}, "bot_token": {"123:abc"}, "chat_id": {"-100"}},
		{"name": {"ops mail"}, "kind": {"email"}, "smtp_host": {"smtp.example.com"}, "smtp_user": {"u"}, "smtp_password": {"p"},
			"from": {"nimdeploy <deploy@example.com>"}, "to": {"ops@example.com, dev@example.com"}},
	} {
		if code, body, hdr := u.do("POST", "/ui/notify/channels", f, true); code != http.StatusNoContent || hdr.Get("HX-Refresh") != "true" {
			t.Fatalf("destination %s: %d %s", f.Get("name"), code, body)
		}
	}
	chans, _ := store.notifyChannels(ctx)
	ids := map[string]string{}
	for _, c := range chans {
		ids[c.Name] = c.ID
	}
	// Editing keeps the secret when its field is left empty, and never shows it.
	if _, body, _ := u.do("GET", "/ui/notify/channels/"+ids["ops chat"], nil, false); strings.Contains(body, "123:abc") || !strings.Contains(body, `value="-100"`) {
		t.Errorf("edit form shows the token or lost the chat id")
	}
	u.do("POST", "/ui/notify/channels", url.Values{"id": {ids["ops chat"]}, "name": {"ops chat"}, "kind": {"telegram"}, "chat_id": {"-200"}}, true)
	chans, _ = store.notifyChannels(ctx)
	for _, c := range chans {
		if c.Name == "ops chat" && (c.Config.BotToken != "123:abc" || c.Config.ChatID != "-200") {
			t.Errorf("edited telegram: %+v", c.Config)
		}
	}
	if _, body, _ := u.do("POST", "/ui/notify/rules", url.Values{"name": {"r"}, "event": {notifyDeployFailed}}, true); !strings.Contains(body, "destination") {
		t.Errorf("rule without destinations accepted: %s", body)
	}
	rule := url.Values{"name": {"Acme production"}, "lang": {"en"}, "enabled": {"on"},
		"event":   {notifyDeployFailed, notifyDeployRecovered, notifyAgentOffline, notifyAgentBack},
		"client":  {"Acme"},
		"agent":   {"acme-prod"},
		"channel": {ids["ops hook"], ids["ops chat"], ids["ops mail"]}}
	if code, body, _ := u.do("POST", "/ui/notify/rules", rule, true); code != http.StatusNoContent {
		t.Fatalf("rule: %d %s", code, body)
	}
	if _, body, _ := u.do("GET", "/notifications", nil, false); !strings.Contains(body, "Acme production") || !strings.Contains(body, "Deploy failed") ||
		!strings.Contains(body, "ops hook") || strings.Contains(body, hookSrv.URL+"/hook") {
		t.Errorf("notifications page")
	}

	// A failure is sent to the three destinations, once; the webhook fails once and is retried.
	hook.fail = 1
	now := time.Now().UTC()
	failed := hubEvent{ID: "e1", Type: hubEventDeployDone, Time: now, Deploy: "shop",
		State:   &State{Deploy: "shop", Status: StatusFailed, Error: "exit status 1", Labels: map[string]string{"client": "Acme", "environment": "production"}},
		LogTail: []string{"ERROR: boom"}}
	signedBatch(t, srv.URL, "acme-prod", token, failed)
	nf.wait()
	got := hook.take()
	var hookMsg, tg map[string]any
	for _, m := range got {
		switch m["_path"] {
		case "/hook":
			hookMsg = m
		case "/bot123:abc/sendMessage":
			tg = m
		}
	}
	if len(got) != 2 || hookMsg["event"] != notifyDeployFailed || hookMsg["link"] != "https://hub.example.com/d/acme-prod/shop" ||
		!strings.Contains(tg["text"].(string), "ERROR: boom") || tg["chat_id"] != "-200" {
		t.Fatalf("failure notices: %v", got)
	}
	if len(mails) != 1 || !strings.Contains(string(mails[0].Msg), "shop failed") || len(mails[0].Rcpt) != 2 {
		t.Fatalf("mail: %+v", mails)
	}
	// The agent resends it from its outbox: nothing new.
	signedBatch(t, srv.URL, "acme-prod", token, failed)
	nf.wait()
	if got := hook.take(); len(got) != 0 {
		t.Errorf("resent event notified again: %v", got)
	}
	// Another server's failure doesn't match the rule.
	signedBatch(t, srv.URL, "beta-prod", other, hubEvent{ID: "b1", Type: hubEventDeployDone, Time: now, Deploy: "shop",
		State: &State{Deploy: "shop", Status: StatusFailed}})
	nf.wait()
	if got := hook.take(); len(got) != 0 {
		t.Errorf("other client notified: %v", got)
	}
	// The next success is a recovery (success itself isn't in the rule).
	signedBatch(t, srv.URL, "acme-prod", token, hubEvent{ID: "e2", Type: hubEventDeployDone, Time: now.Add(time.Minute), Deploy: "shop",
		State: &State{Deploy: "shop", Status: StatusSuccess}})
	nf.wait()
	got = hook.take()
	if len(got) != 2 || !strings.Contains(firstString(got, "/bot123:abc/sendMessage", "text"), "✅ shop recovered") {
		t.Errorf("recovery: %v", got)
	}
	// A second success says nothing.
	signedBatch(t, srv.URL, "acme-prod", token, hubEvent{ID: "e3", Type: hubEventDeployDone, Time: now.Add(2 * time.Minute), Deploy: "shop",
		State: &State{Deploy: "shop", Status: StatusSuccess}})
	nf.wait()
	if got := hook.take(); len(got) != 0 {
		t.Errorf("plain success notified: %v", got)
	}

	// Offline and back: once each.
	future := time.Now().Add(time.Hour)
	if err := h.checkAgents(ctx, future); err != nil {
		t.Fatal(err)
	}
	nf.wait()
	got = hook.take()
	if len(got) != 2 || firstString(got, "/hook", "event") != notifyAgentOffline {
		t.Errorf("offline: %v", got)
	}
	_ = h.checkAgents(ctx, future)
	nf.wait()
	if got := hook.take(); len(got) != 0 {
		t.Errorf("offline twice: %v", got)
	}
	_ = h.checkAgents(ctx, time.Now())
	nf.wait()
	if got := hook.take(); len(got) != 2 || firstString(got, "/hook", "event") != notifyAgentBack {
		t.Errorf("back: %v", got)
	}

	// Paused, nothing goes out; the test button still works.
	rules, _ := store.notifyRules(ctx)
	u.do("POST", "/ui/notify/rules/"+rules[0].ID+"/toggle", url.Values{"on": {"0"}}, true)
	signedBatch(t, srv.URL, "acme-prod", token, hubEvent{ID: "e4", Type: hubEventDeployDone, Time: now.Add(3 * time.Minute), Deploy: "shop",
		State: &State{Deploy: "shop", Status: StatusFailed}})
	nf.wait()
	if got := hook.take(); len(got) != 0 {
		t.Errorf("paused rule sent: %v", got)
	}
	if _, body, _ := u.do("POST", "/ui/notify/channels/"+ids["ops hook"]+"/test", url.Values{}, true); !strings.Contains(body, "Sent") {
		t.Errorf("test: %s", body)
	}
	if got := hook.take(); len(got) != 1 || got[0]["event"] != "test" {
		t.Errorf("test message: %v", got)
	}
	u.do("POST", "/ui/notify/rules/"+rules[0].ID+"/delete", url.Values{}, true)
	u.do("POST", "/ui/notify/channels/"+ids["ops mail"]+"/delete", url.Values{}, true)
	rules, _ = store.notifyRules(ctx)
	chans, _ = store.notifyChannels(ctx)
	if len(rules) != 0 || len(chans) != 2 {
		t.Errorf("after deleting: %d rules, %d destinations", len(rules), len(chans))
	}
}

func firstString(msgs []map[string]any, path, key string) string {
	for _, m := range msgs {
		if m["_path"] == path {
			s, _ := m[key].(string)
			return s
		}
	}
	return ""
}
