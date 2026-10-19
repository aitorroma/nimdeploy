package main

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A helm that keeps each release's revision in $HELM_STATE/<ns>-<name> and
// fails the upgrade of $HELM_FAIL (namespace/name).
const fakeHelm = `#!/bin/sh
echo "$*" >> "$HELM_STATE/calls"
cmd=$1; shift
case $cmd in
status)
  f="$HELM_STATE/$3-$1"
  [ -f "$f" ] || { echo "Error: release: not found" >&2; exit 1; }
  echo "{\"name\":\"$1\",\"version\":$(cat "$f")}";;
upgrade)
  name=$2; ns=$5; f="$HELM_STATE/$ns-$name"
  if [ "$ns/$name" = "$HELM_FAIL" ]; then
    echo "Error: UPGRADE FAILED: release $name failed, and has been rolled back due to atomic being set: context deadline exceeded" >&2
    exit 1
  fi
  rev=$(cat "$f" 2>/dev/null || echo 0); echo $((rev+1)) > "$f"
  echo "Release \"$name\" has been upgraded. Happy Helming!";;
rollback)
  f="$HELM_STATE/$4-$1"; rev=$(cat "$f"); echo $((rev+1)) > "$f"
  echo "Rollback was a success! Happy Helming!";;
esac
`

func helmEnv(t *testing.T) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "helm")
	if err := os.WriteFile(bin, []byte(fakeHelm), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	os.Mkdir(state, 0o755)
	os.WriteFile(filepath.Join(state, "ns-a-a"), []byte("3"), 0o644)
	os.WriteFile(filepath.Join(state, "ns-b-b"), []byte("2"), 0o644)
	t.Setenv("HELM_STATE", state)
	t.Setenv("HELM_FAIL", "")
	releases := `
[[deploy.%s.helm.release]]
name = "a"
namespace = "ns-a"
values = ["instances/a.yaml"]
[[deploy.%s.helm.release]]
name = "b"
namespace = "ns-b"
values = ["instances/b.yaml"]
set = { "tunnel.name" = "bee" }
[[deploy.%s.helm.release]]
name = "c"
namespace = "ns-c"
`
	rel := func(name string) string { return strings.ReplaceAll(releases, "%s", name) }
	e := newEnv(t, `true`, `[server]
api_token_env = "TEST_API_TOKEN"
[deploy.ship]
provider = "generic"
path = "/hooks/svc"
secret_env = "TEST_WEBHOOK_SECRET"
working_directory = "`+dir+`"
timeout = "1m"
[deploy.ship.params.VERSION]
from = "version"
match = "^[0-9.]+$"
[deploy.ship.helm]
chart = "./chart"
binary = "`+bin+`"
kubeconfig = "/etc/k3s.yaml"
timeout = "3m"
values = ["common.yaml"]
set = { "image.tag" = "${VERSION}" }
`+rel("ship")+`
[deploy.roll]
schedule = "@yearly"
working_directory = "`+dir+`"
timeout = "1m"
[deploy.roll.helm]
chart = "./chart"
binary = "`+bin+`"
on_failure = "rollback"
`+rel("roll"))
	return e, state
}

func TestHelmExecutor(t *testing.T) {
	e, state := helmEnv(t)
	rev := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(state, name))
		return strings.TrimSpace(string(b))
	}

	// Every release, in order: two upgrades and a new install.
	if rec := e.send(t, sendOpts{body: `{"version":"1.0"}`, delivery: "h1"}); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	st := e.waitIdleName(t, "ship")
	out := readLatest(t, e, "ship")
	want := []HelmResult{
		{Release: "a", Namespace: "ns-a", Outcome: "upgraded", Revision: 4, Previous: 3},
		{Release: "b", Namespace: "ns-b", Outcome: "upgraded", Revision: 3, Previous: 2},
		{Release: "c", Namespace: "ns-c", Outcome: "installed", Revision: 1},
	}
	if st.Status != StatusSuccess || st.Helm == nil || !reflect.DeepEqual(st.Helm.Releases, want) || st.Helm.Upgraded != 3 {
		t.Fatalf("state %+v %+v\n%s", st, st.Helm, out)
	}
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	for _, w := range []string{
		"upgrade --install a ./chart --namespace ns-a --kubeconfig /etc/k3s.yaml --wait --timeout 3m0s --atomic --values common.yaml --values instances/a.yaml --set-string image.tag=1.0",
		"upgrade --install b ./chart --namespace ns-b --kubeconfig /etc/k3s.yaml --wait --timeout 3m0s --atomic --values common.yaml --values instances/b.yaml --set-string image.tag=1.0 --set-string tunnel.name=bee",
		"status c --namespace ns-c --output json --kubeconfig /etc/k3s.yaml",
	} {
		if !strings.Contains(string(calls), w) {
			t.Errorf("missing call %q in\n%s", w, calls)
		}
	}
	if !strings.Contains(out, "helm: 3 releases: 3 upgraded") || !strings.Contains(out, "[2/3]") {
		t.Errorf("log:\n%s", out)
	}

	// b fails: a keeps the new version, c is not reached.
	t.Setenv("HELM_FAIL", "ns-b/b")
	e.send(t, sendOpts{body: `{"version":"1.1"}`, delivery: "h2"})
	st = e.waitIdleName(t, "ship")
	h := st.Helm
	if st.Status != StatusFailed || h == nil || h.Releases[0].Outcome != "upgraded" || h.Releases[1].Outcome != "failed" ||
		h.Releases[2].Outcome != "skipped" || h.Skipped != 1 || h.Failed != 1 {
		t.Fatalf("stop: %+v %+v", st, h)
	}
	if !strings.Contains(st.Error, "helm: ns-b/b: UPGRADE FAILED") || rev("ns-c-c") != "1" {
		t.Errorf("error %q, c at %s", st.Error, rev("ns-c-c"))
	}

	// on_failure = "rollback": a goes back to the revision it had.
	if _, err := e.runner.Submit("roll", Trigger{Source: TriggerManual}); err != nil {
		t.Fatal(err)
	}
	st = e.waitIdleName(t, "roll")
	h = st.Helm
	if st.Status != StatusFailed || h == nil || h.Releases[0].Outcome != "rolled_back" || h.Releases[0].Previous != 5 || h.RolledBack != 1 {
		t.Fatalf("rollback: %+v %+v\n%s", st, h, readLatest(t, e, "roll"))
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(state, "calls"))), "rollback a 5 --namespace ns-a --wait") {
		t.Error("no helm rollback a 5")
	}

	body := e.request(http.MethodGet, "/metrics", testToken, "").Body.String()
	for _, w := range []string{
		`nimdeploy_helm_releases_total{deploy="ship",result="upgraded"} 3`,
		`nimdeploy_helm_releases_total{deploy="ship",result="failed"} 1`,
		`nimdeploy_helm_last_run_releases{deploy="roll",result="rolled_back"} 1`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("metrics missing %s", w)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHelmArgsAndValidation(t *testing.T) {
	h := &HelmConfig{Chart: "oci://r/app", Version: "0.1.0", CreateNamespace: true, KubeContext: "prod",
		Set: map[string]string{"image.tag": "${DEPLOY_TAG}", "note": "a,b\\c"}, Args: []string{"--debug"}}
	h.Timeout.Duration = time.Minute
	got, err := helmUpgradeArgs(h, HelmRelease{Name: "x", Namespace: "y"}, Trigger{Tag: "v1.2.0"})
	want := []string{"upgrade", "--install", "x", "oci://r/app", "--namespace", "y", "--kube-context", "prod", "--create-namespace",
		"--version", "0.1.0", "--wait", "--timeout", "1m0s", "--set-string", "image.tag=v1.2.0", "--set-string", `note=a\,b\\c`, "--debug"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("args\n got %q\nwant %q (%v)", got, want, err)
	}
	// No tag: an empty image.tag is refused, never "the chart's default".
	if _, err := helmUpgradeArgs(h, HelmRelease{Name: "x", Namespace: "y"}, Trigger{}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty set accepted: %v", err)
	}
	e := &helmErrors{}
	e.Write([]byte("coalesce.go: warning\nError: UPGRADE FAILED: boom\nmore"))
	if e.last() != "UPGRADE FAILED: boom" {
		t.Errorf("last error %q", e.last())
	}
	s := &HelmSummary{Releases: []HelmResult{{Release: "a", Namespace: "n", Outcome: "upgraded"}, {Release: "b", Namespace: "n", Outcome: "failed"}, {Release: "c", Namespace: "n", Outcome: "skipped"}}}
	s.count()
	if s.text() != "3 releases: 1 upgraded, failed: n/b, 1 not reached" {
		t.Errorf("text %q", s.text())
	}

	// The customer from the webhook: one release named after it.
	hc := &HelmConfig{Releases: []HelmRelease{{Client: "${CLIENTE}", Name: "proyecto", Namespace: "proyecto-${CLIENTE}", Values: []string{"instances/${CLIENTE}.yaml"}}}}
	got2, err := resolveReleases(hc, Trigger{Params: []Param{{Name: "CLIENTE", Value: "cliente1"}}})
	if err != nil || got2[0].Namespace != "proyecto-cliente1" || got2[0].Client != "cliente1" || got2[0].Values[0] != "instances/cliente1.yaml" {
		t.Errorf("resolved %+v %v", got2, err)
	}
	for _, bad := range []string{"", "Cliente1", "../x", "a_b"} {
		if _, err := resolveReleases(hc, Trigger{Params: []Param{{Name: "CLIENTE", Value: bad}}}); err == nil {
			t.Errorf("client %q accepted", bad)
		}
	}
	if l := (HelmResult{Client: "cliente1", Namespace: "proyecto-cliente1", Release: "proyecto"}).label(); l != "cliente1 (proyecto-cliente1/proyecto)" {
		t.Errorf("label %q", l)
	}

	base := "[logging]\ndirectory = \"/tmp/x\"\n[deploy.a]\nschedule = \"@daily\"\n"
	rel := "[[deploy.a.helm.release]]\nname = \"r\"\nnamespace = \"n\"\n"
	cases := map[string]string{
		"command = \"/bin/true\"\n[deploy.a.helm]\nchart = \"c\"\n" + rel: "not both",
		"[deploy.a.helm]\n" + rel:                                                                              "chart is required",
		"[deploy.a.helm]\nchart = \"c\"\n":                                                                     "at least one",
		"[deploy.a.helm]\nchart = \"c\"\non_failure = \"retry\"\n" + rel:                                       "on_failure",
		"[deploy.a.helm]\nchart = \"c\"\nset = { \"image.tag\" = \"${NOPE}\" }\n" + rel:                        "not one of the params",
		"[deploy.a.helm]\nchart = \"c\"\nset = { \"a b\" = \"x\" }\n" + rel:                                    "value path",
		"[deploy.a.helm]\nchart = \"c\"\n[[deploy.a.helm.release]]\nname = \"R\"\nnamespace = \"n\"\n":         "lowercase",
		"[deploy.a.helm]\nchart = \"c\"\n[[deploy.a.helm.release]]\nname = \"r\"\n":                            "namespace is required",
		"[deploy.a.helm]\nchart = \"c\"\n" + rel + rel:                                                         "twice",
		"[deploy.a.helm]\nchart = \"c\"\n[[deploy.a.helm.release]]\nname = \"r\"\nnamespace = \"n-${NOPE}\"\n": "not one of the params",
	}
	for extra, want := range cases {
		path := filepath.Join(t.TempDir(), "c.toml")
		_ = os.WriteFile(path, []byte(base+extra), 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", extra, err, want)
		}
	}
	path := filepath.Join(t.TempDir(), "c.toml")
	_ = os.WriteFile(path, []byte(base+"[deploy.a.helm]\nchart = \"c\"\nset = { \"image.tag\" = \"${DEPLOY_TAG}\" }\n"+rel), 0o600)
	if _, err := LoadConfig(path); err != nil {
		t.Errorf("valid helm config: %v", err)
	}
}
