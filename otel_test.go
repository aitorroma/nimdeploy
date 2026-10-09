package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// otlpReceiver records what an exporter posts, by path.
type otlpReceiver struct {
	mu   sync.Mutex
	got  map[string][]map[string]any
	srv  *httptest.Server
	code int
}

func newOTLPReceiver(t *testing.T) *otlpReceiver {
	rcv := &otlpReceiver{got: map[string][]map[string]any{}, code: 200}
	rcv.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if r.Header.Get("Content-Type") != "application/json" || json.Unmarshal(b, &m) != nil || r.Header.Get("X-Api-Key") != "k1" {
			w.WriteHeader(400)
			return
		}
		rcv.mu.Lock()
		rcv.got[r.URL.Path] = append(rcv.got[r.URL.Path], m)
		code := rcv.code
		rcv.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(rcv.srv.Close)
	return rcv
}

type gotSpan struct {
	TraceID, SpanID, Parent, Name string
	Kind                          int
	Attrs                         map[string]string
	Status                        int
}

func (rcv *otlpReceiver) spans() []gotSpan {
	rcv.mu.Lock()
	defer rcv.mu.Unlock()
	var out []gotSpan
	for _, payload := range rcv.got["/v1/traces"] {
		for _, rs := range payload["resourceSpans"].([]any) {
			for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
				for _, s := range ss.(map[string]any)["spans"].([]any) {
					m := s.(map[string]any)
					g := gotSpan{TraceID: m["traceId"].(string), SpanID: m["spanId"].(string), Name: m["name"].(string),
						Kind: int(m["kind"].(float64)), Attrs: map[string]string{}}
					if p, ok := m["parentSpanId"].(string); ok {
						g.Parent = p
					}
					if st, ok := m["status"].(map[string]any); ok {
						g.Status = int(st["code"].(float64))
					}
					for _, a := range m["attributes"].([]any) {
						am := a.(map[string]any)
						for _, v := range am["value"].(map[string]any) {
							g.Attrs[am["key"].(string)] = toStr(v)
						}
					}
					out = append(out, g)
				}
			}
		}
	}
	return out
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func findSpan(spans []gotSpan, name string) *gotSpan {
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

func TestOTelTraces(t *testing.T) {
	rcv := newOTLPReceiver(t)
	t.Setenv("TEST_OTEL_HEADERS", "X-Api-Key=k1")
	e := newGenericEnv(t, `echo "tp=$TRACEPARENT"; exit 0`, `before = "echo before-hook"`, `[labels]
environment = "stage"
[otel]
endpoint = "`+rcv.srv.URL+`"
headers_env = "TEST_OTEL_HEADERS"
logs = true
`)
	tr, err := newTracer(e.cfg, e.runner.metrics)
	if err != nil || tr == nil {
		t.Fatal(tr, err)
	}
	e.runner.tracer = tr
	t.Cleanup(tr.Shutdown)
	var serviceOut syncBuffer
	log.SetOutput(&serviceOut)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// An authenticated webhook continues the caller's trace.
	const callerTrace, callerSpan = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	rec := e.send(t, sendOpts{body: `{"x":1}`, delivery: "o1", headers: map[string]string{"traceparent": "00-" + callerTrace + "-" + callerSpan + "-01"}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	e.waitIdleName(t, "svc")
	// A forged traceparent on a request that fails authentication is not adopted.
	e.send(t, sendOpts{body: `{"x":2}`, unsigned: true, headers: map[string]string{"traceparent": "00-" + strings.Repeat("a", 32) + "-" + callerSpan + "-01"}})
	tr.flush()

	spans := rcv.spans()
	server := findSpan(spans, "POST /hooks/svc")
	deploy := findSpan(spans, "deploy svc")
	command := findSpan(spans, "command")
	before := findSpan(spans, "hook before")
	if server == nil || deploy == nil || command == nil || before == nil {
		t.Fatalf("spans %+v", spans)
	}
	if server.TraceID != callerTrace || server.Parent != callerSpan || server.Kind != spanServer || server.Attrs["http.response.status_code"] != "202" {
		t.Errorf("server span %+v", server)
	}
	if deploy.TraceID != callerTrace || deploy.Parent != server.SpanID || deploy.Attrs["nimdeploy.status"] != "success" ||
		deploy.Attrs["nimdeploy.label.environment"] != "stage" || deploy.Status != 1 {
		t.Errorf("deploy span %+v", deploy)
	}
	if command.Parent != deploy.SpanID || before.Parent != deploy.SpanID || command.Attrs["process.exit.code"] != "0" {
		t.Errorf("children: command %+v before %+v", command, before)
	}
	if out := serviceOut.String(); !strings.Contains(out, "trace_id="+callerTrace+" span_id="+deploy.SpanID) ||
		!strings.Contains(out, "trace_id="+callerTrace+" span_id="+server.SpanID) {
		t.Errorf("service log without the deploy's and the request's span ids:\n%s", out)
	}
	if out := readLatest(t, e, "svc"); !strings.Contains(out, "tp=00-"+callerTrace+"-"+command.SpanID+"-01") {
		t.Errorf("TRACEPARENT for the command:\n%s", out)
	}
	for _, s := range spans {
		if s.TraceID == strings.Repeat("a", 32) {
			t.Errorf("forged traceparent adopted: %+v", s)
		}
	}
	var refused *gotSpan
	for i := range spans {
		if spans[i].Name == "POST /hooks/svc" && spans[i].Attrs["http.response.status_code"] == "401" {
			refused = &spans[i]
		}
	}
	if refused == nil || refused.Parent != "" {
		t.Errorf("refused request span %+v", refused)
	}

	// Logs: records go to /v1/logs with their trace.
	setLogExport(tr.enqueueLog)
	defer setLogExport(nil)
	var sink strings.Builder
	old := serviceLog.out
	serviceLog.out = &sink
	log.SetOutput(serviceLog)
	defer func() { serviceLog.out = old; log.SetOutput(os.Stderr); log.SetFlags(log.LstdFlags) }()
	log.Printf("deploy=svc status=failed trace_id=%s error=%q", callerTrace, "boom")
	tr.flush()
	rcv.mu.Lock()
	logsPayload := rcv.got["/v1/logs"]
	rcv.mu.Unlock()
	if len(logsPayload) == 0 {
		t.Fatal("no logs exported")
	}
	b, _ := json.Marshal(logsPayload)
	if !strings.Contains(string(b), `"traceId":"`+callerTrace+`"`) || !strings.Contains(string(b), `"severityText":"ERROR"`) {
		t.Errorf("logs %s", b)
	}
}

func TestOTelMetricsPayload(t *testing.T) {
	text := `# HELP nimdeploy_deploys_total Finished deploys.
# TYPE nimdeploy_deploys_total counter
nimdeploy_deploys_total{deploy="web",status="success"} 3
# HELP nimdeploy_deploy_duration_seconds Duration.
# TYPE nimdeploy_deploy_duration_seconds histogram
nimdeploy_deploy_duration_seconds_bucket{deploy="web",le="5"} 1
nimdeploy_deploy_duration_seconds_bucket{deploy="web",le="15"} 2
nimdeploy_deploy_duration_seconds_bucket{deploy="web",le="+Inf"} 3
nimdeploy_deploy_duration_seconds_sum{deploy="web"} 42.5
nimdeploy_deploy_duration_seconds_count{deploy="web"} 3
# TYPE nimdeploy_phase_duration_seconds summary
nimdeploy_phase_duration_seconds_sum{deploy="web",phase="command"} 10
nimdeploy_phase_duration_seconds_count{deploy="web",phase="command"} 2
# TYPE nimdeploy_deploy_info gauge
nimdeploy_deploy_info{deploy="web",client="Acme \"Q\""} 1
`
	tr := &tracer{metrics: newMetrics()}
	b, _ := json.Marshal(tr.metricsPayload(text))
	var p struct {
		ResourceMetrics []struct {
			ScopeMetrics []struct {
				Metrics []map[string]json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	ms := map[string]map[string]json.RawMessage{}
	for _, m := range p.ResourceMetrics[0].ScopeMetrics[0].Metrics {
		var name string
		_ = json.Unmarshal(m["name"], &name)
		ms[name] = m
	}
	if len(ms) != 4 || ms["nimdeploy_deploys_total"]["sum"] == nil || ms["nimdeploy_deploy_info"]["gauge"] == nil ||
		ms["nimdeploy_phase_duration_seconds"]["summary"] == nil {
		t.Fatalf("metrics %s", b)
	}
	var h struct {
		DataPoints []struct {
			Count          string
			Sum            float64
			BucketCounts   []string
			ExplicitBounds []float64
		}
	}
	_ = json.Unmarshal(ms["nimdeploy_deploy_duration_seconds"]["histogram"], &h)
	dp := h.DataPoints[0]
	if dp.Count != "3" || dp.Sum != 42.5 || strings.Join(dp.BucketCounts, ",") != "1,1,1" || len(dp.ExplicitBounds) != 2 {
		t.Errorf("histogram %+v", dp)
	}
	if !strings.Contains(string(b), `Acme \"Q\"`) {
		t.Errorf("escaped label value: %s", b)
	}
}

func TestTraceparent(t *testing.T) {
	c, ok := parseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok || !c.Sampled || c.traceparent() != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Errorf("%+v", c)
	}
	for _, bad := range []string{"", "00-0000000000000000000000000000000000-00f067aa0ba902b7-01", "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", "00-zz-00f067aa0ba902b7-01"} {
		if _, ok := parseTraceparent(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	h, err := parseOTelHeaders("Authorization=Basic%20abc, x-team = ops")
	if err != nil || h["Authorization"] != "Basic abc" || h["x-team"] != "ops" {
		t.Errorf("headers %v %v", h, err)
	}
	var nilSpan *span
	nilSpan.set(spanAttr{"a", "b"})
	nilSpan.End()
	if nilSpan.child("x", spanInternal) != nil || nilSpan.TraceID() != "" {
		t.Error("nil span")
	}
}

// syncBuffer is a bytes.Buffer safe for the logger and the test to share.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
