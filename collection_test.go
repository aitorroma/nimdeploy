package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnsibleCollection runs the collection's modules from a playbook
// against a real nimdeploy server.
func TestAnsibleCollection(t *testing.T) {
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook not installed")
	}
	e := newGenericEnv(t, `echo "svc $ACTION"; [ "$ACTION" != fail ]`, `[deploy.svc.params.ACTION]
from = "action"
enum = ["run", "fail"]
[deploy.svc.labels]
environment = "stage"`, `[server]
api_token_env = "TEST_API_TOKEN"
`)
	srv := httptest.NewServer(e.h)
	t.Cleanup(srv.Close)
	collections, _ := filepath.Abs("ansible")
	dir := t.TempDir()
	py, _ := exec.LookPath("python3")
	playbook := `- hosts: localhost
  connection: local
  gather_facts: false
  vars:
    ansible_python_interpreter: ` + py + `
    url: "` + srv.URL + `"
  tasks:
    - name: deploy and wait
      aitorroma.nimdeploy.nimdeploy_run:
        url: "{{ url }}"
        api_token: "` + testToken + `"
        deploy: svc
        params: { ACTION: run }
        delivery_id: job-1
        poll_interval: 1
      register: first
    - assert:
        that: [first.changed, first.result == 'started', first.state.status == 'success']

    - name: the same delivery again runs nothing
      aitorroma.nimdeploy.nimdeploy_run:
        url: "{{ url }}"
        api_token: "` + testToken + `"
        deploy: svc
        params: { ACTION: run }
        delivery_id: job-1
        poll_interval: 1
      register: again
    - assert:
        that: [not again.changed, again.result == 'duplicate', again.state.status == 'success']

    - name: a failing deploy fails the task
      aitorroma.nimdeploy.nimdeploy_run:
        url: "{{ url }}"
        api_token: "` + testToken + `"
        deploy: svc
        params: { ACTION: fail }
        poll_interval: 1
      register: failed
      ignore_errors: true
    - assert:
        that: [failed is failed, failed.state.status == 'failed']

    - name: invalid params are refused
      aitorroma.nimdeploy.nimdeploy_run:
        url: "{{ url }}"
        api_token: "` + testToken + `"
        deploy: svc
        params: { ACTION: "rm -rf" }
      register: refused
      ignore_errors: true
    - assert:
        that: [refused is failed, "'must be one of' in refused.msg"]

    - name: signed webhook
      aitorroma.nimdeploy.nimdeploy_send:
        url: "{{ url }}/hooks/svc"
        secret: "` + testSecret + `"
        body: { action: run }
        delivery_id: hook-1
      register: sent
    - assert:
        that: [sent.changed, sent.status == 202]

    - name: wrong secret
      aitorroma.nimdeploy.nimdeploy_send:
        url: "{{ url }}/hooks/svc"
        secret: wrong
        body: { action: run }
      register: bad
      ignore_errors: true
    - assert:
        that: [bad is failed, bad.status == 401]

    - name: status by label
      aitorroma.nimdeploy.nimdeploy_status:
        url: "{{ url }}"
        api_token: "` + testToken + `"
        labels: { environment: stage }
      register: st
    - assert:
        that: ["'svc' in st.deploys", "'agency' not in st.deploys"]

    - name: check mode changes nothing
      aitorroma.nimdeploy.nimdeploy_run:
        url: "{{ url }}"
        api_token: "` + testToken + `"
        deploy: svc
      check_mode: true
      register: dry
    - assert:
        that: [dry.changed, dry.result == 'would_run']
`
	path := filepath.Join(dir, "play.yml")
	if err := os.WriteFile(path, []byte(playbook), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ansible-playbook", "-i", "localhost,", path)
	cmd.Env = append(os.Environ(), "ANSIBLE_COLLECTIONS_PATH="+collections, "ANSIBLE_NOCOLOR=1", "ANSIBLE_RETRY_FILES_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "failed=0") {
		t.Fatalf("playbook: %v\n%s", err, out)
	}
	e.waitIdleName(t, "svc")
	if h, _ := e.runner.History("svc", 0); len(h) != 3 {
		t.Errorf("runs: %d, want 3 (deploy, failing deploy, webhook)", len(h))
	}
	_ = http.StatusOK
}
