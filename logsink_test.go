package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLogSink(t *testing.T) {
	var out bytes.Buffer
	var exported []logRecord
	s := &logSink{out: &out, json: true, service: "nimdeploy", export: func(r logRecord) { exported = append(exported, r) }}
	s.secrets = []string{"s3cr3t-token-value"}

	_, _ = s.Write([]byte(`deploy=web status=failed duration=3s run=20261008-1 error="exit status 1: \"boom\"" log=/x.log` + "\n"))
	_, _ = s.Write([]byte("http POST /hooks/web status=401 client=1.2.3.4 delivery=d1"))
	_, _ = s.Write([]byte("\nnotify: cannot reach https://hooks.slack.com/s3cr3t-token-value\n"))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines:\n%s", out.String())
	}
	var first map[string]string
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["level"] != "error" || first["deploy"] != "web" || first["run"] != "20261008-1" || first["error"] != `exit status 1: "boom"` || first["msg"] != "" || first["service"] != "nimdeploy" {
		t.Errorf("first %v", first)
	}
	var second map[string]string
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if second["msg"] != "http POST /hooks/web" || second["status"] != "401" || second["level"] != "info" {
		t.Errorf("second %v", second)
	}
	if strings.Contains(out.String(), "s3cr3t-token-value") || !strings.Contains(lines[2], redacted) || !strings.Contains(lines[2], `"level":"error"`) {
		t.Errorf("redaction: %s", lines[2])
	}
	if len(exported) != 3 || exported[0].field("deploy") != "web" {
		t.Errorf("export %+v", exported)
	}

	out.Reset()
	s.json = false
	_, _ = s.Write([]byte("deploy=web status=started token=s3cr3t-token-value\n"))
	if !strings.HasSuffix(out.String(), "deploy=web status=started token=[REDACTED]\n") {
		t.Errorf("text: %q", out.String())
	}

	setLogSecrets([]string{"short", "a-long-enough-secret", "a-long-enough-secret"})
	if len(serviceLog.secrets) != 1 {
		t.Errorf("secrets %v", serviceLog.secrets)
	}
	setLogSecrets(nil)
}
