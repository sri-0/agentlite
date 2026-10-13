package transport

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/youmark/pkcs8"
)

// clientCertificate pairs a certificate with its private key, decrypting the
// key when it is encrypted. The key is handed to the standard library
// re-encoded, so tls.X509KeyPair still checks that the two belong together.
func clientCertificate(certFile, keyFile string, password []byte) (tls.Certificate, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	block := privateKeyBlock(keyPEM)
	if block == nil {
		return tls.Certificate{}, fmt.Errorf("%s: no private key", keyFile)
	}
	encrypted := block.Type == "ENCRYPTED PRIVATE KEY" || x509.IsEncryptedPEMBlock(block) //nolint:staticcheck // legacy keys are still issued
	switch {
	case encrypted && len(password) == 0:
		return tls.Certificate{}, fmt.Errorf("%s: the key is encrypted and no password is configured", keyFile)
	case !encrypted && len(password) > 0:
		return tls.Certificate{}, fmt.Errorf("%s: a password is configured but the key is not encrypted", keyFile)
	case !encrypted:
		return tls.X509KeyPair(certPEM, keyPEM)
	}
	key, err := decrypt(block, password)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s: %w", keyFile, err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// privateKeyBlock is the first PEM block holding a private key.
func privateKeyBlock(data []byte) *pem.Block {
	for {
		block, rest := pem.Decode(data)
		if block == nil || strings.HasSuffix(block.Type, "PRIVATE KEY") {
			return block
		}
		data = rest
	}
}

func decrypt(block *pem.Block, password []byte) (any, error) {
	if block.Type == "ENCRYPTED PRIVATE KEY" {
		return pkcs8.ParsePKCS8PrivateKey(block.Bytes, password)
	}
	// Legacy PEM encryption: deprecated for its padding oracle, which needs
	// an attacker to probe decryption online; a key file read at start
	// offers none.
	der, err := x509.DecryptPEMBlock(block, password) //nolint:staticcheck // see above
	if err != nil {
		return nil, err
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	return nil, errors.New("unsupported private key type")
}
