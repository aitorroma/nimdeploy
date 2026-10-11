package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestHubCommands: the hub queues a command, the agent's long poll picks it
// up, runs the deploy and the hub sees the result.
func TestHubCommands(t *testing.T) {
	store := testHubStore(t)
	h := &hubServer{store: store, opts: &hubOptions{uiToken: testHubToken}, started: time.Now()}
	srv := httptest.NewServer(h.routes())
	t.Cleanup(srv.Close)
	ctx := context.Background()
	token, _ := store.addAgent(ctx, "cmd-1")
	t.Setenv("TEST_HUB_TOKEN", token)
	e := newEnv(t, `echo "run by $DEPLOY_PUSHER trigger=$DEPLOY_TRIGGER"`, `[server]
api_token_env = "TEST_API_TOKEN"
[hub]
url = "`+srv.URL+`"
agent = "cmd-1"
token_env = "TEST_HUB_TOKEN"
heartbeat = "10s"
accept_commands = true
`)
	_ = e
	// The first heartbeat tells the hub the agent accepts commands.
	waitFor(t, "heartbeat", func() bool { a, _, _ := store.agent(ctx, "cmd-1"); return a.AcceptsCommands })

	c, err := h.queueCommand(ctx, hubCommand{Agent: "cmd-1", Deploy: "agency", Action: hubCommandActionRun, By: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "command done", func() bool {
		cs, _ := store.commandsFor(ctx, "cmd-1", "agency", 5)
		return len(cs) == 1 && cs[0].Status == "done"
	})
	cs, _ := store.commandsFor(ctx, "cmd-1", "agency", 5)
	if cs[0].Result != StatusSuccess || cs[0].ID != c.ID {
		t.Errorf("command %+v", cs[0])
	}
	if out := readLatest(t, e, "agency"); !strings.Contains(out, "run by hub:ana trigger=hub") {
		t.Errorf("log:\n%s", out)
	}

	// An unknown deploy comes back refused.
	if _, err := h.queueCommand(ctx, hubCommand{Agent: "cmd-1", Deploy: "nope", Action: hubCommandActionRun, By: "ana"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "refused", func() bool {
		cs, _ := store.commandsFor(ctx, "cmd-1", "nope", 5)
		return len(cs) == 1 && cs[0].Status == "refused" && strings.Contains(cs[0].Result, "no deploy nope")
	})

	// Agents that didn't opt in get no commands.
	store.addAgent(ctx, "quiet")
	if _, err := h.queueCommand(ctx, hubCommand{Agent: "quiet", Deploy: "x", Action: hubCommandActionRun}); err == nil || !strings.Contains(err.Error(), "accept_commands") {
		t.Errorf("command for an agent without accept_commands: %v", err)
	}
	if _, err := h.queueCommand(ctx, hubCommand{Agent: "cmd-1", Deploy: "agency", Action: "rm -rf"}); err == nil {
		t.Error("unknown action accepted")
	}
}

// TestCommandAnswerMustBeSigned: an answer the agent's hub didn't sign is ignored.
func TestCommandAnswerMustBeSigned(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(hubTimestampHeader, "1")
		_, _ = w.Write([]byte(`{"commands":[{"id":"evil","deploy":"agency","action":"run"}]}`))
	}))
	t.Cleanup(fake.Close)
	a := &hubAgent{cfg: HubConfig{URL: fake.URL, Agent: "x", token: "t0ken-t0ken"}, client: &http.Client{}, stop: make(chan struct{})}
	if cmds, err := a.fetchCommands(a.cfg); err == nil || len(cmds) != 0 {
		t.Errorf("unsigned answer accepted: %v %v", cmds, err)
	}
}

// TestHubAgentCA: certificates issued by the hub's CA, checked by fingerprint.
func TestHubAgentCA(t *testing.T) {
	store := testHubStore(t)
	dir := t.TempDir()
	ca, err := loadOrCreateCA(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := loadOrCreateCA(filepath.Join(dir, "ca")); err != nil || !again.cert.Equal(ca.cert) {
		t.Fatalf("reloading the CA: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "ca", hubCAKeyFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("CA key mode %v", fi.Mode())
	}
	p := newTestPKI(t) // the hub's server certificate
	srvCert, srvKey := p.issue(t, "hub", true)
	h := &hubServer{store: store, opts: &hubOptions{uiToken: testHubToken}, ca: ca}
	tc, err := serverTLS(srvCert, srvKey, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	base := serveTLS(t, h.routes(), withClientCA(tc, ca.cert))
	ctx := context.Background()

	issue := func(agent string) (string, string, issuedCert) {
		ic, err := ca.issue(agent)
		if err != nil {
			t.Fatal(err)
		}
		cf, kf := filepath.Join(dir, agent+".pem"), filepath.Join(dir, agent+".key")
		_ = os.WriteFile(cf, []byte(ic.CertPEM), 0o600)
		_ = os.WriteFile(kf, []byte(ic.KeyPEM), 0o600)
		return cf, kf, ic
	}
	cf, kf, ic := issue("prod-1")
	token, err := store.createAgent(ctx, newAgentOpts{Name: "prod-1", Client: "Acme", Env: "production", RequireCert: true, CertFP: ic.Fingerprint, CertNotAfter: ic.NotAfter})
	if err != nil {
		t.Fatal(err)
	}
	send := func(cert, key string) error {
		a := &hubAgent{cfg: HubConfig{URL: base, Agent: "prod-1", token: token, CAFile: p.caFile, CertFile: cert, KeyFile: key}}
		a.client = hubClient(a.cfg)
		return a.post(ctx, hubBatch{})
	}
	if err := send(cf, kf); err != nil {
		t.Errorf("own certificate: %v", err)
	}
	if err := send("", ""); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("without a certificate: %v", err)
	}
	ocf, okf, _ := issue("other")
	if err := send(ocf, okf); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("another agent's certificate: %v", err)
	}
	// Renewing the agent issues a new certificate: the old one stops working.
	ic2, err := ca.issue("prod-1") // not written over the old files
	if err != nil {
		t.Fatal(err)
	}
	token, _ = store.createAgent(ctx, newAgentOpts{Name: "prod-1", RequireCert: true, CertFP: ic2.Fingerprint, CertNotAfter: ic2.NotAfter})
	if err := send(cf, kf); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("old certificate after renewing: %v", err)
	}
	a, _, _ := store.agent(ctx, "prod-1")
	if a.Client != "Acme" || a.Env != "production" || !a.RequireCert || time.Until(a.CertNotAfter) < 360*24*time.Hour {
		t.Errorf("agent %+v", a)
	}
}
