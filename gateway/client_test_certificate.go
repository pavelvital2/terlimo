package main

import (
	"crypto/tls"
	"errors"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"os"
	"path/filepath"
)

func clientTestCertificate() (tls.Certificate, error) {
	if os.Getenv("WL_TEST_ENABLED") != "1" {
		return selfsign.GenerateSelfSigned()
	}
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return tls.Certificate{}, errors.New("TEST_CERTIFICATE_MISSING")
	}
	certPath := filepath.Join(dir, "wl-test-dtls-cert")
	keyPath := filepath.Join(dir, "wl-test-dtls-key")
	keyInfo, e := os.Stat(keyPath)
	if e != nil || keyInfo.Mode().Perm()&0077 != 0 {
		return tls.Certificate{}, errors.New("TEST_CERTIFICATE_PERMISSIONS")
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}
