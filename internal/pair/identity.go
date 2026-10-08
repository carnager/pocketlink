// Package pair handles device identity, the trust store and the one-time
// pairing handshake.
//
// Every peer has a long-lived self-signed certificate. Peers are identified
// by the SHA-256 fingerprint of that certificate, which is exchanged once via
// QR code and pinned from then on. No CA is involved.
package pair

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// LoadOrCreateIdentity loads the TLS identity stored in dir, creating a new
// self-signed certificate on first use. ECDSA P-256 is used rather than
// Ed25519 because older Android TLS stacks don't handle Ed25519 certificates.
func LoadOrCreateIdentity(dir, cn string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		return cert, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return tls.Certificate{}, err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(30, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// Fingerprint returns the hex SHA-256 of a DER-encoded certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// ShortID abbreviates a fingerprint for display.
func ShortID(fp string) string {
	if len(fp) > 16 {
		return fp[:16]
	}
	return fp
}

// PinnedVerifier returns a tls.Config.VerifyPeerCertificate func that accepts
// only a peer whose certificate matches fp. Use with InsecureSkipVerify, which
// disables CA chain validation but still runs this callback.
func PinnedVerifier(fp string) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("no peer certificate")
		}
		if got := Fingerprint(raw[0]); subtle.ConstantTimeCompare([]byte(got), []byte(fp)) != 1 {
			return fmt.Errorf("certificate fingerprint mismatch: got %s", got)
		}
		return nil
	}
}
