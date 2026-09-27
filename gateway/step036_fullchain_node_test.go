package main

// Isolated STEP036 node process. It uses the production admin socket, WRAP/DTLS
// listener, service handler, and bootstrap handler. Credentials arrive only by
// the backend worker through bootstrap_provision; this file never seeds them.
import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

func step036NodeErrorClass(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, io.EOF) {
		return "EOF"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "net_timeout"
	}
	return "other"
}

type step036NodeTraceConn struct {
	net.Conn
	t *testing.T
}

func (c *step036NodeTraceConn) Read(buf []byte) (int, error) {
	n, err := c.Conn.Read(buf)
	c.t.Logf("node handler Read: n=%d class=%s", n, step036NodeErrorClass(err))
	return n, err
}

func (c *step036NodeTraceConn) Write(buf []byte) (int, error) {
	n, err := c.Conn.Write(buf)
	c.t.Logf("node handler Write: n=%d class=%s", n, step036NodeErrorClass(err))
	return n, err
}

func TestStep036FullchainNode(t *testing.T) {
	dir := os.Getenv("STEP036_NODE_DIR")
	if dir == "" {
		t.Skip("STEP036_NODE_DIR not set")
	}
	mainPassword := os.Getenv("STEP036_NODE_MAIN_PASSWORD")
	evidenceSocket := os.Getenv("STEP036_EVIDENCE_SOCKET")
	serviceSocket := os.Getenv("STEP036_SERVICE_SOCKET")
	serviceSeed := os.Getenv("STEP036_SERVICE_SEED")
	if mainPassword == "" || evidenceSocket == "" || serviceSocket == "" || serviceSeed == "" {
		t.Fatal("fullchain node configuration incomplete")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WL_TEST_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_ENABLED", "1")
	t.Setenv("WL_TEST_SERVICE_SEED", serviceSeed)
	t.Setenv("WL_TEST_BACKEND_SOCKET", evidenceSocket)
	t.Setenv("WL_TEST_SERVICE_BACKEND_SOCKET", serviceSocket)

	oldDB, oldKeys := db, serverWrapKeys
	db = &Database{MainPassword: mainPassword, Passwords: map[string]*PasswordEntry{},
		Devices: map[string]*ClientDevice{}, ClientBootstrap: map[string]ClientTestBootstrap{}}
	serverWrapKeys = newWrapKeyStore()
	t.Cleanup(func() { db, serverWrapKeys = oldDB, oldKeys })
	if err := serverWrapKeys.SetServiceClassifier(serviceSeed); err != nil {
		t.Fatal(err)
	}
	if err := refreshWrapKeysFromDBLocked(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := startAdminSocket(ctx, dir, nil); err != nil {
		t.Fatal(err)
	}

	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := listenWrapped(addr, serverWrapKeys)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := dtls.NewListenerWithOptions(wrapped,
		dtls.WithCertificates(cert),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256),
		dtls.WithConnectionIDGenerator(dtls.RandomCIDGenerator(8)))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pinHash := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	pin := base64.RawURLEncoding.EncodeToString(pinHash[:])
	port := listener.Addr().(*net.UDPAddr).Port
	manifest := struct {
		Port        int    `json:"dtls_port"`
		Pin         string `json:"dtls_spki_sha256"`
		AdminSocket string `json:"admin_socket"`
	}{port, pin, adminSocketPath(dir)}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node-ready.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	go func() {
		stop := filepath.Join(dir, "stop")
		for {
			if _, err := os.Stat(stop); err == nil {
				_ = listener.Close()
				return
			}
			select {
			case <-ctx.Done():
				_ = listener.Close()
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			dtlsConn, ok := c.(*dtls.Conn)
			if !ok || dtlsConn.HandshakeContext(ctx) != nil {
				t.Log("node handshake_success=false")
				return
			}
			t.Log("node handshake_success=true")
			identity, ok := wrappedIdentity(c.RemoteAddr())
			t.Logf("node wrapped_identity_found=%t is_service=%t", ok, identity.isService)
			if !ok {
				return
			}
			if identity.isService {
				t.Log("node service_handler_enter")
				clientTestServiceServe(ctx, &step036NodeTraceConn{Conn: c, t: t}, identity)
				t.Log("node service_handler_exit")
				return
			}
			bootstrap, ok := clientTestBootstrapFor(identity)
			if !ok {
				return
			}
			clientTestBootstrapServe(ctx, c, bootstrap)
		}(conn)
	}
}
