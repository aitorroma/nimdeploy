package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newGenericEnv adds a generic deploy "svc" (next to the default "agency")
// that runs script; section is appended to its [deploy.svc] table.
func newGenericEnv(t *testing.T, script, section, extra string) *env {
	t.Helper()
	return newEnv(t, `echo agency`, extra+`
[deploy.svc]
provider = "generic"
path = "/hooks/svc"
secret_env = "TEST_WEBHOOK_SECRET"
command = "/bin/sh"
args = ["-c", `+jsonQuote(script)+`]
timeout = "5s"
`+section+"\n")
}

type sendOpts struct {
	body, delivery, timestamp string
	headers                   map[string]string
	unsigned                  bool
}

func (e *env) send(t *testing.T, o sendOpts) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hooks/svc", strings.NewReader(o.body))
	req.Header.Set("Content-Type", "application/json")
	if !o.unsigned {
		req.Header.Set("X-Signature", signGeneric([]byte(testSecret), []byte(o.body), o.timestamp))
	}
	if o.timestamp != "" {
		req.Header.Set("X-Timestamp", o.timestamp)
	}
	if o.delivery != "" {
		req.Header.Set("X-Delivery-ID", o.delivery)
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) waitIdleName(t *testing.T, name string) State {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		e.runner.mu.Lock()
		busy := false
		for lane, n := range e.runner.running {
			if n > 0 && (lane == name || strings.HasPrefix(lane, name+laneSep)) {
				busy = true
			}
		}
		busy = busy || e.runner.hasPendingLocked(name)
		e.runner.mu.Unlock()
		if !busy {
			return e.runner.State(name)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("deploy %s did not finish", name)
	return State{}
}

func readLatest(t *testing.T, e *env, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.logDir, name, "latest.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGenericWebhookParams(t *testing.T) {
	e := newGenericEnv(t, `echo "service=$SERVICE version=$VERSION target=$TARGET secret=$TEST_WEBHOOK_SECRET"`, `
pusher_from = "user"
[deploy.svc.when]
action = ["deploy", "redeploy"]
"meta.dry" = false
[deploy.svc.params]
SERVICE = { from = "service", enum = ["api", "worker"], required = true }
VERSION = { from = "release.tag", match = '^(v\d+\.\d+\.\d+|latest)$', default = "latest" }
TARGET  = { from = "hosts[1].name" }
`, "")

	body := `{"action":"deploy","meta":{"dry":false},"service":"api","release":{"tag":"v1.4.2"},"hosts":[{"name":"a"},{"name":"web-2.internal"}],"user":"ansible"}`
	rec := e.send(t, sendOpts{body: body, delivery: "d-1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	st := e.waitIdleName(t, "svc")
	if st.Status != StatusSuccess || st.Pusher != "ansible" || st.Provider != providerGeneric {
		t.Fatalf("state %+v", st)
	}
	want := []Param{{"SERVICE", "api"}, {"TARGET", "web-2.internal"}, {"VERSION", "v1.4.2"}}
	if !reflect.DeepEqual(st.Params, want) {
		t.Fatalf("params %+v", st.Params)
	}
	out := readLatest(t, e, "svc")
	for _, w := range []string{"param.SERVICE=api", "param.VERSION=v1.4.2", "service=api version=v1.4.2 target=web-2.internal secret=\n", "provider=generic"} {
		if !strings.Contains(out, w) {
			t.Errorf("log missing %q:\n%s", w, out)
		}
	}

	// History is rebuilt from the log, params included.
	h, err := e.runner.History("svc", 0)
	if err != nil || len(h) != 1 || !reflect.DeepEqual(h[0].Params, want) {
		t.Fatalf("history %+v %v", h, err)
	}

	// Defaults, numbers and booleans.
	rec = e.send(t, sendOpts{body: `{"action":"redeploy","meta":{"dry":false},"service":"worker"}`, delivery: "d-2"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if st := e.waitIdleName(t, "svc"); paramValue(st.Params, "VERSION") != "latest" || paramValue(st.Params, "TARGET") != "" {
		t.Fatalf("defaults %+v", st.Params)
	}
	if e.send(t, sendOpts{body: `{"action":"deploy","meta":{"dry":false},"service":"worker"}`, delivery: "d-2"}).Code != http.StatusOK {
		t.Error("duplicate delivery not ignored")
	}
}

func TestGenericWebhookRejects(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	e := newGenericEnv(t, `touch `+marker, `
[deploy.svc.when]
action = "deploy"
[deploy.svc.params]
SERVICE = { from = "service", enum = ["api", "worker"], required = true }
VERSION = { from = "version" }
`, "")
	cases := []struct {
		name string
		o    sendOpts
		code int
		msg  string
	}{
		{"unsigned", sendOpts{body: `{"action":"deploy","service":"api"}`, unsigned: true}, 401, "invalid signature"},
		{"bad signature", sendOpts{body: `{"action":"deploy","service":"api"}`, headers: map[string]string{"X-Signature": "sha256=" + strings.Repeat("0", 64)}}, 401, "invalid signature"},
		{"when not met", sendOpts{body: `{"action":"rollback","service":"api"}`}, 200, `action is \"rollback\", not deploy`},
		{"when missing", sendOpts{body: `{"service":"api"}`}, 200, "action is missing"},
		{"required missing", sendOpts{body: `{"action":"deploy"}`}, 400, "param SERVICE: service is missing"},
		{"not in enum", sendOpts{body: `{"action":"deploy","service":"db"}`}, 400, "must be one of api, worker"},
		{"shell characters", sendOpts{body: `{"action":"deploy","service":"api","version":"1; rm -rf /"}`}, 400, "param VERSION: has characters outside"},
		{"object value", sendOpts{body: `{"action":"deploy","service":"api","version":{"a":1}}`}, 400, "is not a string, number or boolean"},
		{"newline", sendOpts{body: `{"action":"deploy","service":"api","version":"a\nb"}`}, 400, "control characters"},
		{"not JSON", sendOpts{body: `action=deploy`}, 400, "invalid JSON"},
		{"too long", sendOpts{body: `{"action":"deploy","service":"api","version":"` + strings.Repeat("a", 300) + `"}`}, 400, "longer than 256"},
	}
	for _, c := range cases {
		rec := e.send(t, c.o)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.msg) {
			t.Errorf("%s: code %d body %s", c.name, rec.Code, rec.Body)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a rejected request ran the command")
	}
	if st := e.runner.State("svc"); st.Status != StatusNever {
		t.Fatalf("state %+v", st)
	}
}

func TestGenericTimestamp(t *testing.T) {
	e := newGenericEnv(t, `true`, `timestamp_header = "X-Timestamp"
max_skew = "1m"`, "")
	now := strconv.FormatInt(time.Now().Unix(), 10)
	old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)

	if rec := e.send(t, sendOpts{body: `{}`, timestamp: now}); rec.Code != http.StatusAccepted {
		t.Fatalf("fresh: %d %s", rec.Code, rec.Body)
	}
	e.waitIdleName(t, "svc")
	if rec := e.send(t, sendOpts{body: `{}`, timestamp: old}); rec.Code != http.StatusUnauthorized {
		t.Errorf("old timestamp accepted: %d", rec.Code)
	}
	// Signed without the timestamp: the timestamp could be swapped.
	rec := e.send(t, sendOpts{body: `{}`, headers: map[string]string{"X-Timestamp": now}})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("signature without timestamp accepted: %d", rec.Code)
	}
	if rec := e.send(t, sendOpts{body: `{}`}); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing timestamp accepted: %d", rec.Code)
	}
	// RFC 3339 works too.
	if rec := e.send(t, sendOpts{body: `{}`, timestamp: time.Now().UTC().Format(time.RFC3339)}); rec.Code != http.StatusAccepted {
		t.Errorf("RFC 3339: %d %s", rec.Code, rec.Body)
	}
}

func TestGenericToken(t *testing.T) {
	for _, header := range []string{"", "X-Deploy-Token"} {
		section := `auth = "token"`
		if header != "" {
			section += "\ntoken_header = \"" + header + "\""
		}
		e := newGenericEnv(t, `true`, section, "")
		good, bad := map[string]string{"Authorization": "Bearer " + testSecret}, map[string]string{"Authorization": "Bearer nope"}
		if header != "" {
			good, bad = map[string]string{header: testSecret}, map[string]string{header: "nope"}
		}
		if rec := e.send(t, sendOpts{body: `{}`, unsigned: true, headers: good}); rec.Code != http.StatusAccepted {
			t.Errorf("header %q good token: %d %s", header, rec.Code, rec.Body)
		}
		if rec := e.send(t, sendOpts{body: `{}`, unsigned: true, headers: bad}); rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q bad token: %d", header, rec.Code)
		}
		if rec := e.send(t, sendOpts{body: `{}`, unsigned: true}); rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q no token: %d", header, rec.Code)
		}
		e.waitIdleName(t, "svc")
	}
}

func TestQueueKeyLanes(t *testing.T) {
	runsFile := filepath.Join(t.TempDir(), "runs")
	e := newGenericEnv(t, `echo "$SERVICE $V" >> `+runsFile+`; sleep 0.4`, `
queue_key = "SERVICE"
[deploy.svc.params]
SERVICE = { from = "service", enum = ["api", "web"], required = true }
V = { from = "v" }
`, "")
	res := func(o sendOpts) string {
		rec := e.send(t, o)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("code %d: %s", rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), `"result": "queued"`) {
			return "queued"
		}
		return "started"
	}
	got := []string{
		res(sendOpts{body: `{"service":"api","v":"1"}`, delivery: "a1"}),
		res(sendOpts{body: `{"service":"web","v":"1"}`, delivery: "w1"}), // other lane: runs in parallel
		res(sendOpts{body: `{"service":"api","v":"2"}`, delivery: "a2"}), // queued
		res(sendOpts{body: `{"service":"api","v":"3"}`, delivery: "a3"}), // replaces a2
	}
	if want := []string{"started", "started", "queued", "queued"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("results %v, want %v", got, want)
	}
	e.waitIdleName(t, "svc")
	b, _ := os.ReadFile(runsFile)
	runs := strings.Fields(strings.ReplaceAll(string(b), "\n", " | "))
	joined := strings.Join(runs, " ")
	for _, w := range []string{"api 1", "web 1", "api 3"} {
		if !strings.Contains(joined, w) {
			t.Errorf("missing run %q in %q", w, joined)
		}
	}
	if strings.Contains(joined, "api 2") {
		t.Errorf("superseded run executed: %q", joined)
	}
}

func TestManualRunParams(t *testing.T) {
	e := newGenericEnv(t, `echo "service=$SERVICE"`, `
[deploy.svc.params]
SERVICE = { from = "service", enum = ["api", "web"], required = true }
`, `[server]
api_token_env = "TEST_API_TOKEN"
`)
	if rec := e.request(http.MethodPost, "/deploy/svc", testToken, `{}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "SERVICE is required") {
		t.Errorf("missing param: %d %s", rec.Code, rec.Body)
	}
	if rec := e.request(http.MethodPost, "/deploy/svc", testToken, `{"params":{"SERVICE":"db"}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid param: %d %s", rec.Code, rec.Body)
	}
	if rec := e.request(http.MethodPost, "/deploy/svc", testToken, `{"params":{"SERVICE":"api","EXTRA":"x"}}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown param EXTRA") {
		t.Errorf("unknown param: %d %s", rec.Code, rec.Body)
	}
	if rec := e.request(http.MethodPost, "/deploy/svc", testToken, `{"user":"ops","params":{"SERVICE":"web"}}`); rec.Code != http.StatusAccepted {
		t.Fatalf("manual: %d %s", rec.Code, rec.Body)
	}
	st := e.waitIdleName(t, "svc")
	if st.Trigger != TriggerManual || st.Branch != "" || paramValue(st.Params, "SERVICE") != "web" || !strings.Contains(readLatest(t, e, "svc"), "service=web") {
		t.Fatalf("state %+v", st)
	}
}

// Params and when work with git providers too, e.g. to skip a deploy by commit message.
func TestGitProviderWhenAndParams(t *testing.T) {
	e := newEnv(t, `echo "by=$PUSHED_BY"`, "")
	d := e.cfg.Deploy["agency"]
	d.When = map[string]any{"pusher.name": "dev"}
	d.Params = map[string]*ParamConfig{"PUSHED_BY": {From: "pusher.name", Required: true}}
	if err := d.validate(); err != nil {
		t.Fatal(err)
	}
	if rec := e.push(t, pushOpts{delivery: "g1"}); rec.Code != http.StatusAccepted {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	e.waitIdle(t)
	if out := readLatest(t, e, "agency"); !strings.Contains(out, "by=dev") || !strings.Contains(out, "param.PUSHED_BY=dev") {
		t.Fatalf("log:\n%s", out)
	}
	d.When = map[string]any{"pusher.name": "someone-else"}
	if err := d.validate(); err != nil {
		t.Fatal(err)
	}
	if rec := e.push(t, pushOpts{delivery: "g2"}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("when not applied: %d %s", rec.Code, rec.Body)
	}
}

func TestGenericConfigValidation(t *testing.T) {
	base := `[logging]
directory = "/tmp/x"
[deploy.svc]
path = "/hooks/svc"
secret_env = "S"
command = "/bin/true"
`
	cases := map[string]string{
		`provider = "generic"` + "\nbranch = \"main\"": "branch does not apply",
		`auth = "token"`: "auth is only for provider",
		`provider = "generic"` + "\nauth = \"magic\"":                                                            "auth must be hmac or token",
		`provider = "generic"` + "\nauth = \"token\"\nsignature_header = \"X\"":                                  "are for auth = \"hmac\"",
		`provider = "generic"` + "\nqueue_key = \"NOPE\"":                                                        "queue_key NOPE must be one of the params",
		`provider = "generic"` + "\n[deploy.svc.params]\nPATH = { from = \"p\" }":                                "name is reserved",
		`provider = "generic"` + "\n[deploy.svc.params]\nLD_PRELOAD = { from = \"p\" }":                          "name is reserved",
		`provider = "generic"` + "\n[deploy.svc.params]\nDEPLOY_NAME = { from = \"p\" }":                         "name is reserved",
		`provider = "generic"` + "\n[deploy.svc.params]\nlower = { from = \"p\" }":                               "must be UPPER_CASE",
		`provider = "generic"` + "\n[deploy.svc.params]\nA = { from = \"p[x]\" }":                                "is not an array index",
		`provider = "generic"` + "\n[deploy.svc.params]\nA = { from = \"p\", enum = [\"a\"], match = \"b\" }":    "not both",
		`provider = "generic"` + "\n[deploy.svc.params]\nA = { from = \"p\", required = true, default = \"x\" }": "cannot have a default",
		`provider = "generic"` + "\n[deploy.svc.params]\nA = { from = \"p\", enum = [\"a\"], default = \"b\" }":  "default: must be one of a",
		`provider = "generic"` + "\nenv = [\"A=1\"]\n[deploy.svc.params]\nA = { from = \"p\" }":                  "both an env entry and a param",
		`provider = "generic"` + "\n[deploy.svc.when]\nx = []":                                                   "empty list",
		`provider = "generic"` + "\nlock = false\nqueue_key = \"A\"\n[deploy.svc.params]\nA = { from = \"p\" }":  "queue_key needs lock = true",
		"repository = \"a/b\"\nprovider = \"github\"\npusher_from = \"x\"":                                       "pusher_from is only for provider",
	}
	for extra, want := range cases {
		path := filepath.Join(t.TempDir(), "c.toml")
		// Keys must come before any sub-table of the deploy.
		toml := base + extra + "\n"
		if !strings.Contains(extra, "repository") && !strings.Contains(extra, "generic") {
			toml = base + "repository = \"a/b\"\n" + extra + "\n"
		}
		_ = os.WriteFile(path, []byte(toml), 0o600)
		_, err := LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", extra, err, want)
		}
	}

	// A valid one, without repository or branch.
	path := filepath.Join(t.TempDir(), "c.toml")
	_ = os.WriteFile(path, []byte(base+`provider = "generic"
queue_key = "SERVICE"
[deploy.svc.params]
SERVICE = { from = "service", required = true }
`), 0o600)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Deploy["svc"]
	if d.Auth != authHMAC || d.SignatureHeader != "X-Signature" || d.DeliveryHeader != "X-Delivery-ID" || d.MaxSkew.Duration != 5*time.Minute {
		t.Fatalf("defaults %+v", d)
	}
}

func TestJSONPath(t *testing.T) {
	doc, err := decodeJSON([]byte(`{"a":{"b":[{"c":"x"},{"c":7}]},"k.e.y":true,"n":1.50,"nil":null}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"a.b[0].c": "x", "a.b[1].c": "7", `["k.e.y"]`: "true", "n": "1.50",
	}
	for path, want := range cases {
		steps, err := parseJSONPath(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		v, ok := lookupJSON(doc, steps)
		s, sok := scalarString(v)
		if !ok || !sok || s != want {
			t.Errorf("%s = %v (%v), want %s", path, v, ok, want)
		}
	}
	for _, missing := range []string{"a.b[5].c", "a.x", "nil", "a.b.c", "n.x"} {
		steps, _ := parseJSONPath(missing)
		if _, ok := lookupJSON(doc, steps); ok {
			t.Errorf("%s should be missing", missing)
		}
	}
	for _, bad := range []string{"", ".a", "a..b", "a[", "a[-1]", `a["x`, "a b"} {
		if _, err := parseJSONPath(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestCLISend(t *testing.T) {
	e := newGenericEnv(t, `echo "service=$SERVICE"`, `timestamp_header = "X-Timestamp"
[deploy.svc.params]
SERVICE = { from = "service", required = true }
`, "")
	srv := httptest.NewServer(e.h)
	defer srv.Close()

	t.Setenv("NIMDEPLOY_SECRET", testSecret)
	if code := cliSend([]string{"-timestamp-header", "X-Timestamp", "-data", `{"service":"api"}`, srv.URL + "/hooks/svc"}); code != 0 {
		t.Fatalf("send exit %d", code)
	}
	st := e.waitIdleName(t, "svc")
	if st.Status != StatusSuccess || paramValue(st.Params, "SERVICE") != "api" || st.Delivery == "" {
		t.Fatalf("state %+v", st)
	}
	// Body from a file; wrong secret fails with exit 1.
	f := filepath.Join(t.TempDir(), "b.json")
	_ = os.WriteFile(f, []byte(`{"service":"web"}`), 0o600)
	t.Setenv("OTHER", "wrong")
	if code := cliSend([]string{"-secret-env", "OTHER", "-timestamp-header", "X-Timestamp", "-data", "@" + f, srv.URL + "/hooks/svc"}); code != 1 {
		t.Errorf("wrong secret exit %d", code)
	}
	if code := cliSend([]string{"-data", "{nope", srv.URL + "/hooks/svc"}); code != 2 {
		t.Errorf("invalid JSON exit %d", code)
	}
}
