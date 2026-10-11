package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Agent certificates renew themselves. Before its hub client certificate
// expires, the agent sends the hub a certificate request for the key it
// already has, over a connection made with that certificate and signed with
// its token. The hub signs a new certificate for the same key and the agent
// writes it over cert_file: the key and the token stay as they are, and the
// key never leaves the server. An expired certificate can't connect, so it
// can't renew either: that one is replaced from the dashboard (New token).

const (
	hubCertPath          = "/api/v1/cert"
	agentCertRenewBefore = 40 * 24 * time.Hour // before certExpiringSoon: the hub warns only if renewing fails
	agentCertCheckEvery  = 12 * time.Hour
	agentCertRetry       = time.Hour
)

type hubCertRequest struct {
	CSR string `json:"csr"`
}

type hubCertAnswer struct {
	Cert     string    `json:"cert"`
	NotAfter time.Time `json:"not_after"`
}

// --- hub side -------------------------------------------------------------------------------

// handleCertRenew signs a new certificate for the key of the one the agent
// connected with.
func (h *hubServer) handleCertRenew(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read the request")
		return
	}
	a, ok := h.authAgent(w, r, body)
	if !ok {
		return
	}
	if !a.RequireCert || h.ca == nil {
		writeError(w, http.StatusConflict, "agent "+a.Name+" has no certificate from this hub to renew")
		return
	}
	used := r.TLS.VerifiedChains[0][0] // authAgent checked it is this agent's
	var req hubCertRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON")
		return
	}
	csr, err := parseCSR(req.CSR)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !samePublicKey(csr.PublicKey, used.PublicKey) {
		writeError(w, http.StatusBadRequest, "the request must be for the key of the current certificate")
		return
	}
	ic, err := h.ca.issueFor(a.Name, csr.PublicKey)
	if err != nil {
		log.Printf("hub: renewing %s's certificate: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "cannot sign the certificate")
		return
	}
	if err := h.store.renewAgentCert(r.Context(), a.Name, certFingerprint(used.Raw), ic.Fingerprint, ic.NotAfter); err != nil {
		log.Printf("hub: %v", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	log.Printf("hub: agent %s renewed its certificate from %s, valid until %s", a.Name, clientAddr(r), ic.NotAfter.Format("2006-01-02"))
	h.notifier.dispatch([]hubNotice{{Event: notifyCertRenewed, Agent: a.Name, Client: a.ClientName(), Env: a.EnvName(),
		LastSeen: a.LastSeen, CertNotAfter: ic.NotAfter, Time: time.Now()}})
	h.writeSigned(w, a.token, hubCertAnswer{Cert: ic.CertPEM, NotAfter: ic.NotAfter})
}

func parseCSR(s string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("csr: expected a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr: %w", err)
	}
	return csr, nil
}

func samePublicKey(a, b crypto.PublicKey) bool {
	k, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && k.Equal(b)
}

// --- agent side ------------------------------------------------------------------------------

// certLoop renews the hub client certificate when it gets close to expiring.
func (a *hubAgent) certLoop() {
	wait := time.Minute // after the first heartbeat
	for {
		select {
		case <-a.stop:
			return
		case <-time.After(wait):
		}
		wait = agentCertCheckEvery
		cfg := a.config()
		if cfg.CertFile == "" {
			continue
		}
		notAfter, renewed, err := renewHubCert(context.Background(), cfg, a.httpClient(), false)
		switch {
		case err != nil:
			log.Printf("hub: cannot renew the client certificate %s: %v (retrying in %s)", cfg.CertFile, err, agentCertRetry)
			wait = agentCertRetry
		case renewed:
			log.Printf("hub: client certificate %s renewed, valid until %s", cfg.CertFile, notAfter.Format("2006-01-02"))
			// New connections, so they present the new certificate.
			a.mu.Lock()
			a.client = hubClient(a.cfg)
			a.pollClient = nil
			a.mu.Unlock()
		}
	}
}

// renewHubCert asks the hub for a new certificate for the current key and
// writes it over cert_file, when it expires within agentCertRenewBefore (or
// always, with force). It returns the certificate's expiry.
func renewHubCert(ctx context.Context, cfg HubConfig, client *http.Client, force bool) (time.Time, bool, error) {
	pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return time.Time{}, false, err
	}
	cur, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return time.Time{}, false, err
	}
	left := time.Until(cur.NotAfter)
	if left <= 0 {
		return cur.NotAfter, false, fmt.Errorf("it expired on %s: issue a new one from the hub (Agents, New token)", cur.NotAfter.Format("2006-01-02"))
	}
	if !force && left > agentCertRenewBefore {
		return cur.NotAfter, false, nil
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return cur.NotAfter, false, errors.New("unsupported key type")
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cfg.Agent}, DNSNames: []string{cfg.Agent}}, key)
	if err != nil {
		return cur.NotAfter, false, err
	}
	body, _ := json.Marshal(hubCertRequest{CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL+hubCertPath, bytes.NewReader(body))
	if err != nil {
		return cur.NotAfter, false, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nimdeploy/"+version)
	req.Header.Set(hubAgentHeader, cfg.Agent)
	req.Header.Set(hubTimestampHeader, ts)
	req.Header.Set(hubSignatureHeader, signHub([]byte(cfg.token), ts, body))
	resp, err := client.Do(req)
	if err != nil {
		return cur.NotAfter, false, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return cur.NotAfter, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return cur.NotAfter, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(truncate(string(answer), 200)))
	}
	if err := verifyHubSignature(cfg.token, resp.Header.Get(hubTimestampHeader), answer, resp.Header.Get(hubSignatureHeader), time.Now()); err != nil {
		return cur.NotAfter, false, fmt.Errorf("answer not signed by the hub: %v", err)
	}
	var ans hubCertAnswer
	if err := json.Unmarshal(answer, &ans); err != nil {
		return cur.NotAfter, false, err
	}
	block, _ := pem.Decode([]byte(ans.Cert))
	if block == nil {
		return cur.NotAfter, false, errors.New("the hub sent no certificate")
	}
	next, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return cur.NotAfter, false, err
	}
	if !samePublicKey(key.Public(), next.PublicKey) || next.Subject.CommonName != cfg.Agent || next.NotAfter.Before(cur.NotAfter) {
		return cur.NotAfter, false, errors.New("the hub's certificate is not a renewal of this one")
	}
	if err := replaceFile(cfg.CertFile, []byte(ans.Cert)); err != nil {
		return cur.NotAfter, false, err
	}
	return next.NotAfter, true, nil
}

// replaceFile writes data over path keeping its mode: a new file renamed
// over it when the directory allows, else in place.
func replaceFile(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*"); err == nil {
		name := tmp.Name()
		_, err = tmp.Write(data)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(name, mode)
		}
		if err == nil {
			err = os.Rename(name, path)
		}
		if err != nil {
			_ = os.Remove(name)
		}
		return err
	}
	return os.WriteFile(path, data, mode)
}

// cliRenewCert: "nimdeploy renew-cert" renews the hub client certificate now.
func cliRenewCert(cfg *Config, envFile string, args []string) int {
	fset := flag.NewFlagSet("renew-cert", flag.ExitOnError)
	ifNeeded := fset.Bool("if-needed", false, "only when it expires within "+fmt.Sprint(agentCertRenewBefore.Hours()/24)+" days (what the service does)")
	fset.Usage = func() {
		_, _ = io.WriteString(fset.Output(), "usage: nimdeploy renew-cert [-if-needed]\n\nAsks the hub for a new [hub] client certificate for the same key and writes it over\ncert_file. The service renews it by itself before it expires; this does it now.\n\n")
		fset.PrintDefaults()
	}
	fset.Parse(args)
	h := cfg.Hub
	if h.URL == "" || h.CertFile == "" {
		fmt.Fprintln(os.Stderr, "nimdeploy renew-cert: needs [hub] url, cert_file and key_file")
		return 2
	}
	h.token = os.Getenv(h.TokenEnv)
	if h.token == "" {
		var err error
		if h.token, err = readEnvFile(envFile, h.TokenEnv); err != nil || h.token == "" {
			fmt.Fprintf(os.Stderr, "nimdeploy renew-cert: the agent token: export %s or run with access to %s (%v)\n", h.TokenEnv, envFile, err)
			return 1
		}
	}
	notAfter, renewed, err := renewHubCert(context.Background(), h, hubClient(h), !*ifNeeded)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nimdeploy renew-cert:", err)
		return 1
	}
	if !renewed {
		fmt.Printf("%s is valid until %s: nothing to do\n", h.CertFile, notAfter.Format("2006-01-02"))
		return 0
	}
	fmt.Printf("%s renewed, valid until %s (the service picks it up by itself)\n", h.CertFile, notAfter.Format("2006-01-02"))
	return 0
}
