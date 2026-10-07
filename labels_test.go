package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLabels(t *testing.T) {
	srv, got := recorder(t)
	t.Setenv("TEST_NOTIFY_URL", srv.URL)
	e := newEnv(t, `echo "client=$DEPLOY_LABEL_CLIENT env=$DEPLOY_LABEL_ENVIRONMENT component=$DEPLOY_LABEL_COMPONENT"; exit 1`, `[server]
api_token_env = "TEST_API_TOKEN"
[labels]
client = "Acme Corp"
environment = "production"
component = "global"
[notify]
format = "slack"
on = "always"
url_env = "TEST_NOTIFY_URL"
[deploy.agency.labels]
component = "frontend"
url = "https://acme.example/agency"
[deploy.other]
path = "/hooks/other"
repository = "acme/other"
secret_env = "TEST_WEBHOOK_SECRET"
command = "/bin/true"
[deploy.other.labels]
environment = "stage"
`)
	want := map[string]string{"client": "Acme Corp", "environment": "production", "component": "frontend", "url": "https://acme.example/agency"}
	if l := e.cfg.Deploy["agency"].labels; formatLabels(l) != formatLabels(want) {
		t.Fatalf("merged %v", l)
	}

	// A deploy that never ran already shows its labels.
	if st := e.runner.State("agency"); st.Labels["client"] != "Acme Corp" {
		t.Fatalf("never-run state %+v", st)
	}

	e.push(t, pushOpts{delivery: "l1"})
	st := e.waitIdle(t)
	if st.Labels["component"] != "frontend" {
		t.Fatalf("state labels %v", st.Labels)
	}
	if out := readLatest(t, e, "agency"); !strings.Contains(out, "client=Acme Corp env=production component=frontend") {
		t.Errorf("script env:\n%s", out)
	}

	// Notification: title with client, environment, [PRODUCTION] and the url.
	deadline := time.Now().Add(5 * time.Second)
	for len(got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	msgs := got()
	if len(msgs) == 0 {
		t.Fatal("no notification")
	}
	text, _ := msgs[0].body["text"].(string)
	if !strings.HasPrefix(text, "❌ [PRODUCTION] Acme Corp · production · agency deploy FAILED") || !strings.Contains(text, "https://acme.example/agency") {
		t.Errorf("message %q", text)
	}

	// API filter and history.
	rec := e.request(http.MethodGet, "/status?label=environment=stage", testToken, "")
	var states map[string]State
	_ = json.Unmarshal(rec.Body.Bytes(), &states)
	if len(states) != 1 || states["other"].Labels["environment"] != "stage" {
		t.Errorf("filtered status %v", states)
	}
	if rec := e.request(http.MethodGet, "/status?label=bad", testToken, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad filter: %d", rec.Code)
	}
	if h, _ := e.runner.History("agency", 1); len(h) != 1 || h[0].Labels["client"] != "Acme Corp" {
		t.Errorf("history labels %+v", h)
	}

	// Metrics: one info line per deploy.
	body := e.request(http.MethodGet, "/metrics", testToken, "").Body.String()
	if !strings.Contains(body, `nimdeploy_deploy_info{deploy="agency",client="Acme Corp",component="frontend",environment="production",url="https://acme.example/agency"} 1`) {
		t.Errorf("metrics:\n%s", body)
	}
	if !strings.Contains(body, `nimdeploy_deploy_info{deploy="other",client="Acme Corp",component="global",environment="stage"} 1`) {
		t.Errorf("metrics other")
	}
}

func TestLabelsValidationAndHelpers(t *testing.T) {
	base := "[logging]\ndirectory = \"/tmp/x\"\n[deploy.s]\npath = \"/hooks/s\"\nrepository = \"a/b\"\nsecret_env = \"S\"\ncommand = \"/bin/true\"\n"
	cases := map[string]string{
		"[labels]\nClient = \"x\"\n":                           "keys are lowercase",
		"[labels]\ndeploy = \"x\"\n":                           "reserved",
		"[labels]\nk = \"a\\nb\"\n":                            "control characters",
		"[labels]\nk = \"" + strings.Repeat("a", 201) + "\"\n": "longer than 200",
	}
	for extra, want := range cases {
		path := filepath.Join(t.TempDir(), "c.toml")
		_ = os.WriteFile(path, []byte(extra+base), 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", extra, err, want)
		}
	}
	path := filepath.Join(t.TempDir(), "c.toml")
	_ = os.WriteFile(path, []byte(base+"[deploy.s.labels]\nbad-key = \"x\"\n"), 0o600)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "deploy.s.labels") {
		t.Errorf("deploy label validation: %v", err)
	}

	if got := labelTitle(map[string]string{"environment": "stage", "client": "Acme"}, "web"); got != "Acme · stage · web" {
		t.Errorf("title %q", got)
	}
	if got := labelTitle(nil, "web"); got != "web" {
		t.Errorf("title without labels %q", got)
	}
	f, _ := parseLabelFilter([]string{"client=Acme", "environment=prod"})
	if !f.match(map[string]string{"client": "Acme", "environment": "prod", "x": "y"}) || f.match(map[string]string{"client": "Acme"}) {
		t.Error("filter")
	}
	if env := strings.Join(labelEnv(map[string]string{"client": "Acme", "env": "prod"}), ","); env != "DEPLOY_LABEL_CLIENT=Acme,DEPLOY_LABEL_ENV=prod" {
		t.Errorf("env %q", env)
	}
}
