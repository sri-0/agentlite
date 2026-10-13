package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/youmark/pkcs8"
)

// TestClientCertificate covers the key formats operators mount: plain,
// encrypted PKCS#8 and legacy PEM encryption, and the configuration
// mistakes that must fail at start rather than fall back to plain TLS.
func TestClientCertificate(t *testing.T) {
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	password := []byte("s3cret")

	plain, _ := x509.MarshalPKCS8PrivateKey(ecKey)
	encrypted, err := pkcs8.MarshalPrivateKey(ecKey, password, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey), password, x509.PEMCipherAES256) //nolint:staticcheck // the format under test
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	write := func(name string, block *pem.Block) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ecCert := write("ec.crt", &pem.Block{Type: "CERTIFICATE", Bytes: selfSigned(t, ecKey.Public(), ecKey)})
	rsaCert := write("rsa.crt", &pem.Block{Type: "CERTIFICATE", Bytes: selfSigned(t, rsaKey.Public(), rsaKey)})
	plainKey := write("plain.key", &pem.Block{Type: "PRIVATE KEY", Bytes: plain})
	encryptedKey := write("encrypted.key", &pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: encrypted})
	legacyKey := write("legacy.key", legacy)

	for _, tc := range []struct {
		name, cert, key string
		password        []byte
		ok              bool
	}{
		{"plain key", ecCert, plainKey, nil, true},
		{"encrypted PKCS#8", ecCert, encryptedKey, password, true},
		{"legacy PEM encryption", rsaCert, legacyKey, password, true},
		{"wrong password", ecCert, encryptedKey, []byte("nope"), false},
		{"encrypted key, no password", ecCert, encryptedKey, nil, false},
		{"plain key with a password", ecCert, plainKey, password, false},
		{"key of another certificate", rsaCert, encryptedKey, password, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := clientCertificate(tc.cert, tc.key, tc.password)
			if (err == nil) != tc.ok {
				t.Errorf("ok = %v, err = %v", tc.ok, err)
			}
		})
	}
}

func selfSigned(t *testing.T, pub, priv any) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
