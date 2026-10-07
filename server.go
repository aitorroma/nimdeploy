package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	cfg    *Config
	runner *Runner
}

func NewServer(cfg *Config, runner *Runner) *Server {
	return &Server{cfg: cfg, runner: runner}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	for _, name := range s.cfg.DeployNames() {
		d := s.cfg.Deploy[name]
		if d.Path == "" {
			continue // scheduled only
		}
		mux.HandleFunc("POST "+d.Path, s.countWebhook(d.Name, s.handleWebhook(d)))
	}
	mux.HandleFunc("GET /status", s.requireToken(false, s.handleStatusAll))
	mux.HandleFunc("GET /status/{name}", s.requireToken(false, s.handleStatus))
	mux.HandleFunc("GET /history/{name}", s.requireToken(false, s.handleHistory))
	mux.HandleFunc("POST /deploy/{name}", s.requireToken(true, s.handleManualDeploy))
	mux.HandleFunc("POST /rollback/{name}", s.requireToken(true, s.handleRollback))
	mux.HandleFunc("GET /metrics", s.requireToken(false, s.handleMetrics))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	var h http.Handler = mux
	if base := s.cfg.Server.BasePath; base != "" {
		h = http.StripPrefix(base, mux)
	}
	return s.accessLog(h)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// accessLog logs every request except health checks, with the real client IP.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasSuffix(r.URL.Path, "/healthz") && rec.status == http.StatusOK {
			return
		}
		log.Printf("http %s %s status=%d client=%s delivery=%s duration=%s",
			r.Method, r.URL.Path, rec.status, s.clientIP(r), deliveryID(r), formatDuration(time.Since(start)))
	})
}

// deliveryID is the request's delivery ID under any provider's header name.
func deliveryID(r *http.Request) string {
	return firstHeader(r, "X-GitHub-Delivery", "X-Forgejo-Delivery", "X-Gitea-Delivery", "X-Gitlab-Event-UUID",
		"X-Request-UUID", defaultDeliveryHeader, "X-Request-ID")
}

// clientIP returns the address of whoever sent the request. Forwarding
// headers are only believed when the direct peer is a trusted proxy (or the
// unix socket); X-Forwarded-For is walked right to left, skipping proxies.
func (s *Server) clientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if s.cfg.Server.socketPath == "" && !s.trustedProxy(peer) {
		return peer
	}
	if name := s.cfg.Server.ClientIPHeader; name != "" {
		if ip := strings.TrimSpace(r.Header.Get(name)); ip != "" {
			if _, err := netip.ParseAddr(ip); err == nil {
				return ip
			}
		}
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, hop := range strings.Split(v, ",") {
			if hop = strings.TrimSpace(hop); hop != "" {
				hops = append(hops, hop)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if !s.trustedProxy(hops[i]) {
			return hops[i]
		}
	}
	if len(hops) > 0 {
		return hops[0]
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	if peer == "" || peer == "@" {
		return "unix"
	}
	return peer
}

func (s *Server) trustedProxy(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range s.cfg.Server.trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// requireToken checks "Authorization: Bearer <api token>". Without a
// configured token, mandatory endpoints are disabled and the rest are open.
func (s *Server) requireToken(mandatory bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := s.cfg.Server.apiToken
		if token == "" {
			if mandatory {
				writeError(w, http.StatusForbidden, "disabled: set server.api_token_env to enable")
				return
			}
			next(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleWebhook(d *DeployConfig) http.HandlerFunc {
	switch d.Provider {
	case providerGeneric:
		return s.handleGeneric(d)
	case providerWooCommerce:
		return s.handleWooCommerce(d)
	}
	if paymentProvider(d.Provider) {
		return s.handlePayment(d)
	}
	p := providers[d.Provider]
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "cannot read body")
			return
		}
		if !p.verify(r, body, d.secret) {
			log.Printf("deploy=%s rejected: invalid %s signature/token from %s", d.Name, d.Provider, s.clientIP(r))
			writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}

		// Some hosts can send form-encoded "payload=" instead of JSON. Fall
		// back to the raw body so a plain `curl -d '{...}'` also works.
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			if form, err := url.ParseQuery(string(body)); err == nil && form.Get("payload") != "" {
				body = []byte(form.Get("payload"))
			}
		}
		ev, err := p.parse(r, body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON payload")
			return
		}

		switch {
		case ev.Ping:
			writeJSON(w, http.StatusOK, map[string]string{"deploy": d.Name, "status": "pong"})
			return
		case !ev.Push:
			ignore(w, d, ev.Delivery, "event "+ev.Name+" not handled")
			return
		}
		if !strings.EqualFold(ev.Repository, d.Repository) {
			ignore(w, d, ev.Delivery, "repository "+ev.Repository+" does not match")
			return
		}

		var match *pushInfo
		var refs []string
		for i, push := range ev.Pushes {
			refs = append(refs, push.Ref)
			if branch, ok := strings.CutPrefix(push.Ref, "refs/heads/"); ok && branch == d.Branch {
				match = &ev.Pushes[i]
			}
		}
		if match == nil {
			ignore(w, d, ev.Delivery, "ref "+strings.Join(refs, ", ")+" is not branch "+d.Branch)
			return
		}
		if match.Deleted {
			ignore(w, d, ev.Delivery, "branch deleted")
			return
		}
		params, ok := s.payloadRules(w, d, ev.Delivery, body)
		if !ok {
			return
		}

		s.submit(w, d, Trigger{
			Source:     TriggerWebhook,
			Provider:   d.Provider,
			Delivery:   ev.Delivery,
			Repository: ev.Repository,
			Ref:        match.Ref,
			Branch:     d.Branch,
			Commit:     match.Commit,
			Pusher:     ev.Pusher,
			Params:     params,
		})
	}
}

// handleGeneric serves provider = "generic": any authenticated JSON POST,
// filtered by when, with its data reaching the command only as params.
func (s *Server) handleGeneric(d *DeployConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "cannot read body")
			return
		}
		if err := verifyGeneric(d, r, body, time.Now()); err != nil {
			log.Printf("deploy=%s rejected: %v from %s", d.Name, err, s.clientIP(r))
			writeError(w, http.StatusUnauthorized, "invalid signature or token")
			return
		}
		delivery := firstHeader(r, d.DeliveryHeader, "X-Request-ID")
		params, ok := s.payloadRules(w, d, delivery, body)
		if !ok {
			return
		}
		pusher := ""
		if d.pusherPath != nil {
			if doc, err := decodeJSON(body); err == nil {
				if v, found := lookupJSON(doc, d.pusherPath); found {
					if str, ok := scalarString(v); ok && defaultParamMatch.MatchString(str) {
						pusher = truncate(str, 64)
					}
				}
			}
		}
		t := Trigger{
			Source:     TriggerWebhook,
			Provider:   d.Provider,
			Delivery:   delivery,
			Repository: d.Repository,
			Pusher:     pusher,
			Params:     params,
		}
		if d.payloadFile {
			t.Payload = body
		}
		s.submit(w, d, t)
	}
}

// payloadRules applies a deploy's when conditions and extracts its params.
// It answers the request itself (200 ignored, 400 invalid) when it returns false.
func (s *Server) payloadRules(w http.ResponseWriter, d *DeployConfig, delivery string, body []byte) ([]Param, bool) {
	if len(d.when) == 0 && len(d.Params) == 0 {
		return nil, true
	}
	doc, err := decodeJSON(body)
	if err != nil {
		log.Printf("deploy=%s delivery=%s rejected: invalid JSON: %v", d.Name, delivery, err)
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return nil, false
	}
	params, reason, err := applyRules(d, doc)
	switch {
	case err != nil:
		log.Printf("deploy=%s delivery=%s rejected: %v", d.Name, delivery, err)
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, false
	case reason != "":
		ignore(w, d, delivery, reason)
		return nil, false
	}
	return params, true
}

type manualRequest struct {
	Commit string            `json:"commit"`
	User   string            `json:"user"`
	Params map[string]string `json:"params,omitempty"`
	// Payload runs the deploy as if this body had arrived in a webhook
	// (when/params/statuses apply); "nimdeploy woocommerce replay" uses it.
	Payload json.RawMessage `json:"payload,omitempty"`
	Event   string          `json:"event,omitempty"`
}

func (s *Server) handleManualDeploy(w http.ResponseWriter, r *http.Request) {
	d, ok := s.cfg.Deploy[r.PathValue("name")]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown deploy")
		return
	}
	var req manualRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "cannot read body")
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	if req.User == "" {
		req.User = "api"
	}
	t := Trigger{
		Source:     TriggerManual,
		Provider:   d.Provider,
		Repository: d.Repository,
		Commit:     req.Commit,
		Pusher:     req.User,
		Event:      req.Event,
	}
	if len(req.Payload) > 0 {
		if len(req.Params) > 0 {
			writeError(w, http.StatusBadRequest, "send params or payload, not both")
			return
		}
		doc, err := decodeJSON(req.Payload)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON payload")
			return
		}
		params, reason, err := applyRules(d, doc)
		switch {
		case err != nil:
			writeError(w, http.StatusBadRequest, err.Error())
			return
		case reason != "":
			ignore(w, d, "", reason)
			return
		}
		t.Params, t.ResourceID = params, resourceID(doc)
		if d.payloadFile {
			t.Payload = req.Payload
		}
	} else {
		params, err := checkManualParams(d.Params, req.Params)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.Params = params
	}
	if d.Provider != providerGeneric && d.Provider != providerWooCommerce {
		t.Ref, t.Branch = "refs/heads/"+d.Branch, d.Branch
	}
	s.submit(w, d, t)
}

// handleRollback redeploys the last good commit, or the one given.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	d, ok := s.cfg.Deploy[r.PathValue("name")]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown deploy")
		return
	}
	if _, git := providers[d.Provider]; !git {
		writeError(w, http.StatusBadRequest, "rollback needs a git deploy (it redeploys a commit)")
		return
	}
	var req manualRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "cannot read body")
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	commit := req.Commit
	if commit == "" {
		if commit, _, err = s.runner.RollbackTarget(d.Name); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	}
	log.Printf("deploy=%s rollback to %s requested by %s", d.Name, shortSHA(commit), firstNonEmpty(req.User, "api"))
	s.submit(w, d, Trigger{
		Source:     TriggerRollback,
		Provider:   d.Provider,
		Repository: d.Repository,
		Ref:        "refs/heads/" + d.Branch,
		Branch:     d.Branch,
		Commit:     commit,
		Pusher:     firstNonEmpty(req.User, "api"),
	})
}

func (s *Server) submit(w http.ResponseWriter, d *DeployConfig, t Trigger) {
	res, err := s.runner.Submit(d.Name, t)
	switch {
	case errors.Is(err, ErrDuplicate):
		ignore(w, d, t.Delivery, "duplicate delivery")
	case errors.Is(err, ErrBusy):
		log.Printf("deploy=%s delivery=%s rejected: already running", d.Name, t.Delivery)
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrQueueFull):
		log.Printf("deploy=%s delivery=%s rejected: queue full (queue_max %d)", d.Name, t.Delivery, d.QueueMax)
		s.runner.notifyRejected(d, t, fmt.Sprintf("queue full (queue_max %d)", d.QueueMax))
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrUnknownDeploy):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		log.Printf("deploy=%s delivery=%s cannot start: %v", d.Name, t.Delivery, err)
		writeError(w, http.StatusInternalServerError, "cannot start deploy")
	default:
		if res.Result == ResultQueued {
			what := "commit=" + t.Commit
			if t.Commit == "" {
				what = "run=" + strings.ReplaceAll(firstNonEmpty(eventRef(State{Event: t.Event, ResourceID: t.ResourceID}), formatParams(t.Params), "-"), " ", "_")
			}
			msg := fmt.Sprintf("deploy=%s status=queued trigger=%s delivery=%s %s waiting=%d", d.Name, t.Source, t.Delivery, what, max(1, res.State.Queued.Count))
			if res.Replaced != "" {
				msg += " replaced=" + res.Replaced
			}
			log.Print(msg)
		}
		writeJSON(w, http.StatusAccepted, res)
	}
}

func ignore(w http.ResponseWriter, d *DeployConfig, delivery, reason string) {
	log.Printf("deploy=%s delivery=%s ignored: %s", d.Name, delivery, reason)
	writeJSON(w, http.StatusOK, map[string]string{"deploy": d.Name, "status": "ignored", "reason": reason})
}

func (s *Server) handleStatusAll(w http.ResponseWriter, r *http.Request) {
	filter, err := parseLabelFilter(r.URL.Query()["label"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	all := map[string]State{}
	for _, name := range s.cfg.DeployNames() {
		if st := s.runner.State(name); filter.match(st.Labels) {
			all[name] = st
		}
	}
	writeJSON(w, http.StatusOK, all)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.cfg.Deploy[name]; !ok {
		writeError(w, http.StatusNotFound, "unknown deploy")
		return
	}
	writeJSON(w, http.StatusOK, s.runner.State(name))
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.cfg.Deploy[name]; !ok {
		writeError(w, http.StatusNotFound, "unknown deploy")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	history, err := s.runner.History(name, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, history)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
