package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	testSecret = "s3cret"
	testToken  = "t0ken"
	sha1       = "1111111111111111111111111111111111111111"
	sha2       = "2222222222222222222222222222222222222222"
	sha3       = "3333333333333333333333333333333333333333"
)

type env struct {
	cfg    *Config
	runner *Runner
	h      http.Handler
	logDir string
	dir    string
}

// newEnv builds a config running script for deploy "agency". extra is
// appended to the TOML (e.g. [server], [notify] sections).
func newEnv(t *testing.T, script, extra string) *env {
	t.Helper()
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	t.Setenv("TEST_WEBHOOK_SECRET", testSecret)
	t.Setenv("TEST_API_TOKEN", testToken)
	if os.Getenv("TEST_NOTIFY_URL") == "" {
		t.Setenv("TEST_NOTIFY_URL", "unused")
	}
	t.Setenv("TEST_GITHUB_TOKEN", "gh-token")
	toml := extra + `
[logging]
directory = "` + logDir + `"
retain = 3

[deploy.agency]
path = "/hooks/agency"
repository = "acme/agency"
branch = "main"
secret_env = "TEST_WEBHOOK_SECRET"
working_directory = "` + dir + `"
command = "/bin/sh"
args = ["-c", ` + jsonQuote(script) + `]
timeout = "5s"
`
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveSecrets(); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(cfg, NewNotifier(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.Shutdown(5 * time.Second) })
	return &env{cfg: cfg, runner: runner, h: NewServer(cfg, runner).Routes(), logDir: logDir, dir: dir}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type pushOpts struct {
	ref, commit, delivery, event, contentType string
	badSig                                    bool
}

func (e *env) push(t *testing.T, o pushOpts) *httptest.ResponseRecorder {
	t.Helper()
	if o.ref == "" {
		o.ref = "refs/heads/main"
	}
	if o.commit == "" {
		o.commit = sha1
	}
	if o.event == "" {
		o.event = "push"
	}
	body := []byte(`{"ref":"` + o.ref + `","after":"` + o.commit + `","repository":{"full_name":"acme/agency"},"pusher":{"name":"dev"}}`)
	if o.contentType == "application/x-www-form-urlencoded" {
		body = []byte("payload=" + url.QueryEscape(string(body)))
	}
	if o.contentType == "" {
		o.contentType = "application/json"
	}
	sig := sign(body)
	if o.badSig {
		sig = "sha256=" + strings.Repeat("0", 64)
	}
	req := httptest.NewRequest(http.MethodPost, "/hooks/agency", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", o.contentType)
	req.Header.Set("X-GitHub-Event", o.event)
	req.Header.Set("X-GitHub-Delivery", o.delivery)
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) request(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// waitIdle waits until nothing is running or queued for deploy "agency".
func (e *env) waitIdle(t *testing.T) State {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := e.runner.State("agency"); !isActive(st.Status) && st.Queued == nil {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("deploy did not finish")
	return State{}
}

func (e *env) logs(t *testing.T) []string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(e.logDir, "agency", "2*.log"))
	return matches
}

func TestDeploySuccess(t *testing.T) {
	e := newEnv(t, `echo "building $DEPLOY_COMMIT"; echo "secrets=$TEST_WEBHOOK_SECRET"`, "")

	rec := e.push(t, pushOpts{delivery: "51af02d4-1234"})
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"result": "started"`) {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	st := e.waitIdle(t)
	if st.Status != StatusSuccess || *st.ExitCode != 0 || st.Trigger != TriggerWebhook {
		t.Fatalf("unexpected state %+v", st)
	}
	if !strings.HasSuffix(st.Log, "-51af02.log") {
		t.Fatalf("log name %q", st.Log)
	}
	if st.StartedAt.Nanosecond() != 0 || st.FinishedAt.Nanosecond() != 0 {
		t.Errorf("timestamps not truncated to seconds: %v %v", st.StartedAt, st.FinishedAt)
	}

	out, err := os.ReadFile(filepath.Join(e.logDir, "agency", "latest.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"deploy=agency status=started", "trigger=webhook", "delivery=51af02d4-1234", "building " + sha1, "status=success", "exit_code=0", "secrets=\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}

	// State survives a restart.
	runner2, _ := NewRunner(e.cfg, nil)
	if got := runner2.State("agency"); got.Status != StatusSuccess || got.Log != st.Log {
		t.Fatalf("persisted state %+v", got)
	}
}

func TestDeployFailure(t *testing.T) {
	e := newEnv(t, `echo boom >&2; exit 3`, "")
	e.push(t, pushOpts{delivery: "aaa"})
	st := e.waitIdle(t)
	if st.Status != StatusFailed || *st.ExitCode != 3 {
		t.Fatalf("unexpected state %+v", st)
	}
	out, _ := os.ReadFile(filepath.Join(e.logDir, "agency", st.Log))
	if !strings.Contains(string(out), "boom") || !strings.Contains(string(out), `error="exit status 3"`) {
		t.Fatalf("log:\n%s", out)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	e := newEnv(t, `sleep 30 & echo $! > child.pid; wait`, "")
	e.cfg.Deploy["agency"].Timeout.Duration = 300 * time.Millisecond
	e.push(t, pushOpts{})
	st := e.waitIdle(t)
	if st.Status != StatusFailed || !strings.Contains(st.Error, "timeout") {
		t.Fatalf("unexpected state %+v", st)
	}
	b, _ := os.ReadFile(filepath.Join(e.dir, "child.pid"))
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("child pid: %v", err)
	}
	for i := 0; syscall.Kill(pid, 0) == nil; i++ {
		if i == 40 {
			t.Fatalf("child %d still alive after timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWebhookFiltering(t *testing.T) {
	e := newEnv(t, `true`, "")

	if rec := e.push(t, pushOpts{badSig: true}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: code %d", rec.Code)
	}
	if rec := e.push(t, pushOpts{event: "ping"}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("ping: code %d %s", rec.Code, rec.Body)
	}
	if rec := e.push(t, pushOpts{event: "issues"}); !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("other event: %s", rec.Body)
	}
	if rec := e.push(t, pushOpts{ref: "refs/heads/dev"}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("other branch: code %d %s", rec.Code, rec.Body)
	}
	if len(e.logs(t)) != 0 {
		t.Fatal("filtered webhooks must not create logs")
	}
}

func TestFormEncodedPayload(t *testing.T) {
	e := newEnv(t, `true`, "")
	if rec := e.push(t, pushOpts{contentType: "application/x-www-form-urlencoded", commit: sha2}); rec.Code != http.StatusAccepted {
		t.Fatalf("form payload: code %d %s", rec.Code, rec.Body)
	}
	if st := e.waitIdle(t); st.Commit != sha2 {
		t.Fatalf("commit %q", st.Commit)
	}
}

func TestDuplicateDelivery(t *testing.T) {
	e := newEnv(t, `true`, "")
	e.push(t, pushOpts{delivery: "dup-1"})
	e.waitIdle(t)
	rec := e.push(t, pushOpts{delivery: "dup-1"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("duplicate: code %d %s", rec.Code, rec.Body)
	}
	if n := len(e.logs(t)); n != 1 {
		t.Fatalf("%d logs, want 1", n)
	}
}

func TestQueueCoalescesPushes(t *testing.T) {
	e := newEnv(t, `sleep 0.5; echo "deployed $DEPLOY_COMMIT"`, "")

	if rec := e.push(t, pushOpts{commit: sha1, delivery: "d1"}); !strings.Contains(rec.Body.String(), `"result": "started"`) {
		t.Fatalf("first: %s", rec.Body)
	}
	if rec := e.push(t, pushOpts{commit: sha2, delivery: "d2"}); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"result": "queued"`) {
		t.Fatalf("second: %d %s", rec.Code, rec.Body)
	}
	rec := e.push(t, pushOpts{commit: sha3, delivery: "d3"})
	if !strings.Contains(rec.Body.String(), `"replaced": "`+sha2+`"`) {
		t.Fatalf("third should replace second: %s", rec.Body)
	}
	if st := e.runner.State("agency"); st.Queued == nil || st.Queued.Commit != sha3 {
		t.Fatalf("queued info %+v", st.Queued)
	}

	st := e.waitIdle(t)
	if st.Status != StatusSuccess || st.Commit != sha3 {
		t.Fatalf("final state %+v", st)
	}
	logs := e.logs(t)
	if len(logs) != 2 {
		t.Fatalf("%d logs, want 2 (first + coalesced): %v", len(logs), logs)
	}
}

func TestQueueDisabledRejects(t *testing.T) {
	e := newEnv(t, `sleep 0.5`, "")
	f := false
	e.cfg.Deploy["agency"].Queue = &f
	e.cfg.Deploy["agency"].queue = false
	e.push(t, pushOpts{delivery: "q1"})
	if rec := e.push(t, pushOpts{delivery: "q2"}); rec.Code != http.StatusConflict {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestAPITokenAndManualDeploy(t *testing.T) {
	e := newEnv(t, `echo "manual commit=[$DEPLOY_COMMIT] by $DEPLOY_PUSHER trigger=$DEPLOY_TRIGGER token=[$TEST_API_TOKEN]"`, `
[server]
api_token_env = "TEST_API_TOKEN"
`)
	if rec := e.request("GET", "/status", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status without token: %d", rec.Code)
	}
	if rec := e.request("GET", "/status", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status with bad token: %d", rec.Code)
	}
	if rec := e.request("GET", "/status/agency", testToken, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"never"`) {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if rec := e.request("GET", "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
	if rec := e.request("POST", "/deploy/agency", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("manual without token: %d", rec.Code)
	}
	if rec := e.request("POST", "/deploy/nope", testToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown deploy: %d", rec.Code)
	}
	if rec := e.request("POST", "/deploy/agency", testToken, `{"user":"ana"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("manual: %d %s", rec.Code, rec.Body)
	}
	st := e.waitIdle(t)
	if st.Trigger != TriggerManual || st.Pusher != "ana" || st.Status != StatusSuccess {
		t.Fatalf("state %+v", st)
	}
	out, _ := os.ReadFile(filepath.Join(e.logDir, "agency", st.Log))
	if !strings.Contains(string(out), "manual commit=[] by ana trigger=manual token=[]") {
		t.Fatalf("log:\n%s", out)
	}
}

func TestManualDeployDisabledWithoutToken(t *testing.T) {
	e := newEnv(t, `true`, "")
	if rec := e.request("POST", "/deploy/agency", "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("code %d", rec.Code)
	}
	if rec := e.request("GET", "/status", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("open status: %d", rec.Code)
	}
}

type recorded struct {
	path string
	auth string
	body map[string]any
}

func recorder(t *testing.T) (*httptest.Server, func() []recorded) {
	var mu sync.Mutex
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		got = append(got, recorded{r.URL.Path, r.Header.Get("Authorization"), body})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), got...)
	}
}

func TestNotificationsAndCommitStatus(t *testing.T) {
	srv, got := recorder(t)
	t.Setenv("TEST_NOTIFY_URL", srv.URL+"/notify")
	// Fails the first time, succeeds afterwards.
	e := newEnv(t, `if [ -f ok ]; then echo fine; else touch ok; echo "npm ERR! build failed"; exit 1; fi`, `
[notify]
format = "slack"
on = "failure"
url_env = "TEST_NOTIFY_URL"

[github]
token_env = "TEST_GITHUB_TOKEN"
api_url = "`+srv.URL+`"
`)
	e.push(t, pushOpts{commit: sha1, delivery: "n1"})
	e.waitIdle(t)
	e.push(t, pushOpts{commit: sha2, delivery: "n2"})
	e.waitIdle(t)
	e.push(t, pushOpts{commit: sha3, delivery: "n3"})
	e.waitIdle(t)
	e.runner.Shutdown(5 * time.Second) // waits for notifications

	var statuses, notes []recorded
	for _, r := range got() {
		if r.path == "/notify" {
			notes = append(notes, r)
		} else {
			statuses = append(statuses, r)
		}
	}

	wantStatuses := []string{sha1 + " pending", sha1 + " failure", sha2 + " pending", sha2 + " success", sha3 + " pending", sha3 + " success"}
	if len(statuses) != len(wantStatuses) {
		t.Fatalf("got %d commit statuses: %+v", len(statuses), statuses)
	}
	for i, s := range statuses {
		want := strings.Split(wantStatuses[i], " ")
		if s.path != "/repos/acme/agency/statuses/"+want[0] || s.body["state"] != want[1] ||
			s.body["context"] != "nimdeploy/agency" || s.auth != "Bearer gh-token" {
			t.Errorf("status %d = %+v, want %s", i, s, wantStatuses[i])
		}
	}

	// on = "failure": the failure and the recovery, not the third success.
	if len(notes) != 2 {
		t.Fatalf("got %d notifications: %+v", len(notes), notes)
	}
	failure, _ := notes[0].body["text"].(string)
	recovery, _ := notes[1].body["text"].(string)
	if !strings.Contains(failure, "FAILED") || !strings.Contains(failure, "npm ERR! build failed") || !strings.Contains(failure, "1111111") {
		t.Errorf("failure message:\n%s", failure)
	}
	if !strings.Contains(recovery, "recovered") {
		t.Errorf("recovery message:\n%s", recovery)
	}
}

func TestReloadAddsDeploy(t *testing.T) {
	e := newEnv(t, `true`, "")
	next := *e.cfg
	next.Deploy = map[string]*DeployConfig{"agency": e.cfg.Deploy["agency"]}
	web := *e.cfg.Deploy["agency"]
	web.Name, web.Path = "web", "/hooks/web"
	next.Deploy["web"] = &web
	e.runner.SetConfig(&next, nil)
	h := NewServer(&next, e.runner).Routes()

	req := httptest.NewRequest("GET", "/status/web", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"never"`) {
		t.Fatalf("new deploy after reload: %d %s", rec.Code, rec.Body)
	}
	if _, err := e.runner.Submit("web", Trigger{Source: TriggerManual}); err != nil {
		t.Fatalf("submit to reloaded deploy: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]string{
		"reserved path":  `[deploy.a]` + "\n" + `path = "/status/x"` + "\nrepository = \"a/b\"\nsecret_env = \"X\"\ncommand = \"true\"",
		"unknown key":    `[deploy.a]` + "\n" + `path = "/a"` + "\nrepository = \"a/b\"\nsecret_env = \"X\"\ncommand = \"true\"\ntypo = 1",
		"notify missing": "[notify]\nformat = \"slack\"\n[deploy.a]\npath = \"/a\"\nrepository = \"a/b\"\nsecret_env = \"X\"\ncommand = \"true\"",
	}
	for name, toml := range cases {
		path := filepath.Join(t.TempDir(), "c.toml")
		os.WriteFile(path, []byte(toml), 0o600)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestPruneLogs(t *testing.T) {
	dir := t.TempDir()
	names := []string{"20260101-000000-a.log", "20260102-000000-b.log", "20260103-000000-c.log", "20260104-000000-d.log"}
	for _, n := range names {
		os.WriteFile(filepath.Join(dir, n), nil, 0o640)
	}
	updateLatest(dir, names[3])
	active := map[string]bool{filepath.Join(dir, names[0]): true}

	if err := pruneLogs(dir, 2, active); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := "20260101-000000-a.log 20260103-000000-c.log 20260104-000000-d.log latest.log"
	if got := strings.Join(left, " "); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestReadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	os.WriteFile(path, []byte("# comment\nA=1\nNIMDEPLOY_API_TOKEN=\"abc\"\n"), 0o600)
	if v, err := readEnvFile(path, "NIMDEPLOY_API_TOKEN"); err != nil || v != "abc" {
		t.Fatalf("got %q %v", v, err)
	}
	if _, err := readEnvFile(path, "MISSING"); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientIP(t *testing.T) {
	cfg := &Config{Server: ServerConfig{Listen: "127.0.0.1:9000", SocketMode: "0666", TrustedProxies: []string{"127.0.0.0/8", "::1", "10.0.0.0/8"}}}
	if err := cfg.Server.validate(); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg}
	cases := []struct {
		name, remote, xff, realIP, want string
	}{
		{"direct client ignores headers", "203.0.113.9:5000", "1.2.3.4", "5.6.7.8", "203.0.113.9"},
		{"proxy with XFF", "127.0.0.1:5000", "140.82.115.3", "", "140.82.115.3"},
		{"spoofed XFF entry is skipped", "127.0.0.1:5000", "6.6.6.6, 140.82.115.3", "", "140.82.115.3"},
		{"proxy chain", "127.0.0.1:5000", "140.82.115.3, 10.0.0.5", "", "140.82.115.3"},
		{"X-Real-IP fallback", "[::1]:5000", "", "140.82.115.3", "140.82.115.3"},
		{"proxy without headers", "127.0.0.1:5000", "", "", "127.0.0.1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.realIP != "" {
			r.Header.Set("X-Real-IP", c.realIP)
		}
		if got := s.clientIP(r); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestClientIPCloudflare(t *testing.T) {
	// Cloudflare -> nginx -> nimdeploy: nginx appends the Cloudflare edge IP.
	cfg := &Config{Server: ServerConfig{Listen: "127.0.0.1:9000", SocketMode: "0666", TrustedProxies: []string{"127.0.0.1", "cloudflare"}}}
	if err := cfg.Server.validate(); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 140.82.115.3, 172.70.1.2")
	if got := s.clientIP(r); got != "140.82.115.3" {
		t.Errorf("XFF through cloudflare: got %s", got)
	}

	// With client_ip_header the header wins, but only from a trusted peer.
	cfg.Server.ClientIPHeader = "CF-Connecting-IP"
	r.Header.Set("CF-Connecting-IP", "2001:db8::7")
	if got := s.clientIP(r); got != "2001:db8::7" {
		t.Errorf("CF-Connecting-IP: got %s", got)
	}
	r.RemoteAddr = "203.0.113.9:5000"
	if got := s.clientIP(r); got != "203.0.113.9" {
		t.Errorf("header from untrusted peer must be ignored: got %s", got)
	}
}

func TestBasePath(t *testing.T) {
	e := newEnv(t, `true`, `
[server]
base_path = "/nimdeploy/"
`)
	if rec := e.request("GET", "/nimdeploy/status/agency", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("prefixed status: %d", rec.Code)
	}
	if rec := e.request("GET", "/status/agency", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unprefixed status should 404: %d", rec.Code)
	}
	body := `{"ref":"refs/heads/main","after":"` + sha1 + `","repository":{"full_name":"acme/agency"}}`
	req := httptest.NewRequest("POST", "/nimdeploy/hooks/agency", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sign([]byte(body)))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("prefixed webhook: %d %s", rec.Code, rec.Body)
	}
	e.waitIdle(t)
}

func TestUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nimdeploy.sock")
	e := newEnv(t, `true`, `
[server]
listen = "unix:`+sock+`"
socket_mode = "0660"
base_path = "/nd"
api_token_env = "TEST_API_TOKEN"
`)
	// A stale socket from a crash must not prevent startup.
	stale, err := listen(e.cfg.Server)
	if err != nil {
		t.Fatal(err)
	}
	stale.(interface{ SetUnlinkOnClose(bool) }).SetUnlinkOnClose(false)
	stale.Close()

	ln, err := listen(e.cfg.Server)
	if err != nil {
		t.Fatalf("listen over stale socket: %v", err)
	}
	srv := &http.Server{Handler: e.h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	info, _ := os.Stat(sock)
	if info.Mode().Perm() != 0o660 {
		t.Errorf("socket mode %o", info.Mode().Perm())
	}
	c, err := newClient(e.cfg, "/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	var res SubmitResult
	if _, err := c.do("POST", "/deploy/agency", manualRequest{User: "cli"}, &res); err != nil || res.Result != ResultStarted {
		t.Fatalf("deploy over socket: %v %+v", err, res)
	}
	e.waitIdle(t)
	var st State
	if _, err := c.do("GET", "/status/agency", nil, &st); err != nil || st.Status != StatusSuccess || st.Pusher != "cli" {
		t.Fatalf("status over socket: %v %+v", err, st)
	}
}

func TestNginxSnippet(t *testing.T) {
	e := newEnv(t, `true`, `
[server]
listen = "0.0.0.0:9100"
base_path = "/nd"
`)
	out := nginxSnippet(e.cfg, true)
	for _, want := range []string{"location = /nd/hooks/agency {", "proxy_pass http://127.0.0.1:9100;", "client_max_body_size 25m;", "location ^~ /nd/deploy/ {"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	e.cfg.Server.socketPath = "/run/nimdeploy/nimdeploy.sock"
	if out := nginxSnippet(e.cfg, false); !strings.Contains(out, "proxy_pass http://unix:/run/nimdeploy/nimdeploy.sock;") || strings.Contains(out, "/status") {
		t.Errorf("unix snippet:\n%s", out)
	}
}

func TestHistory(t *testing.T) {
	e := newEnv(t, `[ "$DEPLOY_TRIGGER" = manual ] || exit 4`, `
[server]
api_token_env = "TEST_API_TOKEN"
`)
	e.request("POST", "/deploy/agency", testToken, `{"user":"ana"}`)
	e.waitIdle(t)
	time.Sleep(1100 * time.Millisecond) // log names have 1s resolution
	e.push(t, pushOpts{commit: sha2, delivery: "h2"})
	e.waitIdle(t)
	// A log without footer, as left by a crash.
	os.WriteFile(filepath.Join(e.logDir, "agency", "20200101-000000-dead00.log"),
		[]byte("2020-01-01T00:00:00Z deploy=agency status=started\n2020-01-01T00:00:00Z trigger=webhook\n\nhalf done\n"), 0o640)

	rec := e.request("GET", "/history/agency", testToken, "")
	var h []State
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil || len(h) != 3 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	if h[0].Trigger != TriggerWebhook || h[0].Status != StatusFailed || *h[0].ExitCode != 4 || h[0].Commit != sha2 || h[0].Error != "exit status 4" {
		t.Errorf("newest: %+v", h[0])
	}
	if h[1].Trigger != TriggerManual || h[1].Status != StatusSuccess || h[1].Pusher != "ana" || h[1].StartedAt == nil || h[1].FinishedAt == nil {
		t.Errorf("manual: %+v", h[1])
	}
	if h[2].Status != StatusInterrupted || h[2].Log != "20200101-000000-dead00.log" {
		t.Errorf("crashed: %+v", h[2])
	}
	if rec := e.request("GET", "/history/agency?limit=1", testToken, ""); strings.Count(rec.Body.String(), `"log"`) != 1 {
		t.Errorf("limit: %s", rec.Body)
	}
	if rec := e.request("GET", "/history/agency", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("history without token: %d", rec.Code)
	}
}

// fakeGitHub serves workflow runs per commit and records commit statuses
// and notifications.
type fakeGitHub struct {
	mu       sync.Mutex
	runs     map[string]map[string]string // sha -> workflow -> "in_progress" | conclusion
	statuses []string                     // "sha state description"
	notes    []string
}

func (g *fakeGitHub) set(sha, workflow, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.runs[sha] == nil {
		g.runs[sha] = map[string]string{}
	}
	g.runs[sha][workflow] = state
}

func (g *fakeGitHub) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/actions/runs"):
			var runs []map[string]any
			for name, state := range g.runs[r.URL.Query().Get("head_sha")] {
				run := map[string]any{"name": name, "status": "completed", "conclusion": state, "run_attempt": 1, "created_at": "2026-09-29T10:00:00Z"}
				if state == "in_progress" || state == "queued" {
					run["status"], run["conclusion"] = state, nil
				}
				runs = append(runs, run)
			}
			json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
		case strings.Contains(r.URL.Path, "/statuses/"):
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			g.statuses = append(g.statuses, shortSHA(sha)+" "+body["state"]+" "+body["description"])
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/notify":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			g.notes = append(g.notes, body["text"])
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newCIEnv(t *testing.T) (*env, *fakeGitHub) {
	t.Helper()
	old := ciPollInterval
	ciPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { ciPollInterval = old })
	g := &fakeGitHub{runs: map[string]map[string]string{}}
	srv := g.serve(t)
	t.Setenv("TEST_NOTIFY_URL", srv.URL+"/notify")
	e := newEnv(t, `echo "deploying $DEPLOY_COMMIT" >> deployed.txt`, `
[notify]
format = "slack"
url_env = "TEST_NOTIFY_URL"

[github]
token_env = "TEST_GITHUB_TOKEN"
api_url = "`+srv.URL+`"
`)
	d := e.cfg.Deploy["agency"]
	d.WaitForCI = []string{"linter", "tests"}
	d.CITimeout.Duration = 5 * time.Second
	return e, g
}

func (e *env) deployed(t *testing.T) string {
	b, _ := os.ReadFile(filepath.Join(e.dir, "deployed.txt"))
	return string(b)
}

func (e *env) waitLog(t *testing.T, want string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if b, _ := os.ReadFile(filepath.Join(e.logDir, "agency", "latest.log")); strings.Contains(string(b), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log never contained %q", want)
}

func (e *env) waitStatus(t *testing.T, want string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if e.runner.State("agency").Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status never became %s: %+v", want, e.runner.State("agency"))
}

func TestWaitForCIPasses(t *testing.T) {
	e, g := newCIEnv(t)
	g.set(sha1, "linter", "success")
	g.set(sha1, "tests", "in_progress")
	e.push(t, pushOpts{commit: sha1, delivery: "c1"})
	e.waitStatus(t, StatusWaiting)
	e.waitLog(t, "ci: linter=success tests=in_progress")
	if e.deployed(t) != "" {
		t.Fatal("deployed before CI passed")
	}
	g.set(sha1, "tests", "success")
	st := e.waitIdle(t)
	if st.Status != StatusSuccess || !strings.Contains(e.deployed(t), sha1) {
		t.Fatalf("state %+v deployed %q", st, e.deployed(t))
	}
	out, _ := os.ReadFile(filepath.Join(e.logDir, "agency", st.Log))
	for _, want := range []string{"ci: waiting for linter, tests", "ci: linter=success tests=in_progress", "ci: passed after"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
}

func TestWaitForCIFailedSkips(t *testing.T) {
	e, g := newCIEnv(t)
	g.set(sha2, "linter", "success")
	g.set(sha2, "tests", "failure")
	e.push(t, pushOpts{commit: sha2, delivery: "c2"})
	st := e.waitIdle(t)
	if st.Status != StatusSkipped || st.ExitCode != nil || !strings.Contains(st.Error, "CI failed: linter=success tests=failure") {
		t.Fatalf("state %+v", st)
	}
	if e.deployed(t) != "" {
		t.Fatal("deployed although CI failed")
	}
	e.runner.Shutdown(5 * time.Second)
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.statuses) != 1 || !strings.HasPrefix(g.statuses[0], "2222222 failure Not deployed: CI failed") {
		t.Errorf("statuses %q", g.statuses)
	}
	if len(g.notes) != 1 || !strings.Contains(g.notes[0], "NOT deployed") {
		t.Errorf("notes %q", g.notes)
	}
	// history shows it as skipped too
	h, _ := e.runner.History("agency", 0)
	if len(h) != 1 || h[0].Status != StatusSkipped || !strings.Contains(h[0].Error, "tests=failure") {
		t.Errorf("history %+v", h)
	}
}

func TestWaitForCISupersededByNewerPush(t *testing.T) {
	e, g := newCIEnv(t)
	g.set(sha1, "linter", "in_progress")
	g.set(sha1, "tests", "in_progress")
	g.set(sha2, "linter", "success")
	g.set(sha2, "tests", "success")
	e.push(t, pushOpts{commit: sha1, delivery: "s1"})
	e.waitStatus(t, StatusWaiting)
	if rec := e.push(t, pushOpts{commit: sha2, delivery: "s2"}); !strings.Contains(rec.Body.String(), `"result": "queued"`) {
		t.Fatalf("second push: %s", rec.Body)
	}
	st := e.waitIdle(t)
	if st.Status != StatusSuccess || st.Commit != sha2 {
		t.Fatalf("final %+v", st)
	}
	if d := e.deployed(t); strings.Contains(d, sha1) || !strings.Contains(d, sha2) {
		t.Fatalf("deployed %q", d)
	}
	h, _ := e.runner.History("agency", 0)
	if len(h) != 2 || h[1].Status != StatusSkipped || !strings.Contains(h[1].Error, "superseded") {
		t.Fatalf("history %+v", h)
	}
	e.runner.Shutdown(5 * time.Second)
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.statuses {
		if strings.HasPrefix(s, "1111111") {
			t.Errorf("superseded commit got a status: %q", s)
		}
	}
	if len(g.notes) != 0 {
		t.Errorf("notes %q", g.notes)
	}
}

func TestWaitForCITimeoutAndManual(t *testing.T) {
	e, g := newCIEnv(t)
	e.cfg.Deploy["agency"].CITimeout.Duration = 100 * time.Millisecond
	e.push(t, pushOpts{commit: sha3, delivery: "t1"}) // no runs at all
	st := e.waitIdle(t)
	if st.Status != StatusSkipped || !strings.Contains(st.Error, "CI not finished after 100ms: linter=not_started tests=not_started") {
		t.Fatalf("timeout state %+v", st)
	}
	_ = g
	// Manual runs don't wait for CI.
	if _, err := e.runner.Submit("agency", Trigger{Source: TriggerManual, Repository: "acme/agency", Branch: "main", Commit: sha3}); err != nil {
		t.Fatal(err)
	}
	if st := e.waitIdle(t); st.Status != StatusSuccess || !strings.Contains(e.deployed(t), sha3) {
		t.Fatalf("manual %+v", st)
	}
}

func TestWaitForCIRequiresToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(path, []byte("[deploy.a]\npath = \"/a\"\nrepository = \"a/b\"\nsecret_env = \"X\"\ncommand = \"true\"\nwait_for_ci = [\"tests\"]\n"), 0o600)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "token_env") {
		t.Fatalf("err %v", err)
	}
}

// providerCase builds a webhook request the way each git host sends it.
type providerCase struct {
	provider, repo string
	request        func(branch, commit string, deleted, validAuth bool) *http.Request
	ping           func() *http.Request
}

func hexHMAC(body []byte) string {
	return strings.TrimPrefix(sign(body), "sha256=")
}

func jsonRequest(path string, body []byte, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func authOr(valid bool, good, bad string) string {
	if valid {
		return good
	}
	return bad
}

var zero40 = strings.Repeat("0", 40)

var providerCases = []providerCase{
	{
		provider: "gitea", repo: "acme/agency",
		request: func(branch, commit string, deleted, valid bool) *http.Request {
			if deleted {
				commit = zero40
			}
			body := []byte(`{"ref":"refs/heads/` + branch + `","before":"` + sha1 + `","after":"` + commit + `","repository":{"full_name":"acme/agency"},"pusher":{"id":1,"login":"gitea-dev","username":"gitea-dev"}}`)
			// Only the Gitea headers, no GitHub-compatible ones.
			return jsonRequest("/hooks/agency", body, map[string]string{
				"X-Gitea-Event": "push", "X-Gitea-Delivery": "gt-" + commit[:6],
				"X-Gitea-Signature": authOr(valid, hexHMAC(body), strings.Repeat("0", 64)),
			})
		},
	},
	{
		provider: "forgejo", repo: "acme/agency",
		request: func(branch, commit string, deleted, valid bool) *http.Request {
			if deleted {
				commit = zero40
			}
			body := []byte(`{"ref":"refs/heads/` + branch + `","after":"` + commit + `","repository":{"full_name":"acme/agency"},"pusher":{"login":"fj-dev"}}`)
			return jsonRequest("/hooks/agency", body, map[string]string{
				"X-Forgejo-Event": "push", "X-Forgejo-Delivery": "fj-" + commit[:6],
				"X-Forgejo-Signature": authOr(valid, hexHMAC(body), "nope"),
			})
		},
	},
	{
		provider: "gitlab", repo: "acme/web/agency",
		request: func(branch, commit string, deleted, valid bool) *http.Request {
			checkout := `"` + commit + `"`
			if deleted {
				commit, checkout = zero40, "null"
			}
			body := []byte(`{"object_kind":"push","event_name":"push","ref":"refs/heads/` + branch + `","after":"` + commit + `","checkout_sha":` + checkout + `,"user_username":"gl-dev","user_name":"GL Dev","project":{"path_with_namespace":"acme/web/agency"}}`)
			return jsonRequest("/hooks/agency", body, map[string]string{
				"X-Gitlab-Event": "Push Hook", "Idempotency-Key": "gl-" + commit[:6],
				"X-Gitlab-Token": authOr(valid, testSecret, "wrong"),
			})
		},
	},
	{
		provider: "bitbucket", repo: "acme/agency",
		request: func(branch, commit string, deleted, valid bool) *http.Request {
			change := `{"new":{"type":"branch","name":"` + branch + `","target":{"hash":"` + commit + `"}},"old":{"type":"branch","name":"` + branch + `"}}`
			if deleted {
				change = `{"new":null,"old":{"type":"branch","name":"` + branch + `","target":{"hash":"` + sha1 + `"}},"closed":true}`
			}
			// A tag and another branch in the same push must not confuse it.
			body := []byte(`{"actor":{"nickname":"bb-dev","display_name":"BB Dev"},"repository":{"full_name":"acme/agency"},"push":{"changes":[` +
				`{"new":{"type":"tag","name":"v1","target":{"hash":"` + sha3 + `"}}},` +
				`{"new":{"type":"branch","name":"other","target":{"hash":"` + sha3 + `"}}},` + change + `]}}`)
			return jsonRequest("/hooks/agency", body, map[string]string{
				"X-Event-Key": "repo:push", "X-Request-UUID": "bb-" + commit[:6],
				"X-Hub-Signature": authOr(valid, sign(body), "sha256=00"),
			})
		},
	},
	{
		provider: "bitbucket", repo: "PROJ/agency", // Data Center / Server
		request: func(branch, commit string, deleted, valid bool) *http.Request {
			typ := "UPDATE"
			if deleted {
				typ, commit = "DELETE", zero40
			}
			body := []byte(`{"eventKey":"repo:refs_changed","actor":{"name":"bbs-dev","displayName":"BBS Dev"},"repository":{"slug":"agency","project":{"key":"PROJ"}},"changes":[{"ref":{"id":"refs/heads/` + branch + `","displayId":"` + branch + `","type":"BRANCH"},"refId":"refs/heads/` + branch + `","fromHash":"` + sha1 + `","toHash":"` + commit + `","type":"` + typ + `"}]}`)
			return jsonRequest("/hooks/agency", body, map[string]string{
				"X-Event-Key": "repo:refs_changed", "X-Request-Id": "bbs-" + commit[:6],
				"X-Hub-Signature": authOr(valid, sign(body), "sha256=00"),
			})
		},
		ping: func() *http.Request {
			body := []byte(`{"test":true}`)
			return jsonRequest("/hooks/agency", body, map[string]string{"X-Event-Key": "diagnostics:ping", "X-Hub-Signature": sign(body)})
		},
	},
}

func TestProviders(t *testing.T) {
	for _, c := range providerCases {
		t.Run(c.provider+"/"+c.repo, func(t *testing.T) {
			e := newEnv(t, `echo "provider=$DEPLOY_PROVIDER commit=$DEPLOY_COMMIT pusher=$DEPLOY_PUSHER"`, "")
			d := e.cfg.Deploy["agency"]
			d.Provider, d.Repository = c.provider, c.repo
			e.h = NewServer(e.cfg, e.runner).Routes()
			serve := func(r *http.Request) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				e.h.ServeHTTP(rec, r)
				return rec
			}

			if rec := serve(c.request("main", sha2, false, false)); rec.Code != http.StatusUnauthorized {
				t.Fatalf("bad auth: %d %s", rec.Code, rec.Body)
			}
			if rec := serve(c.request("dev", sha2, false, true)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ignored") {
				t.Fatalf("other branch: %d %s", rec.Code, rec.Body)
			}
			if rec := serve(c.request("main", sha2, true, true)); !strings.Contains(rec.Body.String(), "branch deleted") {
				t.Fatalf("deleted branch: %d %s", rec.Code, rec.Body)
			}
			if c.ping != nil {
				if rec := serve(c.ping()); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
					t.Fatalf("ping: %d %s", rec.Code, rec.Body)
				}
			}
			rec := serve(c.request("main", sha2, false, true))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("push: %d %s", rec.Code, rec.Body)
			}
			st := e.waitIdle(t)
			if st.Status != StatusSuccess || st.Commit != sha2 || st.Provider != c.provider || st.Delivery == "" || st.Pusher == "" {
				t.Fatalf("state %+v", st)
			}
			out, _ := os.ReadFile(filepath.Join(e.logDir, "agency", st.Log))
			if !strings.Contains(string(out), "provider="+c.provider+" commit="+sha2+" pusher="+st.Pusher) {
				t.Errorf("log:\n%s", out)
			}
			// Same delivery again (a retry) is ignored.
			if rec := serve(c.request("main", sha2, false, true)); !strings.Contains(rec.Body.String(), "duplicate") {
				t.Errorf("retry: %d %s", rec.Code, rec.Body)
			}
			if h, _ := e.runner.History("agency", 0); len(h) != 1 || h[0].Provider != c.provider {
				t.Errorf("history %+v", h)
			}
		})
	}
}

func TestGitLabIgnoresTagPush(t *testing.T) {
	e := newEnv(t, `true`, "")
	d := e.cfg.Deploy["agency"]
	d.Provider, d.Repository = "gitlab", "acme/agency"
	e.h = NewServer(e.cfg, e.runner).Routes()
	body := []byte(`{"object_kind":"tag_push","ref":"refs/tags/v1","after":"` + sha1 + `","project":{"path_with_namespace":"acme/agency"}}`)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, jsonRequest("/hooks/agency", body, map[string]string{"X-Gitlab-Event": "Tag Push Hook", "X-Gitlab-Token": testSecret}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not handled") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestProviderConfig(t *testing.T) {
	for toml, wantErr := range map[string]string{
		"provider = \"svn\"": "provider must be one of",
		"provider = \"gitlab\"\nwait_for_ci = [\"tests\"]": "only supported with provider",
	} {
		path := filepath.Join(t.TempDir(), "c.toml")
		os.WriteFile(path, []byte("[github]\ntoken_env = \"T\"\n[deploy.a]\npath = \"/a\"\nrepository = \"a/b\"\nsecret_env = \"X\"\ncommand = \"true\"\n"+toml+"\n"), 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%q: err %v", toml, err)
		}
	}
}

func TestPlaceholderSecretRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(path, []byte("[deploy.a]\npath = \"/a\"\nrepository = \"a/b\"\nsecret_env = \"PH_SECRET\"\ncommand = \"true\"\n"), 0o600)
	t.Setenv("PH_SECRET", "change-me")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveSecrets(); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("err %v", err)
	}
}

func TestUserInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	app := t.TempDir()
	stage := t.TempDir()
	p, _ := newUserPaths(stage)
	read := func(path string) string { b, _ := os.ReadFile(p.at(path)); return string(b) }

	// Example mode: template config pointing at the user's log dir.
	if err := writeUserInstall(p, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(p.config), `directory = "`+home+`/.local/state/nimdeploy"`) {
		t.Errorf("logs dir not rewritten:\n%s", read(p.config))
	}
	if info, _ := os.Stat(p.at(p.secrets)); info.Mode().Perm() != 0o600 || !hasPlaceholders(p.at(p.secrets)) {
		t.Errorf("secrets %v placeholders=%v", info.Mode().Perm(), hasPlaceholders(p.at(p.secrets)))
	}
	if info, _ := os.Stat(p.at(p.bin)); info.Mode().Perm() != 0o755 {
		t.Errorf("binary mode %v", info.Mode())
	}
	if !strings.Contains(read(p.unit), "ExecStart=%h/.local/bin/nimdeploy -config %E/nimdeploy/config.toml") {
		t.Errorf("unit:\n%s", read(p.unit))
	}

	// Quick mode on a fresh home: config + generated secret, validated.
	os.RemoveAll(filepath.Join(stage, home))
	q := quickDeploy{repo: "Acme/Shop", provider: "gitlab", branch: "prod", dir: app, command: "./deploy.sh", listen: "127.0.0.1:9100"}
	if err := q.complete(); err != nil {
		t.Fatal(err)
	}
	if err := writeUserInstall(p, &q); err != nil {
		t.Fatal(err)
	}
	if q.name != "shop" || q.secretEnv != "SHOP_WEBHOOK_SECRET" || len(q.secret) != 64 || hasPlaceholders(p.at(p.secrets)) {
		t.Fatalf("quick %+v", q)
	}
	d := q.cfg.Deploy["shop"]
	if d.Provider != "gitlab" || d.Branch != "prod" || d.WorkingDirectory != app || d.Path != "/hooks/shop" ||
		strings.Join(d.Args, " ") != "-eo pipefail -c ./deploy.sh" || q.cfg.Server.Listen != "127.0.0.1:9100" {
		t.Fatalf("deploy %+v", d)
	}
	if !strings.Contains(nginxSnippet(q.cfg, false), "location = /hooks/shop {") {
		t.Error("nginx snippet")
	}

	// Running it again keeps the secret; another repo is added next to it.
	first := q.secret
	q2 := q
	if err := writeUserInstall(p, &q2); err != nil || q2.secret != first {
		t.Fatalf("rerun: %v secret changed=%v", err, q2.secret != first)
	}
	q3 := quickDeploy{repo: "acme/blog", provider: "github", branch: "main", dir: app, command: "make deploy", listen: "127.0.0.1:9100"}
	q3.complete()
	if err := writeUserInstall(p, &q3); err != nil {
		t.Fatal(err)
	}
	if len(q3.cfg.Deploy) != 2 || q3.secret == first || strings.Count(read(p.config), "[deploy.shop]") != 1 {
		t.Fatalf("second deploy: %d deploys\n%s", len(q3.cfg.Deploy), read(p.config))
	}

	// Bad input is refused before touching anything.
	bad := quickDeploy{repo: "acme/x", provider: "svn", branch: "main", dir: app, command: "true", listen: "127.0.0.1:9100"}
	if err := bad.complete(); err == nil {
		t.Error("svn provider accepted")
	}
	if err := (&quickDeploy{repo: "acme/x", provider: "github", dir: "/nonexistent", command: "true", listen: ":1"}).complete(); err == nil {
		t.Error("missing dir accepted")
	}

	if code := cliUninstall([]string{"-destdir", stage, "--purge"}); code != 0 || exists(p.at(p.bin)) || exists(p.at(p.conf)) || exists(p.at(p.unit)) {
		t.Fatalf("uninstall code=%d", code)
	}
}

func TestUserInstallRollsBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	stage := t.TempDir()
	p, _ := newUserPaths(stage)
	if err := writeUserInstall(p, nil); err != nil { // example config, change-me secrets
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p.at(p.config))
	beforeSecrets, _ := os.ReadFile(p.at(p.secrets))
	q := quickDeploy{repo: "acme/shop", provider: "github", branch: "main", dir: t.TempDir(), command: "true", listen: "127.0.0.1:9000"}
	q.complete()
	err := writeUserInstall(p, &q)
	if err == nil || !strings.Contains(err.Error(), "uninstall --purge") {
		t.Fatalf("err %v", err)
	}
	after, _ := os.ReadFile(p.at(p.config))
	afterSecrets, _ := os.ReadFile(p.at(p.secrets))
	if string(after) != string(before) || string(afterSecrets) != string(beforeSecrets) {
		t.Fatal("config or secrets changed after a failed install")
	}
}
