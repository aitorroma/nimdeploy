package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWhenOperators(t *testing.T) {
	when := map[string]any{
		"ref":     map[string]any{"match": "^refs/tags/v[0-9]+", "not_match": "-rc"},
		"user":    map[string]any{"not": []any{"bot", "renovate"}},
		"size":    map[string]any{"gt": int64(10), "lte": 100.0},
		"dry_run": map[string]any{"exists": false},
		"title":   map[string]any{"prefix": "[deploy]", "contains": "api", "suffix": "!"},
		"env":     map[string]any{"in": []any{"stage", "prod"}},
		"flag":    true,
	}
	conds, err := parseWhen(when)
	if err != nil {
		t.Fatal(err)
	}
	doc := func(js string) any {
		v, err := decodeJSON([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	ok := `{"ref":"refs/tags/v1.2","user":"ana","size":50,"title":"[deploy] api now!","env":"prod","flag":true}`
	if r := matchWhen(conds, doc(ok)); r != "" {
		t.Fatalf("should match: %s", r)
	}
	bad := map[string]string{
		`"ref":"refs/tags/v1.2"`:      `"ref":"refs/tags/v1.2-rc1"`,
		`"user":"ana"`:                `"user":"bot"`,
		`"size":50`:                   `"size":5`,
		`"size":50,`:                  `"size":"big",`,
		`"flag":true`:                 `"flag":true,"dry_run":false`,
		`"title":"[deploy] api now!"`: `"title":"[deploy] web now!"`,
		`"env":"prod"`:                `"env":"dev"`,
	}
	for from, to := range bad {
		js := strings.Replace(ok, from, to, 1)
		if r := matchWhen(conds, doc(js)); r == "" {
			t.Errorf("%s should not match", to)
		}
	}

	anyOf, _ := parseWhen(map[string]any{"action": "deploy", "labels.force": true})
	if matchWhenAny(anyOf, doc(`{"action":"x","labels":{"force":true}}`)) != "" || matchWhenAny(anyOf, doc(`{"action":"deploy"}`)) != "" {
		t.Error("when_any: one is enough")
	}
	if r := matchWhenAny(anyOf, doc(`{"action":"x"}`)); !strings.Contains(r, "none of when_any") {
		t.Errorf("when_any none: %q", r)
	}

	for _, bad := range []map[string]any{
		{"a": map[string]any{"regex": "x"}},
		{"a": map[string]any{"gt": "ten"}},
		{"a": map[string]any{"match": "("}},
		{"a": map[string]any{"exists": false, "not": "x"}},
		{"a": map[string]any{}},
	} {
		if _, err := parseWhen(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// TestSharedPath: one webhook, several deploys, each with its own rules.
func TestSharedPath(t *testing.T) {
	e := newGenericEnv(t, `echo "svc ran for $ACTION"`, `
[deploy.svc.when]
action = { in = ["deploy", "redeploy"] }
[deploy.svc.params.ACTION]
from = "action"
[deploy.svc-tags]
provider = "generic"
path = "/hooks/svc"
secret_env = "TEST_WEBHOOK_SECRET"
command = "/bin/sh"
args = ["-c", "echo tags ran for $TAG"]
timeout = "5s"
[deploy.svc-tags.when_any]
tag = { match = "^v[0-9]" }
force = true
[deploy.svc-tags.params.TAG]
from = "tag"
`, "")

	// Both match.
	rec := e.send(t, sendOpts{body: `{"action":"deploy","tag":"v1.0"}`, delivery: "s1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("both: %d %s", rec.Code, rec.Body)
	}
	var res struct{ Results []sharedResult }
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Results) != 2 || res.Results[0].Deploy != "svc" || res.Results[1].Deploy != "svc-tags" ||
		res.Results[0].Code != 202 || res.Results[1].Code != 202 {
		t.Fatalf("results %s", rec.Body)
	}
	e.waitIdleName(t, "svc")
	e.waitIdleName(t, "svc-tags")
	if !strings.Contains(readLatest(t, e, "svc"), "svc ran for deploy") || !strings.Contains(readLatest(t, e, "svc-tags"), "tags ran for v1.0") {
		t.Error("logs")
	}

	// Only one matches: still 202, the other is ignored.
	rec = e.send(t, sendOpts{body: `{"action":"build","tag":"v2.0"}`, delivery: "s2"})
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusAccepted || res.Results[0].Code != 200 || res.Results[1].Code != 202 {
		t.Fatalf("one: %d %s", rec.Code, rec.Body)
	}
	e.waitIdleName(t, "svc-tags")

	// None: 200 ignored.
	if rec := e.send(t, sendOpts{body: `{"action":"build","tag":"x"}`, delivery: "s3"}); rec.Code != http.StatusOK {
		t.Errorf("none: %d %s", rec.Code, rec.Body)
	}
	// Bad signature: refused by all.
	if rec := e.send(t, sendOpts{body: `{"action":"deploy"}`, unsigned: true}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned: %d", rec.Code)
	}
	// The same delivery again is a duplicate for each deploy, not across them.
	rec = e.send(t, sendOpts{body: `{"action":"deploy","tag":"v1.0"}`, delivery: "s1"})
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || !strings.Contains(string(res.Results[0].Response), "duplicate") {
		t.Errorf("duplicate: %d %s", rec.Code, rec.Body)
	}

	// The nginx snippet lists the path once.
	if n := strings.Count(nginxSnippet(e.cfg, false), "location = /hooks/svc "); n != 1 {
		t.Errorf("nginx blocks for the shared path: %d", n)
	}
}

func TestSharedPathValidation(t *testing.T) {
	base := "[logging]\ndirectory = \"/tmp/x\"\n"
	dep := func(name, extra string) string {
		return "[deploy." + name + "]\nprovider = \"generic\"\npath = \"/hooks/a\"\nsecret_env = \"S\"\ncommand = \"/bin/true\"\n" + extra
	}
	cases := map[string]string{
		dep("a", "") + dep("b", "auth = \"token\"\n"):                                                                         "same auth",
		dep("a", "") + strings.Replace(dep("b", ""), "secret_env = \"S\"", "secret_env = \"T\"", 1):                           "same secret_env",
		dep("a", "") + "[deploy.b]\npath = \"/hooks/a\"\nrepository = \"x/y\"\nsecret_env = \"S\"\ncommand = \"/bin/true\"\n": "same provider",
	}
	for cfg, want := range cases {
		path := filepath.Join(t.TempDir(), "c.toml")
		_ = os.WriteFile(path, []byte(base+cfg), 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want %q", err, want)
		}
	}
	path := filepath.Join(t.TempDir(), "c.toml")
	_ = os.WriteFile(path, []byte(base+dep("a", "")+dep("b", "")), 0o600)
	if _, err := LoadConfig(path); err != nil {
		t.Errorf("compatible shared path: %v", err)
	}
}
