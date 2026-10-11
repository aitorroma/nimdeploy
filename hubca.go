package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The hub's own CA for agent certificates (mTLS). When an agent is created
// with "require a client certificate", the hub issues it a certificate named
// like the agent and keeps its fingerprint: only that certificate is
// accepted, so renewing or revoking the agent invalidates the old one at
// once, without revocation lists. The CA lives in -agent-ca-dir (a volume
// or a Kubernetes Secret), created on first start when empty.

const (
	hubCAFile        = "ca.pem"
	hubCAKeyFile     = "ca.key"
	agentCertValid   = 365 * 24 * time.Hour
	hubCAValid       = 10 * 365 * 24 * time.Hour
	certExpiringSoon = 30 * 24 * time.Hour
)

type hubCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// loadOrCreateCA reads the CA from dir, or creates one there.
func loadOrCreateCA(dir string) (*hubCA, error) {
	certPath, keyPath := filepath.Join(dir, hubCAFile), filepath.Join(dir, hubCAKeyFile)
	certPEM, err1 := os.ReadFile(certPath)
	keyPEM, err2 := os.ReadFile(keyPath)
	if err1 == nil && err2 == nil {
		return parseCA(certPEM, keyPEM)
	}
	if !errors.Is(err1, os.ErrNotExist) || !errors.Is(err2, os.ErrNotExist) {
		return nil, fmt.Errorf("agent CA in %s: need both %s and %s", dir, hubCAFile, hubCAKeyFile)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: "nimdeploy hub agents"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(hubCAValid), IsCA: true, BasicConstraintsValid: true,
		MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return parseCA(certPEM, keyPEM)
}

func parseCA(certPEM, keyPEM []byte) (*hubCA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("agent CA: not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("agent CA key: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("agent CA: the certificate is not a CA")
	}
	return &hubCA{cert: cert, key: key}, nil
}

func randomSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// issuedCert is an agent certificate, shown once.
type issuedCert struct {
	CertPEM, KeyPEM string
	Fingerprint     string
	NotAfter        time.Time
}

// issue makes a client certificate for an agent (CN and DNS name = agent).
func (ca *hubCA) issue(agent string) (issuedCert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return issuedCert{}, err
	}
	notAfter := time.Now().Add(agentCertValid)
	if notAfter.After(ca.cert.NotAfter) {
		notAfter = ca.cert.NotAfter
	}
	tmpl := &x509.Certificate{SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: agent, OrganizationalUnit: []string{"nimdeploy agent"}},
		DNSNames: []string{agent}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return issuedCert{}, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return issuedCert{}, err
	}
	return issuedCert{
		CertPEM:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})),
		Fingerprint: certFingerprint(der),
		NotAfter:    notAfter,
	}, nil
}

func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// requestCertFingerprint is the fingerprint of the request's verified client
// certificate, or "" without one.
func requestCertFingerprint(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return ""
	}
	return certFingerprint(r.TLS.VerifiedChains[0][0].Raw)
}

// withClientCA makes a server TLS config also accept client certificates
// signed by ca (verified when sent; each agent's policy decides if required).
func withClientCA(tc *tls.Config, ca *x509.Certificate) *tls.Config {
	base := tc.GetConfigForClient
	tc.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		var c *tls.Config
		if base != nil {
			var err error
			if c, err = base(hello); err != nil {
				return nil, err
			}
		} else {
			c = tc.Clone()
			c.GetConfigForClient = nil
		}
		pool := x509.NewCertPool()
		if c.ClientCAs != nil {
			pool = c.ClientCAs.Clone()
		}
		pool.AddCert(ca)
		c.ClientCAs = pool
		if c.ClientAuth == tls.NoClientCert {
			c.ClientAuth = tls.VerifyClientCertIfGiven
		}
		return c, nil
	}
	return tc
}
