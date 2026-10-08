package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testPlaybook = `- hosts: all
  gather_facts: false
  tasks:
    - name: show vars
      debug:
        msg: "vars deploy={{ nimdeploy.deploy }} trigger={{ nimdeploy.trigger }} service={{ service | default('none') }} version={{ app_version }} client={{ nimdeploy.labels.client | default('') }}"
    - name: change something
      command: /bin/true
      changed_when: true
    - name: fail on bad
      fail:
        msg: boom
      when: inventory_hostname == 'bad'
`

// ansibleDir writes a playbook and an inventory with a good host, a failing
// one and an unreachable one (a closed local port).
func ansibleDir(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook not installed")
	}
	dir := t.TempDir()
	py, _ := exec.LookPath("python3")
	inv := `[web]
good ansible_connection=local ansible_python_interpreter=` + py + `
bad ansible_connection=local ansible_python_interpreter=` + py + `
[db]
gone ansible_host=127.0.0.1 ansible_port=1 ansible_connection=ssh ansible_ssh_retries=0 ansible_ssh_common_args="-o ConnectTimeout=1 -o BatchMode=yes -o StrictHostKeyChecking=no"
`
	for name, content := range map[string]string{"site.yml": testPlaybook, "hosts.ini": inv} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAnsibleExecutor(t *testing.T) {
	dir := ansibleDir(t)
	e := newEnv(t, `true`, `[server]
api_token_env = "TEST_API_TOKEN"
[labels]
client = "Acme"
[deploy.infra]
provider = "generic"
path = "/hooks/svc"
secret_env = "TEST_WEBHOOK_SECRET"
working_directory = "`+dir+`"
timeout = "2m"
[deploy.infra.ansible]
playbook = "site.yml"
inventory = ["hosts.ini"]
limit = "${GROUP}"
extra_vars = { app_version = "v-${VERSION}" }
[deploy.infra.params.GROUP]
from = "group"
enum = ["web", "db", "good"]
[deploy.infra.params.SERVICE]
from = "service"
[deploy.infra.params.VERSION]
from = "version"
`)
	// web: good succeeds, bad fails.
	rec := e.send(t, sendOpts{body: `{"group":"web","service":"api","version":"1.2"}`, delivery: "a1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	st := e.waitIdleName(t, "infra")
	out := readLatest(t, e, "infra")
	if st.Status != StatusFailed || st.Ansible == nil {
		t.Fatalf("state %+v\n%s", st, out)
	}
	a := st.Ansible
	if a.Hosts != 2 || a.HostsFailed != 1 || a.HostsChanged != 1 || !reflect.DeepEqual(a.FailedHosts, []string{"bad"}) || st.Error != "ansible: failed on bad" {
		t.Errorf("summary %+v error %q", a, st.Error)
	}
	if !strings.Contains(out, "vars deploy=infra trigger=webhook service=api version=v-1.2 client=Acme") {
		t.Errorf("extra vars not passed:\n%s", out)
	}
	if !strings.Contains(out, "--limit web") || !strings.Contains(out, "ansible: 2 hosts: ok=") {
		t.Errorf("log:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.logDir, "infra")); err == nil {
		matches, _ := filepath.Glob(filepath.Join(e.logDir, "infra", ".*ansible-vars.json"))
		if len(matches) != 0 {
			t.Errorf("vars file left behind: %v", matches)
		}
	}

	// good only: success.
	e.send(t, sendOpts{body: `{"group":"good","version":"2"}`, delivery: "a2"})
	st = e.waitIdleName(t, "infra")
	if st.Status != StatusSuccess || st.Ansible == nil || st.Ansible.HostsChanged != 1 || st.Ansible.Hosts != 1 {
		t.Errorf("good: %+v %+v", st, st.Ansible)
	}

	// db: unreachable.
	e.send(t, sendOpts{body: `{"group":"db","version":"3"}`, delivery: "a3"})
	st = e.waitIdleName(t, "infra")
	if st.Status != StatusFailed || st.Ansible == nil || st.Ansible.HostsUnreachable != 1 || !strings.Contains(st.Error, "unreachable: gone") {
		t.Errorf("db: %+v %+v", st, st.Ansible)
	}

	// No group: the limit would be empty (every host), so it doesn't run.
	e.send(t, sendOpts{body: `{"version":"4"}`, delivery: "a4"})
	st = e.waitIdleName(t, "infra")
	if st.Status != StatusFailed || !strings.Contains(st.Error, "limit") || st.Ansible != nil {
		t.Errorf("empty limit: %+v", st)
	}

	body := e.request(http.MethodGet, "/metrics", testToken, "").Body.String()
	for _, want := range []string{
		`nimdeploy_ansible_hosts_total{deploy="infra",result="failed"} 1`,
		`nimdeploy_ansible_hosts_total{deploy="infra",result="changed"} 2`,
		`nimdeploy_ansible_hosts_total{deploy="infra",result="unreachable"} 1`,
		`nimdeploy_phase_duration_seconds_count{deploy="infra",phase="command"} 4`,
		`nimdeploy_queue_wait_seconds_count{deploy="infra"} 4`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestAnsibleCheckModeAndPull(t *testing.T) {
	dir := ansibleDir(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// A git repository with local.yml, for ansible-pull.
	repo := filepath.Join(t.TempDir(), "playbooks")
	py, _ := exec.LookPath("python3")
	local := "- hosts: localhost\n  connection: local\n  gather_facts: false\n  vars:\n    ansible_python_interpreter: " + py +
		"\n  tasks:\n    - debug:\n        msg: \"pulled {{ nimdeploy.deploy }}\"\n"
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(repo, "local.yml"), []byte(local), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	e := newEnv(t, `true`, `
[deploy.check]
schedule = "@daily"
working_directory = "`+dir+`"
timeout = "2m"
[deploy.check.ansible]
playbook = "site.yml"
inventory = ["hosts.ini"]
limit = "good"
check = true
diff = true
extra_vars = { app_version = "x" }
[deploy.pull]
schedule = "@daily"
timeout = "2m"
[deploy.pull.ansible]
mode = "pull"
url = "file://`+repo+`"
checkout = "main"
directory = "`+filepath.Join(t.TempDir(), "checkout")+`"
inventory = ["localhost,"]
`)
	if _, err := e.runner.Submit("check", Trigger{Source: TriggerManual}); err != nil {
		t.Fatal(err)
	}
	st := e.waitIdleName(t, "check")
	out := readLatest(t, e, "check")
	if st.Status != StatusSuccess || !strings.Contains(out, "--check --diff") || st.Ansible == nil || st.Ansible.Hosts != 1 {
		t.Errorf("check: %+v\n%s", st, out)
	}

	if _, err := e.runner.Submit("pull", Trigger{Source: TriggerManual}); err != nil {
		t.Fatal(err)
	}
	st = e.waitIdleName(t, "pull")
	out = readLatest(t, e, "pull")
	if st.Status != StatusSuccess || !strings.Contains(out, "pulled pull") || st.Ansible == nil {
		t.Errorf("pull: %+v\n%s", st, out)
	}
}

func TestAnsibleArgsAndValidation(t *testing.T) {
	a := &AnsibleConfig{Mode: "playbook", Playbook: "site.yml", Inventory: []string{"inv/prod", "aws_ec2.yml"},
		Limit: "web:&${ENV}", Tags: []string{"deploy", "${TAG}", ""}, SkipTags: []string{"slow"}, Forks: 5,
		Verbosity: 2, VaultPasswordFile: "/etc/vault.pass", Args: []string{"--become"}}
	params := []Param{{Name: "ENV", Value: "prod"}, {Name: "TAG", Value: "api"}}
	got, err := ansibleArgs(a, params, "/tmp/v.json")
	want := []string{"--inventory", "inv/prod", "--inventory", "aws_ec2.yml", "--limit", "web:&prod", "--tags", "deploy,api",
		"--skip-tags", "slow", "--forks", "5", "--vault-password-file", "/etc/vault.pass", "--extra-vars", "@/tmp/v.json", "-vv", "--become", "site.yml"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("args\n got %q\nwant %q (%v)", got, want, err)
	}
	if _, err := ansibleArgs(&AnsibleConfig{Limit: "${ENV}"}, nil, ""); err == nil {
		t.Error("empty limit accepted")
	}

	p := &recapParser{}
	_, _ = p.Write([]byte("PLAY RECAP ****\nweb1  : ok=3 changed=1 unreachable=0 failed=0 skipped=2 rescued=0 ignored=0\nweb2 : ok=1 changed=0 unreachable=0 failed=1 skipped=0 rescued=1 ignored=1\n"))
	_, _ = p.Write([]byte("db1 : ok=0 changed=0 unreachable=1 failed=0"))
	s := p.summary()
	if s.Hosts != 3 || s.Ok != 4 || s.Changed != 1 || s.Failed != 1 || s.Unreachable != 1 || s.Skipped != 2 || s.Rescued != 1 ||
		s.HostsChanged != 1 || s.HostsFailed != 1 || s.HostsUnreachable != 1 || s.failure() != "failed on web2; unreachable: db1" {
		t.Errorf("summary %+v", s)
	}

	base := "[logging]\ndirectory = \"/tmp/x\"\n[deploy.a]\nschedule = \"@daily\"\n"
	cases := map[string]string{
		"command = \"/bin/true\"\n[deploy.a.ansible]\nplaybook = \"x.yml\"\n": "not both",
		"[deploy.a.ansible]\n":                                                   "playbook is required",
		"[deploy.a.ansible]\nmode = \"pull\"\n":                                  "url",
		"[deploy.a.ansible]\nplaybook = \"x\"\nlimit = \"${NOPE}\"\n":            "not one of the params",
		"[deploy.a.ansible]\nplaybook = \"x\"\nverbosity = 9\n":                  "verbosity",
		"[deploy.a.ansible]\nplaybook = \"x\"\nextra_vars = { nimdeploy = 1 }\n": "reserved",
		"[deploy.a.ansible]\nplaybook = \"x\"\nextra_vars = { \"a-b\" = 1 }\n":   "valid variable name",
		"[deploy.a.ansible]\nplaybook = \"x\"\nurl = \"https://x\"\n":            "for mode = \"pull\"",
		"[deploy.a]\n": "",
	}
	for extra, want := range cases {
		if want == "" {
			continue
		}
		path := filepath.Join(t.TempDir(), "c.toml")
		_ = os.WriteFile(path, []byte(base+extra), 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", extra, err, want)
		}
	}
	path := filepath.Join(t.TempDir(), "c.toml")
	_ = os.WriteFile(path, []byte(base+"[deploy.a.ansible]\nplaybook = \"x.yml\"\nextra_vars = { nested = { list = [1, \"${X}\"] } }\n[deploy.a.params.X]\nfrom = \"x\"\n"), 0o600)
	if _, err := LoadConfig(path); err != nil {
		t.Errorf("nested extra_vars: %v", err)
	}
}
