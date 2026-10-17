package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
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
	//lint:ignore SA1019 the format under test
	legacy, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey), password,
		x509.PEMCipherAES256)
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

// TestHeadersStayWithTheirHost: the configured and forwarded headers carry
// credentials (an API key, the user's token), so a redirect to another host
// must get none of them, on that hop or any after it.
func TestHeadersStayWithTheirHost(t *testing.T) {
	var mu sync.Mutex
	got := map[string]http.Header{}
	record := func(name string, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got[name] = r.Header.Clone()
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			record("other, first hop", w, r)
			http.Redirect(w, r, "/second", http.StatusFound)
			return
		}
		record("other, second hop", w, r)
	}))
	defer other.Close()
	system := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/away" {
			http.Redirect(w, r, other.URL+"/first", http.StatusFound)
			return
		}
		record("system", w, r)
	}))
	defer system.Close()

	type userKey struct{}
	client, err := New(Config{
		Headers: map[string]string{"X-Api-Key": "key"},
		ContextHeaders: func(ctx context.Context) http.Header {
			return http.Header{"Authorization": {ctx.Value(userKey{}).(string)}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), userKey{}, "Bearer user")
	for _, url := range []string{system.URL, system.URL + "/away"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if h := got["system"]; h.Get("X-Api-Key") != "key" || h.Get("Authorization") != "Bearer user" {
		t.Errorf("the system got %v", h)
	}
	for _, hop := range []string{"other, first hop", "other, second hop"} {
		if h := got[hop]; h == nil || h.Get("X-Api-Key") != "" || h.Get("Authorization") != "" {
			t.Errorf("%s: got %v", hop, h)
		}
	}
}
