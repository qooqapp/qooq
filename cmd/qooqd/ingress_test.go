package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckIngressAppDomain(t *testing.T) {
	dir := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "*.qooq.localhost"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kder, _ := x509.MarshalECPrivateKey(key)
	cert, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	_ = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)

	tests := []struct {
		name                            string
		port                            int
		domain, redirect, cert, keyPath string
		ok                              bool
	}{
		{name: "nothing set: self-hosted as before", port: 443, ok: true},
		{name: "full shared-node set", port: 443, domain: "apps.example.net", redirect: "https://example.org", cert: cert, keyPath: keyFile, ok: true},
		{name: "app domain without a certificate", port: 443, domain: "apps.example.net", ok: true},
		{name: "bad https port", port: 0},
		{name: "bad app domain", port: 443, domain: "Apps.example.net"},
		{name: "redirect without app domain", port: 443, redirect: "https://example.org"},
		{name: "redirect not a URL", port: 443, domain: "apps.example.net", redirect: "example.org"},
		{name: "cert without key", port: 443, domain: "apps.example.net", cert: cert},
		{name: "key without cert", port: 443, domain: "apps.example.net", keyPath: keyFile},
		{name: "cert unreadable", port: 443, domain: "apps.example.net", cert: filepath.Join(dir, "missing"), keyPath: keyFile},
		{name: "cert without app domain", port: 443, cert: cert, keyPath: keyFile},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkIngressAppDomain(tc.port, tc.domain, tc.redirect, tc.cert, tc.keyPath)
			if (err == nil) != tc.ok {
				t.Errorf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// A shared node takes its settings from an env file; every new ingress flag has
// to be reachable from there.
func TestIngressAppDomainFlagsHaveEnv(t *testing.T) {
	want := map[string]string{
		"ingress-https-port":          "QOOQ_INGRESS_HTTPS_PORT",
		"ingress-app-domain":          "QOOQ_INGRESS_APP_DOMAIN",
		"ingress-app-domain-redirect": "QOOQ_INGRESS_APP_DOMAIN_REDIRECT",
		"ingress-cert-file":           "QOOQ_INGRESS_CERT_FILE",
		"ingress-key-file":            "QOOQ_INGRESS_KEY_FILE",
		"ingress-cloudflare":          "QOOQ_INGRESS_CLOUDFLARE",
	}
	for flagName, env := range want {
		if buildFlagEnv[flagName] != env {
			t.Errorf("%s: env = %q, want %q", flagName, buildFlagEnv[flagName], env)
		}
	}
}
