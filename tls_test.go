package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type testPKI struct {
	dir    string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caFile string
	serial int64
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	p := &testPKI{dir: t.TempDir(), ca: ca, caKey: key, serial: 1}
	p.caFile = filepath.Join(p.dir, "ca.pem")
	_ = os.WriteFile(p.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	return p
}

// issue writes a certificate and key signed by the CA; server ones are for 127.0.0.1.
func (p *testPKI) issue(t *testing.T, name string, server bool) (certFile, keyFile string) {
	t.Helper()
	p.serial++
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		tmpl.DNSNames = []string{name + ".clients.test"}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(p.dir, name+".pem"), filepath.Join(p.dir, name+"-key.pem")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return certFile, keyFile
}

func (p *testPKI) client(t *testing.T, name string) *http.Client {
	t.Helper()
	var cert, key string
	if name != "" {
		cert, key = p.issue(t, name, false)
	}
	tc, err := clientTLS(p.caFile, cert, key)
	if err != nil {
		t.Fatal(err)
	}
	return httpClientTLS(10*time.Second, tc)
}

func serveTLS(t *testing.T, h http.Handler, tc *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(tls.NewListener(ln, tc)) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String()
}

func TestServerTLSAndClientNames(t *testing.T) {
	old := tlsReloadEvery
	tlsReloadEvery = 50 * time.Millisecond
	t.Cleanup(func() { tlsReloadEvery = old })

	p := newTestPKI(t)
	cert, key := p.issue(t, "server", true)
	e := newGenericEnv(t, `echo ok`, `client_names = ["awx", "ci.clients.test"]`, `[server]
api_token_env = "TEST_API_TOKEN"
tls_cert_file = "`+cert+`"
tls_key_file = "`+key+`"
tls_client_ca_file = "`+p.caFile+`"
api_client_names = ["ops"]
`)
	s := e.cfg.Server
	tc, err := serverTLS(s.TLSCertFile, s.TLSKeyFile, s.TLSClientCAFile, s.TLSClientAuth, s.TLSMinVersion)
	if err != nil {
		t.Fatal(err)
	}
	base := serveTLS(t, e.h, tc)

	post := func(c *http.Client, delivery string) int {
		body := `{"x":1}`
		req, _ := http.NewRequest(http.MethodPost, base+"/hooks/svc", strings.NewReader(body))
		req.Header.Set("X-Signature", signGeneric([]byte(testSecret), []byte(body), ""))
		req.Header.Set("X-Delivery-ID", delivery)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(p.client(t, ""), "t1"); code != http.StatusForbidden {
		t.Errorf("no client cert: %d", code)
	}
	if code := post(p.client(t, "other"), "t2"); code != http.StatusForbidden {
		t.Errorf("wrong client cert: %d", code)
	}
	if code := post(p.client(t, "awx"), "t3"); code != http.StatusAccepted {
		t.Errorf("awx (CN): %d", code)
	}
	e.waitIdleName(t, "svc")
	if code := post(p.client(t, "ci"), "t4"); code != http.StatusAccepted { // matches its DNS SAN
		t.Errorf("ci (SAN): %d", code)
	}
	e.waitIdleName(t, "svc")

	get := func(c *http.Client) int {
		req, _ := http.NewRequest(http.MethodGet, base+"/status", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(p.client(t, "awx")); code != http.StatusForbidden {
		t.Errorf("API with awx: %d", code)
	}
	if code := get(p.client(t, "ops")); code != http.StatusOK {
		t.Errorf("API with ops: %d", code)
	}

	// A client certificate from another CA doesn't even get a connection.
	other := newTestPKI(t)
	oc, ok := other.issue(t, "awx", false)
	tcOther, _ := clientTLS(p.caFile, oc, ok)
	if _, err := httpClientTLS(5*time.Second, tcOther).Get(base + "/healthz"); err == nil {
		t.Error("certificate from another CA accepted")
	}

	// Renewing the server certificate in place is picked up without a restart.
	first := serverSerial(t, base, p.client(t, ""))
	newCert, newKey := p.issue(t, "server2", true)
	data, _ := os.ReadFile(newCert)
	kdata, _ := os.ReadFile(newKey)
	_ = os.WriteFile(key, kdata, 0o600)
	_ = os.WriteFile(cert, data, 0o644)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(cert, future, future)
	_ = os.Chtimes(key, future, future)
	time.Sleep(100 * time.Millisecond)
	_ = serverSerial(t, base, p.client(t, "")) // triggers the check
	if second := serverSerial(t, base, p.client(t, "")); second == first {
		t.Error("certificate not reloaded")
	}

	if tcRequire, _ := serverTLS(cert, key, p.caFile, tlsClientAuthRequire, "1.3"); tcRequire != nil {
		reqBase := serveTLS(t, e.h, tcRequire)
		if _, err := p.client(t, "").Get(reqBase + "/healthz"); err == nil {
			t.Error("require: connection without a client certificate")
		}
		if resp, err := p.client(t, "any").Get(reqBase + "/healthz"); err != nil || resp.StatusCode != 200 {
			t.Errorf("require with a certificate: %v", err)
		}
	}
}

func serverSerial(t *testing.T, base string, c *http.Client) string {
	t.Helper()
	c.Transport.(*http.Transport).DisableKeepAlives = true
	resp, err := c.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.TLS.PeerCertificates[0].SerialNumber.String()
}

func TestTLSConfigValidation(t *testing.T) {
	base := "[deploy.a]\nschedule = \"@daily\"\ncommand = \"/bin/true\"\n[logging]\ndirectory = \"/tmp/x\"\n"
	cases := map[string]string{
		"[server]\ntls_cert_file = \"/x\"\n":                                                       "go together",
		"[server]\ntls_client_ca_file = \"/x\"\n":                                                  "need tls_cert_file",
		"[server]\ntls_cert_file = \"/x\"\ntls_key_file = \"/y\"\ntls_client_auth = \"require\"\n": "needs tls_client_ca_file",
		"[server]\nlisten = \"unix:/tmp/n.sock\"\ntls_cert_file = \"/x\"\ntls_key_file = \"/y\"\n": "unix socket",
		"[server]\ntls_cert_file = \"/x\"\ntls_key_file = \"/y\"\ntls_min_version = \"1.1\"\n":     "1.2 or 1.3",
		"[server]\npprof = true\n":                                                                 "pprof needs api_token_env",
	}
	for extra, want := range cases {
		path := filepath.Join(t.TempDir(), "c.toml")
		_ = os.WriteFile(path, []byte(extra+base), 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", extra, err, want)
		}
	}
	path := filepath.Join(t.TempDir(), "c.toml")
	_ = os.WriteFile(path, []byte(base+"[deploy.b]\npath = \"/hooks/b\"\nrepository = \"a/b\"\nsecret_env = \"S\"\ncommand = \"/bin/true\"\nclient_names = [\"x\"]\n"), 0o600)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "client_names needs") {
		t.Errorf("client_names without CA: %v", err)
	}
}

// TestHubAgentCertificates: with -tls-client-ca the agents' endpoint checks
// that a certificate names the agent sending.
func TestHubAgentCertificates(t *testing.T) {
	store := testHubStore(t)
	p := newTestPKI(t)
	cert, key := p.issue(t, "hub", true)
	h := &hubServer{store: store, opts: &hubOptions{uiToken: testHubToken, TLSClientCA: p.caFile, AgentCerts: tlsClientAuthRequire}}
	tc, err := serverTLS(cert, key, p.caFile, tlsClientAuthOptional, "")
	if err != nil {
		t.Fatal(err)
	}
	base := serveTLS(t, h.routes(), tc)
	ctx := context.Background()
	token, _ := store.addAgent(ctx, "stage-1")

	send := func(agentCert string) error {
		var certFile, keyFile string
		if agentCert != "" {
			certFile, keyFile = p.issue(t, agentCert, false)
		}
		a := &hubAgent{cfg: HubConfig{URL: base, Agent: "stage-1", token: token, CAFile: p.caFile, CertFile: certFile, KeyFile: keyFile}}
		a.client = hubClient(a.cfg)
		return a.post(ctx, hubBatch{})
	}
	if err := send("stage-1"); err != nil {
		t.Errorf("own certificate: %v", err)
	}
	if err := send("stage-2"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("another agent's certificate: %v", err)
	}
	if err := send(""); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("no certificate with require: %v", err)
	}
	// The dashboard and health check need no client certificate.
	if resp, err := p.client(t, "").Get(base + "/healthz"); err != nil || resp.StatusCode != 200 {
		t.Errorf("healthz without a certificate: %v", err)
	}
	_ = httptest.NewRecorder
}

func TestCertExpiryAndSaturationMetrics(t *testing.T) {
	p := newTestPKI(t)
	cert, _ := p.issue(t, "server", true)
	watchCertExpiry("server", cert)
	watchCertExpiry("client_ca", p.caFile)
	t.Cleanup(func() {
		certFiles.Lock()
		certFiles.m = map[string]string{}
		certFiles.Unlock()
	})
	at, ok := certExpiry(cert)
	if !ok || time.Until(at) < 50*time.Minute || time.Until(at) > 70*time.Minute {
		t.Fatalf("expiry %v %v", at, ok)
	}

	e := newEnv(t, `sleep 1`, "")
	e.push(t, pushOpts{delivery: "m1"})
	e.push(t, pushOpts{delivery: "m2", commit: sha2}) // waits behind m1
	body := renderMetrics(e.cfg, e.runner)
	for _, want := range []string{
		"nimdeploy_runs_running 1\n",
		"nimdeploy_runs_queued 1\n",
		`nimdeploy_tls_cert_expiry_timestamp_seconds{cert="server"} ` + strconv.FormatInt(at.Unix(), 10),
		`nimdeploy_tls_cert_expiry_timestamp_seconds{cert="client_ca"} `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if !strings.Contains(body, "nimdeploy_runs_oldest_queued_seconds 0.") && !strings.Contains(body, "nimdeploy_runs_oldest_queued_seconds 1.") {
		t.Errorf("oldest queued:\n%s", body)
	}
	e.waitIdle(t)
	if body := renderMetrics(e.cfg, e.runner); !strings.Contains(body, "nimdeploy_runs_queued 0\n") || !strings.Contains(body, "nimdeploy_runs_oldest_queued_seconds 0.000\n") {
		t.Error("saturation after the queue drained")
	}
}
