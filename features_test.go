package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCronNext(t *testing.T) {
	loc := time.UTC
	base := time.Date(2026, 10, 6, 10, 7, 30, 0, loc) // a Tuesday
	cases := []struct{ spec, want string }{
		{"*/15 * * * *", "2026-10-06 10:15"},
		{"0 * * * *", "2026-10-06 11:00"},
		{"@hourly", "2026-10-06 11:00"},
		{"30 3 * * *", "2026-10-07 03:30"},
		{"@daily", "2026-10-07 00:00"},
		{"0 9 * * mon-fri", "2026-10-07 09:00"},
		{"0 9 * * 1-5", "2026-10-07 09:00"},
		{"0 0 * * 7", "2026-10-11 00:00"}, // 7 = Sunday
		{"0 0 * * sun", "2026-10-11 00:00"},
		{"0 0 1 * *", "2026-11-01 00:00"},
		{"0 0 1 jan *", "2027-01-01 00:00"},
		{"5,35 10 * * *", "2026-10-06 10:35"},
		{"0-10/5 11 * * *", "2026-10-06 11:00"},
		{"0 0 13 * fri", "2026-10-09 00:00"}, // day 13 OR a Friday
		{"0 0 29 2 *", "2028-02-29 00:00"},
	}
	for _, c := range cases {
		spec, err := parseCron(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := spec.next(base).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("%s: next = %s, want %s", c.spec, got, c.want)
		}
	}
	every, _ := parseCron("@every 90s")
	if got := every.next(base); got.Sub(base) != 90*time.Second {
		t.Errorf("@every: %v", got)
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "5-1 * * * *", "*/0 * * * *", "@every 10ms", "@sometimes", "0 0 32 * *", "* * * * funday"} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
	never, _ := parseCron("0 0 31 2 *")
	if !never.next(base).IsZero() {
		t.Error("Feb 31 should never run")
	}
}

func TestScheduledDeploy(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	e := newEnv(t, `echo agency`, `[deploy.tick]
schedule = "@every 1s"
command = "/bin/sh"
args = ["-c", "echo \"$DEPLOY_TRIGGER $DEPLOY_PROVIDER\" >> `+runs+`"]
`)
	if d := e.cfg.Deploy["tick"]; d.Provider != providerSchedule || d.Path != "" {
		t.Fatalf("deploy %+v", d)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(runs)
		if strings.Count(string(b), "schedule schedule") >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := os.ReadFile(runs)
	if strings.Count(string(b), "schedule schedule") < 2 {
		t.Fatalf("runs:\n%s", b)
	}
	st := e.runner.State("tick")
	if st.NextRun == nil || st.Trigger != TriggerSchedule {
		t.Fatalf("state %+v", st)
	}
	// No webhook route for a scheduled-only deploy, nor nginx block.
	if strings.Contains(nginxSnippet(e.cfg, false), "tick") {
		t.Error("nginx block for a scheduled-only deploy")
	}
}

func TestHooksAndHealthCheck(t *testing.T) {
	var mu sync.Mutex
	healthy := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !healthy {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()

	e := newEnv(t, `echo command-ran`, "")
	d := e.cfg.Deploy["agency"]
	d.Before = `echo before-ran`
	d.AfterSuccess = `echo after-success-ran`
	d.AfterFailure = `echo after-failure-ran`
	d.HealthURL = srv.URL
	d.HealthTimeout = Duration{1500 * time.Millisecond}

	e.push(t, pushOpts{delivery: "h1"})
	st := e.waitIdle(t)
	out := readLatest(t, e, "agency")
	if st.Status != StatusFailed || !strings.Contains(st.Error, "health check") || !strings.Contains(out, "after-failure-ran") || strings.Contains(out, "after-success-ran") {
		t.Fatalf("unhealthy: %+v\n%s", st, out)
	}

	mu.Lock()
	healthy = true
	mu.Unlock()
	e.push(t, pushOpts{delivery: "h2", commit: sha2})
	st = e.waitIdle(t)
	out = readLatest(t, e, "agency")
	order := []string{"hook=before", "before-ran", "command-ran", "health: GET", "hook=after_success", "after-success-ran"}
	pos := 0
	for _, w := range order {
		i := strings.Index(out[pos:], w)
		if i < 0 {
			t.Fatalf("missing %q (in order) in:\n%s", w, out)
		}
		pos += i
	}
	if st.Status != StatusSuccess {
		t.Fatalf("healthy: %+v", st)
	}

	// A failing before hook stops the deploy before the command.
	d.Before = `echo nope; exit 4`
	e.push(t, pushOpts{delivery: "h3", commit: sha3})
	st = e.waitIdle(t)
	out = readLatest(t, e, "agency")
	if st.Status != StatusFailed || !strings.HasPrefix(st.Error, "before hook") || strings.Contains(out, "\ncommand-ran\n") || !strings.Contains(out, "after-failure-ran") {
		t.Fatalf("before failed: %+v\n%s", st, out)
	}
}

func TestCloudflarePurge(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(b))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"success":true,"errors":[]}`))
	}))
	defer cf.Close()
	t.Setenv("TEST_CF_TOKEN", "cf-token")
	e := newEnv(t, `true`, `[cloudflare]
api_token_env = "TEST_CF_TOKEN"
api_url = "`+cf.URL+`"
`)
	d := e.cfg.Deploy["agency"]
	d.CloudflareZoneID = "zone123"
	d.CloudflarePurge = []string{"everything"}
	e.push(t, pushOpts{delivery: "c1"})
	e.waitIdle(t)
	urls := make([]string, 35)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://example.com/p%d", i)
	}
	d.CloudflarePurge = urls
	e.push(t, pushOpts{delivery: "c2", commit: sha2})
	e.waitIdle(t)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 || !strings.Contains(calls[0], "/zones/zone123/purge_cache Bearer cf-token {\"purge_everything\":true}") ||
		strings.Count(calls[1], "https://example.com/") != 30 || strings.Count(calls[2], "https://example.com/") != 5 {
		t.Fatalf("calls %q", calls)
	}
	if !strings.Contains(readLatest(t, e, "agency"), "cloudflare: purged") {
		t.Error("purge not logged")
	}
}

func TestRollback(t *testing.T) {
	e := newEnv(t, `echo "deploying $DEPLOY_COMMIT ($DEPLOY_TRIGGER)"; [ "$DEPLOY_COMMIT" != "`+sha3+`" ]`, `[server]
api_token_env = "TEST_API_TOKEN"
`)
	if rec := e.request(http.MethodPost, "/rollback/agency", testToken, ""); rec.Code != http.StatusConflict {
		t.Errorf("rollback with no history: %d %s", rec.Code, rec.Body)
	}
	e.push(t, pushOpts{delivery: "r1", commit: sha1})
	e.waitIdle(t)
	e.push(t, pushOpts{delivery: "r2", commit: sha2})
	e.waitIdle(t)
	if c, _, err := e.runner.RollbackTarget("agency"); err != nil || c != sha1 {
		t.Fatalf("target %s %v", c, err)
	}
	if rec := e.request(http.MethodPost, "/rollback/agency", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("rollback without token: %d", rec.Code)
	}
	rec := e.request(http.MethodPost, "/rollback/agency", testToken, `{"user":"ops"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("rollback: %d %s", rec.Code, rec.Body)
	}
	st := e.waitIdle(t)
	if st.Commit != sha1 || st.Trigger != TriggerRollback || st.Pusher != "ops" || !strings.Contains(readLatest(t, e, "agency"), "deploying "+sha1+" (rollback)") {
		t.Fatalf("after rollback %+v", st)
	}

	// rollback_on_failure: sha3 fails, the last good commit comes back by itself.
	e.cfg.Deploy["agency"].RollbackOnFailure = true
	e.push(t, pushOpts{delivery: "r3", commit: sha3})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := e.runner.State("agency"); st.Trigger == TriggerRollback && st.Status == StatusSuccess && st.Commit == sha1 {
			if !strings.Contains(st.Pusher, "auto") {
				t.Errorf("pusher %q", st.Pusher)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no automatic rollback: %+v", e.runner.State("agency"))
}

func hmacHex(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func TestPaymentProviders(t *testing.T) {
	type tc struct {
		provider, body, event, resource string
		sign                            func(body string, ts int64) map[string]string
	}
	now := time.Now().Unix()
	cases := []tc{
		{"stripe", `{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"id":"cs_9","client_reference_id":"42"}}}`, "checkout.session.completed", "cs_9",
			func(b string, ts int64) map[string]string {
				return map[string]string{"Stripe-Signature": fmt.Sprintf("t=%d,v1=%s,v1=%s,v0=old", ts, strings.Repeat("ab", 32), hmacHex(testSecret, fmt.Sprintf("%d.%s", ts, b)))}
			}},
		{"paddle", `{"event_id":"evt_01h","event_type":"transaction.completed","data":{"id":"txn_7"}}`, "transaction.completed", "txn_7",
			func(b string, ts int64) map[string]string {
				return map[string]string{"Paddle-Signature": fmt.Sprintf("ts=%d;h1=%s", ts, hmacHex(testSecret, fmt.Sprintf("%d:%s", ts, b)))}
			}},
		{"lemonsqueezy", `{"meta":{"event_name":"order_created","custom_data":{"plan":"pro"}},"data":{"type":"orders","id":"1001"}}`, "order_created", "1001",
			func(b string, _ int64) map[string]string {
				return map[string]string{"X-Signature": hmacHex(testSecret, b), "X-Event-Name": "order_created"}
			}},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			e := newEnv(t, `echo agency`, `[deploy.pay]
provider = "`+c.provider+`"
path = "/hooks/pay"
secret_env = "TEST_WEBHOOK_SECRET"
events = ["`+c.event+`"]
command = "/bin/sh"
args = ["-c", "echo \"$DEPLOY_EVENT $DEPLOY_RESOURCE_ID\"; test -s \"$DEPLOY_PAYLOAD_FILE\""]
`)
			post := func(body string, headers map[string]string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, "/hooks/pay", strings.NewReader(body))
				for k, v := range headers {
					req.Header.Set(k, v)
				}
				rec := httptest.NewRecorder()
				e.h.ServeHTTP(rec, req)
				return rec
			}
			if rec := post(c.body, c.sign(c.body, now)); rec.Code != http.StatusAccepted {
				t.Fatalf("valid: %d %s", rec.Code, rec.Body)
			}
			st := e.waitIdleName(t, "pay")
			if st.Status != StatusSuccess || st.Event != c.event || st.ResourceID != c.resource {
				t.Fatalf("state %+v", st)
			}
			if !strings.Contains(readLatest(t, e, "pay"), c.event+" "+c.resource) {
				t.Errorf("log:\n%s", readLatest(t, e, "pay"))
			}
			// The same event again: duplicate.
			if rec := post(c.body, c.sign(c.body, now)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
				t.Errorf("duplicate: %d %s", rec.Code, rec.Body)
			}
			// Tampered body.
			if rec := post(strings.Replace(c.body, c.resource, "x"+c.resource, 1), c.sign(c.body, now)); rec.Code != http.StatusUnauthorized {
				t.Errorf("tampered: %d", rec.Code)
			}
			if c.provider != "lemonsqueezy" {
				old := now - 3600
				if rec := post(c.body, c.sign(c.body, old)); rec.Code != http.StatusUnauthorized {
					t.Errorf("old timestamp: %d", rec.Code)
				}
			}
			// Another event type: ignored with 200 (the platform would retry an error).
			other := strings.Replace(c.body, c.event, "something.else", 1)
			other = strings.Replace(other, `"evt_`, `"evt_other`, 1)
			if c.provider == "lemonsqueezy" {
				other = strings.Replace(c.body, "order_created", "order_refunded", 1)
			}
			h := c.sign(other, now)
			if c.provider == "lemonsqueezy" {
				h["X-Event-Name"] = "order_refunded"
			}
			if rec := post(other, h); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ignored") {
				t.Errorf("other event: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestMetrics(t *testing.T) {
	e := newEnv(t, `[ "$DEPLOY_COMMIT" != "`+sha2+`" ]`, `[server]
api_token_env = "TEST_API_TOKEN"
`)
	e.push(t, pushOpts{delivery: "m1"})
	e.waitIdle(t)
	e.push(t, pushOpts{delivery: "m2", commit: sha2})
	e.waitIdle(t)
	e.push(t, pushOpts{delivery: "m3", badSig: true})

	if rec := e.request(http.MethodGet, "/metrics", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("metrics without token: %d", rec.Code)
	}
	rec := e.request(http.MethodGet, "/metrics", testToken, "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics: %d %s", rec.Code, rec.Header())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`nimdeploy_build_info{version="dev"`,
		`nimdeploy_deploys_total{deploy="agency",status="success"} 1`,
		`nimdeploy_deploys_total{deploy="agency",status="failed"} 1`,
		`nimdeploy_deploy_duration_seconds_bucket{deploy="agency",le="5"} 2`,
		`nimdeploy_deploy_duration_seconds_count{deploy="agency"} 2`,
		`nimdeploy_deploy_running{deploy="agency"} 0`,
		`nimdeploy_deploy_last_success{deploy="agency"} 0`,
		`nimdeploy_deploy_last_success_timestamp_seconds{deploy="agency"}`,
		`nimdeploy_webhook_requests_total{deploy="agency",code="202"} 2`,
		`nimdeploy_webhook_requests_total{deploy="agency",code="401"} 1`,
		"# TYPE nimdeploy_deploy_duration_seconds histogram",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	// Every line is a comment or "name{labels} number".
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.HasPrefix(l, "#") {
			continue
		}
		i := strings.LastIndexByte(l, ' ')
		if _, err := strconv.ParseFloat(l[i+1:], 64); err != nil || !strings.HasPrefix(l, "nimdeploy_") {
			t.Errorf("bad line %q", l)
		}
	}
}

func TestNewFeaturesConfig(t *testing.T) {
	base := "[logging]\ndirectory = \"/tmp/x\"\n[deploy.s]\ncommand = \"/bin/true\"\n"
	git := "path = \"/hooks/s\"\nrepository = \"a/b\"\nsecret_env = \"S\"\n"
	cases := map[string]string{
		"":                     "path must start with /",
		"schedule = \"* * *\"": "schedule:",
		"schedule = \"@daily\"\nprovider = \"github\"":                                                "needs a path",
		git + "health_url = \"ftp://x\"":                                                              "health_url must be",
		git + "cloudflare_zone_id = \"z\"":                                                            "go together",
		git + "cloudflare_zone_id = \"z\"\ncloudflare_purge = [\"everything\", \"https://a/\"]":       "goes alone",
		git + "cloudflare_zone_id = \"z\"\ncloudflare_purge = [\"/path\"]":                            "is not",
		git + "cloudflare_zone_id = \"z\"\ncloudflare_purge = [\"everything\"]":                       "needs [cloudflare] api_token_env",
		"path = \"/hooks/s\"\nsecret_env = \"S\"\nprovider = \"generic\"\nrollback_on_failure = true": "needs a git provider",
		"path = \"/hooks/s\"\nsecret_env = \"S\"\nprovider = \"stripe\"\nbranch = \"main\"":           "branch does not apply",
		git + "events = [\"x\"]":                                                                      "events is only for",
		"path = \"/metrics\"\nrepository = \"a/b\"\nsecret_env = \"S\"":                               "is reserved",
	}
	for extra, want := range cases {
		path := filepath.Join(t.TempDir(), "c.toml")
		_ = os.WriteFile(path, []byte(base+extra+"\n"), 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", extra, err, want)
		}
	}
	path := filepath.Join(t.TempDir(), "c.toml")
	_ = os.WriteFile(path, []byte(base+"path = \"/hooks/s\"\nsecret_env = \"S\"\nprovider = \"paddle\"\n"), 0o600)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Deploy["s"]; !d.queueAll || !d.payloadFile || d.MaxSkew.Duration != defaultMaxSkew {
		t.Fatalf("payment defaults %+v", d)
	}
	b, _ := json.Marshal(State{NextRun: new(time.Time)})
	if !strings.Contains(string(b), "next_run") {
		t.Error("next_run not in JSON")
	}
}
