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
		if st := e.runner.State("agency"); st.Status != StatusRunning && st.Queued == nil {
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
