package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Native TLS, so nimdeploy (and the hub) can be reached over HTTPS without a
// reverse proxy, and mutual TLS: clients identified by a certificate from
// your CA. Certificate and CA files are re-read when they change (checked at
// most every few seconds), so renewals need no restart.

const (
	tlsClientAuthOptional = "optional" // verify a client certificate if one is sent
	tlsClientAuthRequire  = "require"  // no verified client certificate, no connection
)

// tlsReloadEvery is how often changed certificate files are looked for.
var tlsReloadEvery = 5 * time.Second

// fileWatch re-reads a set of files when any of their modification times
// changes, keeping the last good result when a reload fails.
type fileWatch[T any] struct {
	mu      sync.Mutex
	paths   []string
	load    func() (T, error)
	value   T
	mtimes  []time.Time
	checked time.Time
}

func newFileWatch[T any](load func() (T, error), paths ...string) (*fileWatch[T], error) {
	w := &fileWatch[T]{paths: paths, load: load}
	v, err := load()
	if err != nil {
		return nil, err
	}
	w.value, w.mtimes, w.checked = v, w.stat(), time.Now()
	return w, nil
}

func (w *fileWatch[T]) stat() []time.Time {
	out := make([]time.Time, len(w.paths))
	for i, p := range w.paths {
		if fi, err := os.Stat(p); err == nil {
			out[i] = fi.ModTime()
		}
	}
	return out
}

func (w *fileWatch[T]) get() T {
	w.mu.Lock()
	defer w.mu.Unlock()
	if time.Since(w.checked) < tlsReloadEvery {
		return w.value
	}
	w.checked = time.Now()
	now := w.stat()
	changed := false
	for i := range now {
		if !now[i].Equal(w.mtimes[i]) {
			changed = true
		}
	}
	if !changed {
		return w.value
	}
	v, err := w.load()
	if err != nil {
		log.Printf("tls: keeping the previous %s: reload failed: %v", strings.Join(w.paths, ", "), err)
		return w.value
	}
	log.Printf("tls: reloaded %s", strings.Join(w.paths, ", "))
	w.value, w.mtimes = v, now
	return w.value
}

func loadCertPair(certFile, keyFile string) func() (*tls.Certificate, error) {
	return func() (*tls.Certificate, error) {
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		return &c, nil
	}
}

func loadCAPool(file string) func() (*x509.CertPool, error) {
	return func() (*x509.CertPool, error) {
		pem, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s has no PEM certificates", file)
		}
		return pool, nil
	}
}

func tlsVersion(v string) (uint16, error) {
	switch v {
	case "", "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	}
	return 0, fmt.Errorf("tls_min_version must be 1.2 or 1.3")
}

// serverTLS builds a server TLS config that reloads its files on change.
func serverTLS(certFile, keyFile, clientCAFile, clientAuth, minVersion string) (*tls.Config, error) {
	minV, err := tlsVersion(minVersion)
	if err != nil {
		return nil, err
	}
	certs, err := newFileWatch(loadCertPair(certFile, keyFile), certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	base := &tls.Config{
		MinVersion:     minV,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return certs.get(), nil },
	}
	if clientCAFile == "" {
		return base, nil
	}
	cas, err := newFileWatch(loadCAPool(clientCAFile), clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("client CA: %w", err)
	}
	mode := tls.VerifyClientCertIfGiven
	if clientAuth == tlsClientAuthRequire {
		mode = tls.RequireAndVerifyClientCert
	}
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		c := base.Clone()
		c.GetConfigForClient = nil
		c.ClientCAs = cas.get()
		c.ClientAuth = mode
		return c, nil
	}
	return base, nil
}

// clientTLS is the TLS config for outgoing requests (to the hub, to an OTLP
// collector): a private CA and/or a client certificate, both reloaded.
func clientTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	if caFile == "" && certFile == "" && keyFile == "" {
		return nil, nil
	}
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("cert_file and key_file go together")
	}
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pool, err := loadCAPool(caFile)()
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		c.RootCAs = pool
	}
	if certFile != "" {
		certs, err := newFileWatch(loadCertPair(certFile, keyFile), certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		c.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return certs.get(), nil }
	}
	return c, nil
}

func httpClientTLS(timeout time.Duration, tc *tls.Config) *http.Client {
	if tc == nil {
		return &http.Client{Timeout: timeout}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	return &http.Client{Timeout: timeout, Transport: tr}
}

// clientNames is who a verified client certificate says the client is: its
// common name, DNS names, URIs and email addresses.
func clientNames(r *http.Request) []string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return nil
	}
	c := r.TLS.VerifiedChains[0][0]
	names := []string{}
	if c.Subject.CommonName != "" {
		names = append(names, c.Subject.CommonName)
	}
	names = append(names, c.DNSNames...)
	names = append(names, c.EmailAddresses...)
	for _, u := range c.URIs {
		names = append(names, u.String())
	}
	return names
}

// clientAllowed reports whether the request's verified certificate carries
// one of the allowed names; no allowed names means anyone.
func clientAllowed(r *http.Request, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, n := range clientNames(r) {
		for _, a := range allowed {
			if strings.EqualFold(n, a) {
				return true
			}
		}
	}
	return false
}

func (s *ServerConfig) validateTLS() error {
	tlsSet := s.TLSCertFile != "" || s.TLSKeyFile != ""
	if (s.TLSCertFile == "") != (s.TLSKeyFile == "") {
		return errors.New("tls_cert_file and tls_key_file go together")
	}
	if !tlsSet && (s.TLSClientCAFile != "" || s.TLSClientAuth != "" || s.TLSMinVersion != "" || len(s.APIClientNames) > 0) {
		return errors.New("tls_client_ca_file, tls_client_auth, tls_min_version and api_client_names need tls_cert_file and tls_key_file")
	}
	if tlsSet && s.socketPath != "" {
		return errors.New("TLS is for a TCP listen address, not a unix socket")
	}
	switch s.TLSClientAuth {
	case "":
		if s.TLSClientCAFile != "" {
			s.TLSClientAuth = tlsClientAuthOptional
		}
	case tlsClientAuthOptional, tlsClientAuthRequire:
		if s.TLSClientCAFile == "" {
			return errors.New("tls_client_auth needs tls_client_ca_file")
		}
	default:
		return errors.New("tls_client_auth must be optional or require")
	}
	if len(s.APIClientNames) > 0 && s.TLSClientCAFile == "" {
		return errors.New("api_client_names needs tls_client_ca_file")
	}
	if _, err := tlsVersion(s.TLSMinVersion); err != nil {
		return err
	}
	return nil
}

func (s *ServerConfig) tlsEnabled() bool { return s.TLSCertFile != "" }

// --- certificate expiry, for alerts ----------------------------------------------------

var certFiles = struct {
	sync.Mutex
	m map[string]string // role → PEM file
}{m: map[string]string{}}

// watchCertExpiry exposes a certificate file's expiry as
// nimdeploy_tls_cert_expiry_timestamp_seconds{cert=role}. Empty paths are skipped.
func watchCertExpiry(role, path string) {
	if path == "" {
		return
	}
	certFiles.Lock()
	certFiles.m[role] = path
	certFiles.Unlock()
}

// certExpiry is the earliest NotAfter of the certificates in a PEM file
// (a chain or a CA bundle expires with its first certificate).
func certExpiry(path string) (time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	var first time.Time
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if first.IsZero() || c.NotAfter.Before(first) {
			first = c.NotAfter
		}
	}
	return first, !first.IsZero()
}

// writeCertExpiry adds the expiry metric to a /metrics page.
func writeCertExpiry(b *strings.Builder) {
	certFiles.Lock()
	roles := sortedKeys(certFiles.m)
	paths := make([]string, len(roles))
	for i, r := range roles {
		paths[i] = certFiles.m[r]
	}
	certFiles.Unlock()
	if len(roles) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP nimdeploy_tls_cert_expiry_timestamp_seconds When a certificate in use expires (Unix time): server, client_ca, hub_client, hub_ca, otel_client, otel_ca.\n# TYPE nimdeploy_tls_cert_expiry_timestamp_seconds gauge\n")
	for i, role := range roles {
		if at, ok := certExpiry(paths[i]); ok {
			fmt.Fprintf(b, "nimdeploy_tls_cert_expiry_timestamp_seconds{cert=%q} %d\n", role, at.Unix())
		}
	}
}
