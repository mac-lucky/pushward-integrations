package bambulab

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
)

func TestNewClient_FingerprintParsing(t *testing.T) {
	// Plain hex.
	c, err := NewClient("h", "a", "s", false, strings.Repeat("ab", sha256.Size))
	if err != nil {
		t.Fatalf("plain hex: %v", err)
	}
	if len(c.certFingerprint) != sha256.Size {
		t.Fatalf("expected %d-byte fingerprint, got %d", sha256.Size, len(c.certFingerprint))
	}

	// Colon-separated hex (openssl format).
	colonHex := strings.Join(make([]string, sha256.Size), "ab:") + "ab"
	if _, err := NewClient("h", "a", "s", false, colonHex); err != nil {
		t.Fatalf("colon hex: %v", err)
	}

	// Wrong length.
	if _, err := NewClient("h", "a", "s", false, "abcd"); err == nil {
		t.Fatal("expected error for short fingerprint")
	}

	// Garbage.
	if _, err := NewClient("h", "a", "s", false, "not-hex-zz"); err == nil {
		t.Fatal("expected error for non-hex fingerprint")
	}
}

func TestTLSConfig_Pinning(t *testing.T) {
	// Build a fingerprint from a fake DER blob.
	fakeCert := []byte("fake-printer-cert-bytes")
	fp := sha256.Sum256(fakeCert)

	c := &Client{}
	cfg := c.tlsConfig(fp[:])
	if cfg.VerifyConnection == nil {
		t.Fatal("expected VerifyConnection callback when pinning")
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify must remain true so default verifier is bypassed")
	}

	// Matching cert: callback returns nil.
	good := tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: fakeCert}}}
	if err := cfg.VerifyConnection(good); err != nil {
		t.Fatalf("matching fingerprint should pass, got: %v", err)
	}

	// Mismatching cert: callback returns error.
	bad := tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("different")}}}
	if err := cfg.VerifyConnection(bad); err == nil {
		t.Fatal("mismatching fingerprint should fail")
	}

	// No peer certs: callback returns error.
	none := tls.ConnectionState{}
	if err := cfg.VerifyConnection(none); err == nil {
		t.Fatal("missing peer certs should fail")
	}
}

func TestTLSConfig_InsecureSkipVerify(t *testing.T) {
	c := &Client{insecureSkipVerify: true}
	cfg := c.tlsConfig(nil)
	if cfg.VerifyConnection != nil {
		t.Fatal("no fingerprint → no VerifyConnection callback expected")
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=true fallback")
	}
}
