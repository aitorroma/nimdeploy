package main

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metrics are exposed on GET /metrics in the Prometheus text format.
// Counters count since the service started; Prometheus handles the resets.
type metrics struct {
	mu          sync.Mutex
	started     time.Time
	deploys     map[[2]string]uint64 // deploy, status
	durCount    map[string]uint64
	durSum      map[string]float64
	durBuckets  map[string][]uint64
	webhooks    map[[2]string]uint64 // deploy, HTTP code
	lastSuccess map[string]time.Time
	lastFailure map[string]time.Time
	waitCount   map[string]uint64
	waitSum     map[string]float64
	waitBuckets map[string][]uint64
	phases      map[[2]string][2]float64 // deploy, phase → count, seconds
	ansibleRuns map[[2]string]uint64     // deploy, host outcome → hosts
	otelDropped uint64
}

// Waiting in the queue: from nothing to the length of a deploy.
var waitBuckets = []float64{0.1, 1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}

// Deploys take seconds to an hour.
var durationBuckets = []float64{5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600}

func newMetrics() *metrics {
	return &metrics{
		started:     time.Now(),
		deploys:     map[[2]string]uint64{},
		durCount:    map[string]uint64{},
		durSum:      map[string]float64{},
		durBuckets:  map[string][]uint64{},
		webhooks:    map[[2]string]uint64{},
		lastSuccess: map[string]time.Time{},
		lastFailure: map[string]time.Time{},
		waitCount:   map[string]uint64{},
		waitSum:     map[string]float64{},
		waitBuckets: map[string][]uint64{},
		phases:      map[[2]string][2]float64{},
		ansibleRuns: map[[2]string]uint64{},
	}
}

func (m *metrics) queueWait(name string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	secs := max(d.Seconds(), 0)
	m.waitCount[name]++
	m.waitSum[name] += secs
	b := m.waitBuckets[name]
	if b == nil {
		b = make([]uint64, len(waitBuckets))
		m.waitBuckets[name] = b
	}
	for i, le := range waitBuckets {
		if secs <= le {
			b[i]++
		}
	}
}

// phase records how long one part of a deploy took (command, health, a hook...).
func (m *metrics) phase(name, phase string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{name, phase}
	v := m.phases[k]
	m.phases[k] = [2]float64{v[0] + 1, v[1] + d.Seconds()}
}

func (m *metrics) ansible(name string, s *AnsibleSummary) {
	if s == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for outcome, n := range map[string]int{"ok": s.HostsOk, "changed": s.HostsChanged, "unreachable": s.HostsUnreachable, "failed": s.HostsFailed} {
		m.ansibleRuns[[2]string{name, outcome}] += uint64(n)
	}
}

func (m *metrics) droppedSpans(n int) {
	m.mu.Lock()
	m.otelDropped += uint64(n)
	m.mu.Unlock()
}

func (m *metrics) finished(name, status string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deploys[[2]string{name, status}]++
	switch status {
	case StatusSuccess:
		m.lastSuccess[name] = time.Now()
	case StatusFailed, StatusInterrupted:
		m.lastFailure[name] = time.Now()
	}
	if status == StatusSkipped {
		return // nothing ran
	}
	secs := d.Seconds()
	m.durCount[name]++
	m.durSum[name] += secs
	b := m.durBuckets[name]
	if b == nil {
		b = make([]uint64, len(durationBuckets))
		m.durBuckets[name] = b
	}
	for i, le := range durationBuckets {
		if secs <= le {
			b[i]++
		}
	}
}

func (m *metrics) webhook(name string, code int) {
	m.mu.Lock()
	m.webhooks[[2]string{name, strconv.Itoa(code)}]++
	m.mu.Unlock()
}

type codeRecorder struct {
	http.ResponseWriter
	code int
}

func (c *codeRecorder) WriteHeader(code int) {
	c.code = code
	c.ResponseWriter.WriteHeader(code)
}

// countWebhook counts a hook's responses by HTTP code.
func (s *Server) countWebhook(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &codeRecorder{ResponseWriter: w, code: http.StatusOK}
		next(rec, r)
		s.runner.metrics.webhook(name, rec.code)
	}
}

// deployGauges is what the runner knows right now about one deploy.
type deployGauges struct {
	running, queued int
	state           State
}

func (r *Runner) gauges(names []string) map[string]deployGauges {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]deployGauges{}
	for _, name := range names {
		g := deployGauges{queued: r.pendingCountLocked(name), state: State{Deploy: name, Status: StatusNever}}
		for lane, n := range r.running {
			if lane == name || strings.HasPrefix(lane, name+laneSep) {
				g.running += n
			}
		}
		if st := r.states[name]; st != nil {
			g.state = copyState(st)
		}
		if at, ok := r.nextRun[name]; ok {
			g.state.NextRun = &at
		}
		if d, ok := r.cfg.Deploy[name]; ok {
			g.state.Labels = copyLabels(d.labels)
		}
		out[name] = g
	}
	return out
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(renderMetrics(s.cfg, s.runner)))
}

// renderMetrics is the /metrics page (also what the OTLP exporter sends).
func renderMetrics(cfg *Config, runner *Runner) string {
	names := cfg.DeployNames()
	g := runner.gauges(names)
	m := runner.metrics
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	family := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	ts := func(t time.Time) string { return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64) }

	family("nimdeploy_build_info", "gauge", "Version of nimdeploy, always 1.")
	fmt.Fprintf(&b, "nimdeploy_build_info{version=%q,goversion=%q} 1\n", version, runtime.Version())
	family("nimdeploy_start_time_seconds", "gauge", "When the service started (Unix time).")
	fmt.Fprintf(&b, "nimdeploy_start_time_seconds %s\n", ts(m.started))

	family("nimdeploy_deploy_info", "gauge", "Labels of each deploy (client, environment...), always 1: join it with the other metrics on deploy.")
	for _, name := range names {
		labels := g[name].state.Labels
		keys := sortedKeys(labels)
		parts := []string{fmt.Sprintf("deploy=%q", name)}
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
		}
		fmt.Fprintf(&b, "nimdeploy_deploy_info{%s} 1\n", strings.Join(parts, ","))
	}

	family("nimdeploy_deploys_total", "counter", "Finished deploys by result since the service started.")
	for _, name := range names {
		for _, status := range []string{StatusSuccess, StatusFailed, StatusSkipped, StatusInterrupted} {
			fmt.Fprintf(&b, "nimdeploy_deploys_total{deploy=%q,status=%q} %d\n", name, status, m.deploys[[2]string{name, status}])
		}
	}

	family("nimdeploy_deploy_duration_seconds", "histogram", "Duration of deploys that ran (not skipped).")
	for _, name := range names {
		buckets := m.durBuckets[name]
		for i, le := range durationBuckets {
			var n uint64
			if buckets != nil {
				n = buckets[i]
			}
			fmt.Fprintf(&b, "nimdeploy_deploy_duration_seconds_bucket{deploy=%q,le=%q} %d\n", name, strconv.FormatFloat(le, 'f', -1, 64), n)
		}
		fmt.Fprintf(&b, "nimdeploy_deploy_duration_seconds_bucket{deploy=%q,le=\"+Inf\"} %d\n", name, m.durCount[name])
		fmt.Fprintf(&b, "nimdeploy_deploy_duration_seconds_sum{deploy=%q} %s\n", name, strconv.FormatFloat(m.durSum[name], 'f', 3, 64))
		fmt.Fprintf(&b, "nimdeploy_deploy_duration_seconds_count{deploy=%q} %d\n", name, m.durCount[name])
	}

	family("nimdeploy_deploy_running", "gauge", "Runs in progress (or waiting for CI).")
	for _, name := range names {
		fmt.Fprintf(&b, "nimdeploy_deploy_running{deploy=%q} %d\n", name, g[name].running)
	}
	family("nimdeploy_deploy_queued", "gauge", "Runs waiting for their turn.")
	for _, name := range names {
		fmt.Fprintf(&b, "nimdeploy_deploy_queued{deploy=%q} %d\n", name, g[name].queued)
	}

	family("nimdeploy_deploy_last_success", "gauge", "1 if the last finished deploy succeeded, 0 if it failed or was interrupted (absent if never run).")
	for _, name := range names {
		switch g[name].state.Status {
		case StatusSuccess:
			fmt.Fprintf(&b, "nimdeploy_deploy_last_success{deploy=%q} 1\n", name)
		case StatusFailed, StatusInterrupted:
			fmt.Fprintf(&b, "nimdeploy_deploy_last_success{deploy=%q} 0\n", name)
		}
	}
	family("nimdeploy_deploy_last_finished_timestamp_seconds", "gauge", "When the last deploy finished (Unix time).")
	for _, name := range names {
		if st := g[name].state; st.FinishedAt != nil {
			fmt.Fprintf(&b, "nimdeploy_deploy_last_finished_timestamp_seconds{deploy=%q} %s\n", name, ts(*st.FinishedAt))
		}
	}
	lastOf := func(metric, help string, seen map[string]time.Time, status string) {
		family(metric, "gauge", help)
		for _, name := range names {
			at, ok := seen[name]
			if st := g[name].state; !ok && st.Status == status && st.FinishedAt != nil {
				at, ok = *st.FinishedAt, true // from before this start
			}
			if ok {
				fmt.Fprintf(&b, "%s{deploy=%q} %s\n", metric, name, ts(at))
			}
		}
	}
	lastOf("nimdeploy_deploy_last_success_timestamp_seconds", "When a deploy last succeeded (Unix time).", m.lastSuccess, StatusSuccess)
	lastOf("nimdeploy_deploy_last_failure_timestamp_seconds", "When a deploy last failed (Unix time).", m.lastFailure, StatusFailed)

	family("nimdeploy_deploy_next_run_timestamp_seconds", "gauge", "Next scheduled run (Unix time), for deploys with a schedule.")
	for _, name := range names {
		if st := g[name].state; st.NextRun != nil {
			fmt.Fprintf(&b, "nimdeploy_deploy_next_run_timestamp_seconds{deploy=%q} %s\n", name, ts(*st.NextRun))
		}
	}

	family("nimdeploy_webhook_requests_total", "counter", "Webhook requests by deploy and HTTP code (202 accepted, 200 ignored/duplicate/ping, 401 bad signature, 400 invalid, 409/503 busy).")
	keys := make([][2]string, 0, len(m.webhooks))
	for k := range m.webhooks {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for _, k := range keys {
		fmt.Fprintf(&b, "nimdeploy_webhook_requests_total{deploy=%q,code=%q} %d\n", k[0], k[1], m.webhooks[k])
	}

	family("nimdeploy_queue_wait_seconds", "histogram", "Time from a request to its run starting (0 when it started at once).")
	for _, name := range names {
		buckets := m.waitBuckets[name]
		for i, le := range waitBuckets {
			var n uint64
			if buckets != nil {
				n = buckets[i]
			}
			fmt.Fprintf(&b, "nimdeploy_queue_wait_seconds_bucket{deploy=%q,le=%q} %d\n", name, strconv.FormatFloat(le, 'f', -1, 64), n)
		}
		fmt.Fprintf(&b, "nimdeploy_queue_wait_seconds_bucket{deploy=%q,le=\"+Inf\"} %d\n", name, m.waitCount[name])
		fmt.Fprintf(&b, "nimdeploy_queue_wait_seconds_sum{deploy=%q} %s\n", name, strconv.FormatFloat(m.waitSum[name], 'f', 3, 64))
		fmt.Fprintf(&b, "nimdeploy_queue_wait_seconds_count{deploy=%q} %d\n", name, m.waitCount[name])
	}

	family("nimdeploy_phase_duration_seconds", "summary", "Time spent in each phase of a deploy: command, health, before, after_success, after_failure, cloudflare, ci.")
	phaseKeys := make([][2]string, 0, len(m.phases))
	for k := range m.phases {
		phaseKeys = append(phaseKeys, k)
	}
	sort.Slice(phaseKeys, func(i, j int) bool {
		return phaseKeys[i][0]+"\x00"+phaseKeys[i][1] < phaseKeys[j][0]+"\x00"+phaseKeys[j][1]
	})
	for _, k := range phaseKeys {
		v := m.phases[k]
		fmt.Fprintf(&b, "nimdeploy_phase_duration_seconds_sum{deploy=%q,phase=%q} %s\n", k[0], k[1], strconv.FormatFloat(v[1], 'f', 3, 64))
		fmt.Fprintf(&b, "nimdeploy_phase_duration_seconds_count{deploy=%q,phase=%q} %d\n", k[0], k[1], uint64(v[0]))
	}

	family("nimdeploy_ansible_hosts_total", "counter", "Hosts in Ansible runs by outcome: ok, changed, unreachable, failed.")
	for _, name := range names {
		if cfg.Deploy[name].Ansible == nil {
			continue
		}
		for _, outcome := range []string{"ok", "changed", "unreachable", "failed"} {
			fmt.Fprintf(&b, "nimdeploy_ansible_hosts_total{deploy=%q,result=%q} %d\n", name, outcome, m.ansibleRuns[[2]string{name, outcome}])
		}
	}
	family("nimdeploy_ansible_last_run_hosts", "gauge", "Hosts of the last Ansible run by outcome.")
	for _, name := range names {
		if s := g[name].state.Ansible; s != nil {
			fmt.Fprintf(&b, "nimdeploy_ansible_last_run_hosts{deploy=%q,result=\"ok\"} %d\n", name, s.HostsOk)
			fmt.Fprintf(&b, "nimdeploy_ansible_last_run_hosts{deploy=%q,result=\"changed\"} %d\n", name, s.HostsChanged)
			fmt.Fprintf(&b, "nimdeploy_ansible_last_run_hosts{deploy=%q,result=\"unreachable\"} %d\n", name, s.HostsUnreachable)
			fmt.Fprintf(&b, "nimdeploy_ansible_last_run_hosts{deploy=%q,result=\"failed\"} %d\n", name, s.HostsFailed)
		}
	}
	if cfg.OTel.Endpoint != "" {
		family("nimdeploy_otel_dropped_total", "counter", "Spans and log records dropped because the OTLP endpoint could not keep up.")
		fmt.Fprintf(&b, "nimdeploy_otel_dropped_total %d\n", m.otelDropped)
	}
	return b.String()
}
