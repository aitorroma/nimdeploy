package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenTelemetry export over OTLP/HTTP with the JSON encoding, without the
// SDK (nimdeploy keeps a single dependency): traces of every webhook and
// deploy, the service log, and the /metrics values. Exporting never blocks a
// deploy: when the collector can't keep up, records are dropped and counted
// (nimdeploy_otel_dropped_total).

// OTelConfig is the [otel] section.
type OTelConfig struct {
	// Endpoint is the OTLP/HTTP base URL, e.g. http://otel-collector:4318;
	// /v1/traces, /v1/logs and /v1/metrics are added.
	Endpoint string `toml:"endpoint"`
	// Traces is on by default with an endpoint; Logs and Metrics are opt-in.
	Traces          *bool    `toml:"traces"`
	Logs            bool     `toml:"logs"`
	Metrics         bool     `toml:"metrics"`
	MetricsInterval Duration `toml:"metrics_interval"`
	// HeadersEnv names an env var with "key=value,key2=value2" headers (an
	// API key for a hosted backend), like OTEL_EXPORTER_OTLP_HEADERS.
	HeadersEnv  string   `toml:"headers_env"`
	ServiceName string   `toml:"service_name"`
	SampleRatio *float64 `toml:"sample_ratio"`
	// Propagate continues a trace from an authenticated request's
	// traceparent header (default true). Commands always get TRACEPARENT.
	Propagate *bool    `toml:"propagate"`
	Timeout   Duration `toml:"timeout"`
	// TLS towards the collector: a private CA and a client certificate (mTLS).
	CAFile   string `toml:"ca_file"`
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`

	headers   map[string]string
	traces    bool
	propagate bool
	ratio     float64
}

func (o *OTelConfig) validate() error {
	if o.Endpoint == "" {
		if o.Logs || o.Metrics || o.Traces != nil || o.HeadersEnv != "" || o.CAFile != "" || o.CertFile != "" {
			return errors.New("otel: settings need endpoint")
		}
		return nil
	}
	u, err := url.Parse(o.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("otel.endpoint must be an http(s) URL, e.g. http://otel-collector:4318")
	}
	o.Endpoint = strings.TrimRight(o.Endpoint, "/")
	o.traces = o.Traces == nil || *o.Traces
	o.propagate = o.Propagate == nil || *o.Propagate
	o.ratio = 1
	if o.SampleRatio != nil {
		o.ratio = *o.SampleRatio
	}
	if o.ratio < 0 || o.ratio > 1 {
		return errors.New("otel.sample_ratio must be between 0 and 1")
	}
	if o.ServiceName == "" {
		o.ServiceName = "nimdeploy"
	}
	if o.Timeout.Duration == 0 {
		o.Timeout.Duration = 10 * time.Second
	}
	if o.MetricsInterval.Duration == 0 {
		o.MetricsInterval.Duration = time.Minute
	}
	if o.MetricsInterval.Duration < 5*time.Second {
		return errors.New("otel.metrics_interval must be at least 5s")
	}
	if (o.CertFile == "") != (o.KeyFile == "") {
		return errors.New("otel.cert_file and otel.key_file go together")
	}
	return nil
}

// parseOTelHeaders reads "k=v,k2=v2" (values may be URL-encoded).
func parseOTelHeaders(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%q is not key=value", part)
		}
		if dv, err := url.QueryUnescape(strings.TrimSpace(v)); err == nil {
			v = dv
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}

// --- trace context -------------------------------------------------------------------

type traceContext struct {
	TraceID [16]byte
	SpanID  [8]byte
	Sampled bool
}

func (c traceContext) valid() bool { return c.TraceID != [16]byte{} && c.SpanID != [8]byte{} }

func (c traceContext) traceparent() string {
	flags := "00"
	if c.Sampled {
		flags = "01"
	}
	return "00-" + hex.EncodeToString(c.TraceID[:]) + "-" + hex.EncodeToString(c.SpanID[:]) + "-" + flags
}

// parseTraceparent reads a W3C traceparent header.
func parseTraceparent(h string) (traceContext, bool) {
	var c traceContext
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || parts[0] == "ff" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return c, false
	}
	if parts[0] == "00" && len(parts) != 4 {
		return c, false
	}
	tid, err1 := hex.DecodeString(parts[1])
	sid, err2 := hex.DecodeString(parts[2])
	flags, err3 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return c, false
	}
	copy(c.TraceID[:], tid)
	copy(c.SpanID[:], sid)
	c.Sampled = flags[0]&1 == 1
	return c, c.valid()
}

func randomID(b []byte) {
	for {
		_, _ = rand.Read(b)
		for _, x := range b {
			if x != 0 {
				return
			}
		}
	}
}

// --- spans -----------------------------------------------------------------------------------

const (
	spanInternal = 1
	spanServer   = 2
	spanClient   = 3
)

type spanAttr struct {
	Key   string
	Value any // string, int64, float64, bool
}

type spanEvent struct {
	Time  time.Time
	Name  string
	Attrs []spanAttr
}

// span is one operation. A nil *span is valid and does nothing, so code can
// trace unconditionally.
type span struct {
	tr     *tracer
	mu     sync.Mutex
	ctx    traceContext
	parent [8]byte
	name   string
	kind   int
	start  time.Time
	end    time.Time
	attrs  []spanAttr
	events []spanEvent
	status int // 0 unset, 1 ok, 2 error
	msg    string
	ended  bool
	remote traceContext // a traceparent to adopt once the request authenticates
}

func (s *span) context() traceContext {
	if s == nil {
		return traceContext{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx
}

// TraceID and SpanID are for log lines; empty without tracing.
func (s *span) TraceID() string {
	if c := s.context(); c.valid() {
		return hex.EncodeToString(c.TraceID[:])
	}
	return ""
}

func (s *span) SpanID() string {
	if c := s.context(); c.valid() {
		return hex.EncodeToString(c.SpanID[:])
	}
	return ""
}

func (s *span) traceparent() string {
	if c := s.context(); c.valid() {
		return c.traceparent()
	}
	return ""
}

func (s *span) set(attrs ...spanAttr) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.attrs = append(s.attrs, attrs...)
	s.mu.Unlock()
}

func (s *span) event(name string, attrs ...spanAttr) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.events = append(s.events, spanEvent{Time: time.Now(), Name: name, Attrs: attrs})
	s.mu.Unlock()
}

func (s *span) fail(msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.status, s.msg = 2, msg
	s.mu.Unlock()
}

func (s *span) ok() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.status == 0 {
		s.status = 1
	}
	s.mu.Unlock()
}

// child starts a span under this one.
func (s *span) child(name string, kind int) *span {
	if s == nil {
		return nil
	}
	return s.tr.start(name, kind, s.context(), time.Now())
}

// childAt starts a child with a given start time (e.g. time spent queued).
func (s *span) childAt(name string, start time.Time) *span {
	if s == nil {
		return nil
	}
	return s.tr.start(name, spanInternal, s.context(), start)
}

func (s *span) endAt(t time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended, s.end = true, t
	sampled := s.ctx.Sampled
	s.mu.Unlock()
	if sampled {
		s.tr.enqueueSpan(s)
	}
}

func (s *span) End() { s.endAt(time.Now()) }

// adoptRemote continues the caller's trace (its traceparent header) once the
// request proved who it is; before that, a forged header can't attach
// anything to someone else's trace.
func (s *span) adoptRemote() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.remote.valid() || s.ended {
		return
	}
	s.ctx.TraceID, s.parent, s.ctx.Sampled = s.remote.TraceID, s.remote.SpanID, s.remote.Sampled
	s.remote = traceContext{}
}

type spanKey struct{}

func withSpan(ctx context.Context, s *span) context.Context {
	return context.WithValue(ctx, spanKey{}, s)
}

func spanFrom(ctx context.Context) *span {
	s, _ := ctx.Value(spanKey{}).(*span)
	return s
}

// --- tracer and exporter ------------------------------------------------------------------------

const (
	otelQueueMax  = 4096
	otelBatchMax  = 512
	otelFlushTick = 2 * time.Second
)

type tracer struct {
	cfg      OTelConfig
	client   *http.Client
	resource []spanAttr
	metrics  *metrics

	mu            sync.Mutex
	spans         []*span
	logs          []logRecord
	failing       bool
	metricsSource func() string

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

// newTracer starts the exporter, or returns nil when [otel] is off.
func newTracer(cfg *Config, m *metrics) (*tracer, error) {
	o := cfg.OTel
	if o.Endpoint == "" {
		return nil, nil
	}
	watchCertExpiry("otel_client", o.CertFile)
	watchCertExpiry("otel_ca", o.CAFile)
	tc, err := clientTLS(o.CAFile, o.CertFile, o.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("otel: %w", err)
	}
	host, _ := os.Hostname()
	res := []spanAttr{{"service.name", o.ServiceName}, {"service.version", version}, {"host.name", host},
		{"telemetry.sdk.name", "nimdeploy"}, {"telemetry.sdk.language", "go"}}
	for _, k := range sortedKeys(cfg.Labels) {
		res = append(res, spanAttr{"nimdeploy.label." + k, cfg.Labels[k]})
		if k == "environment" {
			res = append(res, spanAttr{"deployment.environment.name", cfg.Labels[k]})
		}
	}
	t := &tracer{cfg: o, client: httpClientTLS(o.Timeout.Duration, tc), resource: res, metrics: m,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go t.loop(cfg)
	return t, nil
}

// start creates a span: under parent when it is valid, else a new trace
// (sampled by sample_ratio).
func (t *tracer) start(name string, kind int, parent traceContext, at time.Time) *span {
	if t == nil || !t.cfg.traces {
		return nil
	}
	s := &span{tr: t, name: name, kind: kind, start: at}
	randomID(s.ctx.SpanID[:])
	if parent.valid() {
		s.ctx.TraceID, s.parent, s.ctx.Sampled = parent.TraceID, parent.SpanID, parent.Sampled
	} else {
		randomID(s.ctx.TraceID[:])
		s.ctx.Sampled = t.cfg.ratio >= 1 || mrand.Float64() < t.cfg.ratio
	}
	return s
}

func (t *tracer) enqueueSpan(s *span) {
	t.mu.Lock()
	if len(t.spans) >= otelQueueMax {
		t.mu.Unlock()
		t.metrics.droppedSpans(1)
		return
	}
	t.spans = append(t.spans, s)
	full := len(t.spans) >= otelBatchMax
	t.mu.Unlock()
	if full {
		t.poke()
	}
}

func (t *tracer) enqueueLog(r logRecord) {
	if t == nil || !t.cfg.Logs {
		return
	}
	t.mu.Lock()
	if len(t.logs) >= otelQueueMax {
		t.mu.Unlock()
		t.metrics.droppedSpans(1)
		return
	}
	t.logs = append(t.logs, r)
	t.mu.Unlock()
}

func (t *tracer) poke() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *tracer) loop(cfg *Config) {
	defer close(t.done)
	tick := time.NewTicker(otelFlushTick)
	defer tick.Stop()
	var metricsTick <-chan time.Time
	if t.cfg.Metrics {
		mt := time.NewTicker(t.cfg.MetricsInterval.Duration)
		defer mt.Stop()
		metricsTick = mt.C
	}
	for {
		select {
		case <-t.stop:
			t.flush()
			return
		case <-tick.C:
			t.flush()
		case <-t.wake:
			t.flush()
		case <-metricsTick:
			t.mu.Lock()
			src := t.metricsSource
			t.mu.Unlock()
			if src != nil {
				t.report(t.post("/v1/metrics", t.metricsPayload(src())))
			}
		}
	}
}

// metricsSource renders the current metrics (set by serve).
func (t *tracer) setMetricsSource(f func() string) {
	t.mu.Lock()
	t.metricsSource = f
	t.mu.Unlock()
}

func (t *tracer) flush() {
	t.mu.Lock()
	spans, logs := t.spans, t.logs
	t.spans, t.logs = nil, nil
	t.mu.Unlock()
	for len(spans) > 0 {
		n := min(len(spans), otelBatchMax)
		if err := t.post("/v1/traces", t.tracesPayload(spans[:n])); err != nil {
			t.metrics.droppedSpans(len(spans))
			t.report(err)
			break
		}
		t.report(nil)
		spans = spans[n:]
	}
	for len(logs) > 0 {
		n := min(len(logs), otelBatchMax)
		if err := t.post("/v1/logs", t.logsPayload(logs[:n])); err != nil {
			t.metrics.droppedSpans(len(logs))
			t.report(err)
			break
		}
		logs = logs[n:]
	}
}

// report logs when exporting starts or stops failing, not every attempt.
func (t *tracer) report(err error) {
	t.mu.Lock()
	changed := (err != nil) != t.failing
	t.failing = err != nil
	t.mu.Unlock()
	switch {
	case changed && err != nil:
		log.Printf("otel: cannot export to %s, dropping until it answers: %v", t.cfg.Endpoint, err)
	case changed:
		log.Printf("otel: exporting to %s again", t.cfg.Endpoint)
	}
}

func (t *tracer) post(path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.cfg.Timeout.Duration)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nimdeploy/"+version)
	for k, v := range t.cfg.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return uerr.Err
		}
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Shutdown sends what is waiting.
func (t *tracer) Shutdown() {
	if t == nil {
		return
	}
	close(t.stop)
	select {
	case <-t.done:
	case <-time.After(t.cfg.Timeout.Duration + time.Second):
	}
}

// --- OTLP JSON -----------------------------------------------------------------------------------

func otlpValue(v any) map[string]any {
	switch x := v.(type) {
	case string:
		return map[string]any{"stringValue": x}
	case bool:
		return map[string]any{"boolValue": x}
	case int:
		return map[string]any{"intValue": strconv.Itoa(x)}
	case int64:
		return map[string]any{"intValue": strconv.FormatInt(x, 10)}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return map[string]any{"stringValue": strconv.FormatFloat(x, 'f', -1, 64)}
		}
		return map[string]any{"doubleValue": x}
	}
	return map[string]any{"stringValue": fmt.Sprint(v)}
}

func otlpAttrs(attrs []spanAttr) []map[string]any {
	out := make([]map[string]any, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, map[string]any{"key": a.Key, "value": otlpValue(a.Value)})
	}
	return out
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func (t *tracer) scope() map[string]any {
	return map[string]any{"name": "nimdeploy", "version": version}
}

func (t *tracer) tracesPayload(spans []*span) map[string]any {
	out := make([]map[string]any, 0, len(spans))
	for _, s := range spans {
		s.mu.Lock()
		m := map[string]any{
			"traceId": hex.EncodeToString(s.ctx.TraceID[:]), "spanId": hex.EncodeToString(s.ctx.SpanID[:]),
			"name": s.name, "kind": s.kind, "startTimeUnixNano": nanos(s.start), "endTimeUnixNano": nanos(s.end),
			"attributes": otlpAttrs(s.attrs),
		}
		if s.parent != [8]byte{} {
			m["parentSpanId"] = hex.EncodeToString(s.parent[:])
		}
		if len(s.events) > 0 {
			evs := make([]map[string]any, 0, len(s.events))
			for _, e := range s.events {
				evs = append(evs, map[string]any{"timeUnixNano": nanos(e.Time), "name": e.Name, "attributes": otlpAttrs(e.Attrs)})
			}
			m["events"] = evs
		}
		if s.status != 0 {
			st := map[string]any{"code": s.status}
			if s.msg != "" {
				st["message"] = s.msg
			}
			m["status"] = st
		}
		s.mu.Unlock()
		out = append(out, m)
	}
	return map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": otlpAttrs(t.resource)},
		"scopeSpans": []any{map[string]any{"scope": t.scope(), "spans": out}},
	}}}
}

var severities = map[string]int{"debug": 5, "info": 9, "warn": 13, "error": 17}

func (t *tracer) logsPayload(recs []logRecord) map[string]any {
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		var attrs []spanAttr
		var text []string
		if r.Msg != "" {
			text = append(text, r.Msg)
		}
		var traceID, spanID string
		for _, f := range r.Fields {
			switch f[0] {
			case "trace_id":
				traceID = f[1]
			case "span_id":
				spanID = f[1]
			default:
				attrs = append(attrs, spanAttr{f[0], f[1]})
			}
			text = append(text, f[0]+"="+f[1])
		}
		m := map[string]any{
			"timeUnixNano": nanos(r.Time), "observedTimeUnixNano": nanos(r.Time),
			"severityNumber": severities[r.Level], "severityText": strings.ToUpper(r.Level),
			"body": otlpValue(strings.Join(text, " ")), "attributes": otlpAttrs(attrs),
		}
		if len(traceID) == 32 {
			m["traceId"] = traceID
		}
		if len(spanID) == 16 {
			m["spanId"] = spanID
		}
		out = append(out, m)
	}
	return map[string]any{"resourceLogs": []any{map[string]any{
		"resource":  map[string]any{"attributes": otlpAttrs(t.resource)},
		"scopeLogs": []any{map[string]any{"scope": t.scope(), "logRecords": out}},
	}}}
}

// --- metrics: the /metrics text as OTLP --------------------------------------------------------

type promSample struct {
	name   string
	labels [][2]string
	value  float64
}

// parsePromText reads the Prometheus text format nimdeploy writes.
func parsePromText(text string) (types map[string]string, help map[string]string, samples []promSample) {
	types, help = map[string]string{}, map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			if len(f) == 4 {
				types[f[2]] = f[3]
			}
		case strings.HasPrefix(line, "# HELP "):
			if name, h, ok := strings.Cut(strings.TrimPrefix(line, "# HELP "), " "); ok {
				help[name] = h
			}
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			if s, ok := parsePromSample(line); ok {
				samples = append(samples, s)
			}
		}
	}
	return types, help, samples
}

func parsePromSample(line string) (promSample, bool) {
	var s promSample
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return s, false
	}
	s.name, line = line[:i], line[i:]
	if strings.HasPrefix(line, "{") {
		line = line[1:]
		for !strings.HasPrefix(line, "}") {
			eq := strings.Index(line, `="`)
			if eq < 0 {
				return s, false
			}
			key := line[:eq]
			rest := line[eq+1:]
			end := 1
			for ; end < len(rest); end++ {
				if rest[end] == '\\' {
					end++
					continue
				}
				if rest[end] == '"' {
					break
				}
			}
			if end >= len(rest) {
				return s, false
			}
			val, err := strconv.Unquote(rest[:end+1])
			if err != nil {
				return s, false
			}
			s.labels = append(s.labels, [2]string{key, val})
			line = strings.TrimPrefix(rest[end+1:], ",")
		}
		line = line[1:]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
	if err != nil {
		return s, false
	}
	s.value = v
	return s, true
}

func labelAttrs(labels [][2]string, skip string) []spanAttr {
	out := make([]spanAttr, 0, len(labels))
	for _, l := range labels {
		if l[0] != skip {
			out = append(out, spanAttr{l[0], l[1]})
		}
	}
	return out
}

func labelKey(labels [][2]string, skip string) string {
	var b strings.Builder
	for _, l := range labels {
		if l[0] != skip {
			b.WriteString(l[0] + "\x00" + l[1] + "\x00")
		}
	}
	return b.String()
}

func (t *tracer) metricsPayload(text string) map[string]any {
	types, help, samples := parsePromText(text)
	now := nanos(time.Now())
	start := nanos(t.metrics.started)
	family := func(name string) (string, string) {
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if base, ok := strings.CutSuffix(name, suffix); ok {
				if typ := types[base]; typ == "histogram" || typ == "summary" {
					return base, typ
				}
			}
		}
		return name, types[name]
	}
	type histPoint struct {
		labels  [][2]string
		bounds  []float64
		cumul   []float64
		sum     float64
		count   float64
		hasSum  bool
		hasCnt  bool
		isHisto bool
	}
	points := map[string][]map[string]any{}
	hists := map[string]map[string]*histPoint{}
	var order []string
	seen := map[string]bool{}
	for _, s := range samples {
		base, typ := family(s.name)
		if !seen[base] {
			seen[base] = true
			order = append(order, base)
		}
		switch typ {
		case "histogram", "summary":
			if hists[base] == nil {
				hists[base] = map[string]*histPoint{}
			}
			k := labelKey(s.labels, "le")
			p := hists[base][k]
			if p == nil {
				p = &histPoint{labels: s.labels, isHisto: typ == "histogram"}
				hists[base][k] = p
			}
			switch {
			case strings.HasSuffix(s.name, "_bucket"):
				for _, l := range s.labels {
					if l[0] == "le" && l[1] != "+Inf" {
						if b, err := strconv.ParseFloat(l[1], 64); err == nil {
							p.bounds = append(p.bounds, b)
							p.cumul = append(p.cumul, s.value)
						}
					}
				}
			case strings.HasSuffix(s.name, "_sum"):
				p.sum, p.hasSum = s.value, true
			case strings.HasSuffix(s.name, "_count"):
				p.count, p.hasCnt = s.value, true
			}
		default:
			points[base] = append(points[base], map[string]any{
				"attributes": otlpAttrs(labelAttrs(s.labels, "")), "startTimeUnixNano": start, "timeUnixNano": now, "asDouble": s.value,
			})
		}
	}
	var list []any
	for _, name := range order {
		m := map[string]any{"name": name, "description": help[name]}
		if strings.HasSuffix(name, "_seconds") {
			m["unit"] = "s"
		}
		switch types[name] {
		case "counter":
			m["sum"] = map[string]any{"aggregationTemporality": 2, "isMonotonic": true, "dataPoints": points[name]}
		case "histogram", "summary":
			var dps []map[string]any
			keys := sortedKeys(hists[name])
			for _, k := range keys {
				p := hists[name][k]
				dp := map[string]any{"attributes": otlpAttrs(labelAttrs(p.labels, "le")), "startTimeUnixNano": start,
					"timeUnixNano": now, "count": strconv.FormatUint(uint64(p.count), 10), "sum": p.sum}
				if p.isHisto {
					idx := make([]int, len(p.bounds))
					for i := range idx {
						idx[i] = i
					}
					sort.Slice(idx, func(a, b int) bool { return p.bounds[idx[a]] < p.bounds[idx[b]] })
					bounds := make([]float64, 0, len(idx))
					counts := make([]string, 0, len(idx)+1)
					prev := 0.0
					for _, i := range idx {
						bounds = append(bounds, p.bounds[i])
						counts = append(counts, strconv.FormatUint(uint64(p.cumul[i]-prev), 10))
						prev = p.cumul[i]
					}
					counts = append(counts, strconv.FormatUint(uint64(max(p.count-prev, 0)), 10))
					dp["explicitBounds"], dp["bucketCounts"] = bounds, counts
				}
				dps = append(dps, dp)
			}
			if types[name] == "histogram" {
				m["histogram"] = map[string]any{"aggregationTemporality": 2, "dataPoints": dps}
			} else {
				m["summary"] = map[string]any{"dataPoints": dps}
			}
		default:
			m["gauge"] = map[string]any{"dataPoints": points[name]}
		}
		list = append(list, m)
	}
	return map[string]any{"resourceMetrics": []any{map[string]any{
		"resource":     map[string]any{"attributes": otlpAttrs(t.resource)},
		"scopeMetrics": []any{map[string]any{"scope": t.scope(), "metrics": list}},
	}}}
}
