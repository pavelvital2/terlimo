package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
	"wg-turn-client/internal/wlwire"
)

// Local synthetic gateway: checks deployed relay framing, not database/auth integration.
func TestCatalogResponseLocalDTLS(t *testing.T) {
	t.Setenv("WL_TEST_ENABLED", "1")
	old := db
	b := ClientTestBootstrap{CredentialID: "fixture", Secret: "synthetic-local-only", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	db = &Database{ClientBootstrap: map[string]ClientTestBootstrap{"fixture": b}}
	defer func() { db = old }()
	path := filepath.Join(t.TempDir(), "gateway.sock")
	t.Setenv("WL_TEST_BACKEND_SOCKET", path)
	unix, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	bodies := [][]byte{[]byte(`{"v":1,"status":"ok","nodes":[]}`), []byte(`{"v":1,"status":"ok","catalog":{"nodes":[]}}`), []byte(`{"v":1,"status":"not_modified","revision":"11"}`)}
	// Include a fragmented full catalog response near the gateway bound.
	big, _ := json.Marshal(map[string]any{"v": 1, "status": "ok", "catalog": map[string]any{"synthetic_padding": string(bytes.Repeat([]byte("x"), 15000))}})
	bodies = append(bodies, big)
	go func() {
		for _, body := range bodies {
			u, e := unix.Accept()
			if e != nil {
				return
			}
			var envelope map[string]any
			_ = json.NewDecoder(u).Decode(&envelope)
			_, _ = u.Write(append(body, '\n'))
			_ = u.Close()
		}
	}()
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := dtls.Listen("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &dtls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		if c.(*dtls.Conn).HandshakeContext(ctx) != nil {
			return
		}
		clientTestBootstrapServe(ctx, c, b)
	}()
	// Synthetic self-signed test endpoint only; production trust is not changed.
	c, err := dtls.Dial("udp4", listener.Addr().(*net.UDPAddr), &dtls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	for n, want := range bodies {
		var id wlwire.ID
		id[0] = byte(n + 1)
		fs, e := wlwire.Frames(id, false, []byte(`{"v":1,"op":"catalog"}`))
		if e != nil {
			t.Fatal(e)
		}
		for _, f := range fs {
			if _, e = c.Write(f); e != nil {
				t.Fatal(e)
			}
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		var a wlwire.Assembler
		buf := make([]byte, 2048)
		for {
			n, e := c.Read(buf)
			if e != nil {
				t.Fatal(e)
			}
			if n < 6 || buf[5] != 1 {
				t.Fatal("response flag")
			}
			gotID, body, e := a.Add(buf[:n], time.Now())
			if e != nil {
				t.Fatal(e)
			}
			if body != nil {
				if gotID != id || !bytes.Equal(body, want) {
					t.Fatal("response mismatch")
				}
				break
			}
		}
	}
	_ = c.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(io.ErrNoProgress)
	}
}
