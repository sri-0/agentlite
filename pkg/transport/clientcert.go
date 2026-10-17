package transport

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"

	"github.com/youmark/pkcs8"
)

// clientCertificate pairs a certificate with its private key, decrypting the
// key when it is encrypted. The decrypted key is handed to the standard
// library, which parses any key format and checks that the two belong
// together.
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
	//lint:ignore SA1019 legacy encrypted keys are still issued
	encrypted := block.Type == "ENCRYPTED PRIVATE KEY" || x509.IsEncryptedPEMBlock(block)
	switch {
	case encrypted && len(password) == 0:
		return tls.Certificate{}, fmt.Errorf("%s: the key is encrypted and no password is configured", keyFile)
	case !encrypted && len(password) > 0:
		return tls.Certificate{}, fmt.Errorf("%s: a password is configured but the key is not encrypted", keyFile)
	case !encrypted:
		return tls.X509KeyPair(certPEM, keyPEM)
	}
	der, err := decrypt(block, password)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s: %w", keyFile, err)
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

// decrypt returns the key's DER: PKCS#8 from an encrypted PKCS#8 block, or
// what legacy PEM encryption wrapped (PKCS#1 or SEC 1).
func decrypt(block *pem.Block, password []byte) ([]byte, error) {
	if block.Type != "ENCRYPTED PRIVATE KEY" {
		// Legacy PEM encryption: deprecated for its padding oracle, which
		// needs an attacker to probe decryption online; a key file read at
		// start offers none.
		//lint:ignore SA1019 see above
		return x509.DecryptPEMBlock(block, password)
	}
	key, err := pkcs8.ParsePKCS8PrivateKey(block.Bytes, password)
	if err != nil {
		return nil, err
	}
	return x509.MarshalPKCS8PrivateKey(key)
}
