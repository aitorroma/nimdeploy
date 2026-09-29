package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
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
		mux.HandleFunc("POST "+d.Path, s.handleWebhook(d))
	}
	mux.HandleFunc("GET /status", s.requireToken(false, s.handleStatusAll))
	mux.HandleFunc("GET /status/{name}", s.requireToken(false, s.handleStatus))
	mux.HandleFunc("POST /deploy/{name}", s.requireToken(true, s.handleManualDeploy))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
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

type pushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`
}

func (s *Server) handleWebhook(d *DeployConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		delivery := r.Header.Get("X-GitHub-Delivery")
		event := r.Header.Get("X-GitHub-Event")

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "cannot read body")
			return
		}
		if !validSignature(d.secret, body, r.Header.Get("X-Hub-Signature-256")) {
			log.Printf("deploy=%s delivery=%s rejected: invalid signature from %s", d.Name, delivery, r.RemoteAddr)
			writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}

		switch event {
		case "ping":
			writeJSON(w, http.StatusOK, map[string]string{"deploy": d.Name, "status": "pong"})
			return
		case "push":
		default:
			ignore(w, d, delivery, "event "+event+" not handled")
			return
		}

		// GitHub can send either application/json or form-encoded "payload=".
		// Fall back to the raw body so a plain `curl -d '{...}'` also works.
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			if form, err := url.ParseQuery(string(body)); err == nil && form.Get("payload") != "" {
				body = []byte(form.Get("payload"))
			}
		}
		var p pushPayload
		if err := json.Unmarshal(body, &p); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON payload")
			return
		}

		if !strings.EqualFold(p.Repository.FullName, d.Repository) {
			ignore(w, d, delivery, "repository "+p.Repository.FullName+" does not match")
			return
		}
		branch, isBranch := strings.CutPrefix(p.Ref, "refs/heads/")
		if !isBranch || branch != d.Branch {
			ignore(w, d, delivery, "ref "+p.Ref+" is not branch "+d.Branch)
			return
		}
		if p.Deleted {
			ignore(w, d, delivery, "branch deleted")
			return
		}

		s.submit(w, d, Trigger{
			Source:     TriggerWebhook,
			Delivery:   delivery,
			Repository: p.Repository.FullName,
			Ref:        p.Ref,
			Branch:     branch,
			Commit:     p.After,
			Pusher:     p.Pusher.Name,
		})
	}
}

type manualRequest struct {
	Commit string `json:"commit"`
	User   string `json:"user"`
}

func (s *Server) handleManualDeploy(w http.ResponseWriter, r *http.Request) {
	d, ok := s.cfg.Deploy[r.PathValue("name")]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown deploy")
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
	if req.User == "" {
		req.User = "api"
	}
	s.submit(w, d, Trigger{
		Source:     TriggerManual,
		Repository: d.Repository,
		Ref:        "refs/heads/" + d.Branch,
		Branch:     d.Branch,
		Commit:     req.Commit,
		Pusher:     req.User,
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
	case errors.Is(err, ErrUnknownDeploy):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		log.Printf("deploy=%s delivery=%s cannot start: %v", d.Name, t.Delivery, err)
		writeError(w, http.StatusInternalServerError, "cannot start deploy")
	default:
		if res.Result == ResultQueued {
			log.Printf("deploy=%s status=queued trigger=%s delivery=%s commit=%s replaced=%s", d.Name, t.Source, t.Delivery, t.Commit, res.Replaced)
		}
		writeJSON(w, http.StatusAccepted, res)
	}
}

func ignore(w http.ResponseWriter, d *DeployConfig, delivery, reason string) {
	log.Printf("deploy=%s delivery=%s ignored: %s", d.Name, delivery, reason)
	writeJSON(w, http.StatusOK, map[string]string{"deploy": d.Name, "status": "ignored", "reason": reason})
}

func validSignature(secret, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func (s *Server) handleStatusAll(w http.ResponseWriter, r *http.Request) {
	all := map[string]State{}
	for _, name := range s.cfg.DeployNames() {
		all[name] = s.runner.State(name)
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
