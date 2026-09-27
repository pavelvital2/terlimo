package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cbeuw/connutil"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"wg-turn-client/wlbs"
)

// No sockets: both observed and unobserved RPCs see the same record/error path.
type diagnosticTestConn struct {
	reads, writes, closes int
	wire                  [][]byte
	read                  func() error
}

func (c *diagnosticTestConn) Read([]byte) (int, error) { c.reads++; return 0, c.read() }
func (c *diagnosticTestConn) Write(p []byte) (int, error) {
	c.writes++
	c.wire = append(c.wire, append([]byte(nil), p...))
	return len(p), nil
}
func (c *diagnosticTestConn) Close() error                     { c.closes++; return nil }
func (c *diagnosticTestConn) LocalAddr() net.Addr              { return nil }
func (c *diagnosticTestConn) RemoteAddr() net.Addr             { return nil }
func (c *diagnosticTestConn) SetDeadline(time.Time) error      { return nil }
func (c *diagnosticTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *diagnosticTestConn) SetWriteDeadline(time.Time) error { return nil }

func TestBootstrapDiagnosticEOFAndCancellationTransparent(t *testing.T) {
	for _, mode := range []string{"remote_like_eof", "local_transport_cancel", "caller_cancel"} {
		t.Run(mode, func(t *testing.T) {
			var baselineWire [][]byte
			for _, observed := range []bool{false, true} {
				ctx, cancel := context.WithCancel(context.Background())
				transport, stop := context.WithCancel(ctx)
				d := newBootstrapDiagnostic(ctx)
				d.setTransport(transport)
				d.setStage("HANDSHAKE", true)
				conn := &diagnosticTestConn{read: func() error {
					switch mode {
					case "local_transport_cancel":
						d.noteClose("RELAY_READ", nil)
						stop()
					case "caller_cancel":
						cancel()
					}
					return io.EOF
				}}
				rpc := &wlbs.RPC{Conn: conn}
				if observed {
					rpc.ObserveIO = d.observeIO
				}
				_, err := rpc.Call(ctx, wlbs.ID{}, []byte(`{"v":1,"op":"challenge"}`))
				if mode == "caller_cancel" {
					if !errors.Is(err, context.Canceled) {
						t.Fatal("caller outcome changed", err)
					}
				} else if err != io.EOF {
					t.Fatal("EOF outcome changed", err)
				}
				if conn.reads != 1 || conn.writes != 1 {
					t.Fatal("retry/IO behavior changed")
				}
				if !observed {
					baselineWire = conn.wire
				} else {
					if !reflect.DeepEqual(baselineWire, conn.wire) {
						t.Fatal("wire changed")
					}
					m := d.finish(err)
					if m["operation"] != "CHALLENGE" || m["io_stage"] != "READ" || m["handshake_pin_ok"] != true || m["error_class"] != "EOF" {
						t.Fatal("lost first IO evidence")
					}
					wantClose := "UNKNOWN"
					if mode == "local_transport_cancel" {
						wantClose = "RELAY_READ"
					}
					if mode == "caller_cancel" {
						wantClose = "CALLER_CANCEL"
					}
					if m["first_close"] != wantClose || m["caller_canceled"] != (mode == "caller_cancel") || m["transport_canceled"] != (mode != "remote_like_eof") {
						t.Fatal("close/cancel not distinguished", m)
					}
					before, _ := json.Marshal(m)
					cancel()
					stop()
					d.noteClose("CLEANUP", errors.New("secret-error"))
					d.setStage("COMPLETE", true)
					after, _ := json.Marshal(d.finish(context.Canceled))
					if string(before) != string(after) {
						t.Fatal("cleanup overwrote first error")
					}
				}
				cancel()
				stop()
			}
		})
	}
}

func TestBootstrapDiagnosticBoundedAndRedacted(t *testing.T) {
	d := newBootstrapDiagnostic(context.Background())
	d.observeIO("READ", "https://private.invalid/credential", errors.New("private address token key"))
	m := d.finish(io.EOF)
	if m["operation"] != "UNKNOWN" || m["error_class"] != "OTHER" {
		t.Fatal("allowlist failed")
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "private") || len(raw) > 700 {
		t.Fatal("unbounded or private diagnostic")
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.noteClose("CLEANUP", io.EOF)
			d.observeIO("WRITE", "catalog", nil)
			d.finish(nil)
		}()
	}
	wg.Wait()
	if d.finish(nil)["error_class"] != "OTHER" {
		t.Fatal("later cause replaced first")
	}
}

func TestBootstrapDiagnosticCleanupBeforeEmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newBootstrapDiagnostic(ctx)
	d.observeIO("READ", "catalog", io.EOF)
	order := []string{}
	d.complete(io.EOF, func() { order = append(order, "cleanup"); cancel(); d.noteClose("CLEANUP", context.Canceled) }, func(m bridgeMessage) error {
		order = append(order, "emit")
		if m["error_class"] != "EOF" || m["caller_canceled"] != false || m["first_close"] != "UNKNOWN" {
			t.Fatal("cleanup changed original observation")
		}
		return errors.New("ignored observer output failure")
	})
	if !reflect.DeepEqual(order, []string{"cleanup", "emit"}) {
		t.Fatal("cleanup moved behind IO")
	}
}

func TestBootstrapDiagnosticRealDTLSRemoteCloseStaysUnknown(t *testing.T) {
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	verify, err := managedPinVerifier(base64.RawURLEncoding.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	a, b := connutil.AsyncPacketPipe()
	defer a.Close()
	defer b.Close()
	peer := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}
	server, err := dtls.Server(a, peer, &dtls.Config{Certificates: []tls.Certificate{cert}, ExtendedMasterSecret: dtls.RequireExtendedMasterSecret, LoggerFactory: &NullLoggerFactory{}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := dtls.Client(b, peer, &dtls.Config{InsecureSkipVerify: true, VerifyPeerCertificate: verify, ExtendedMasterSecret: dtls.RequireExtendedMasterSecret, LoggerFactory: &NullLoggerFactory{}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := make(chan error, 1)
	go func() { ready <- server.HandshakeContext(ctx) }()
	if err = client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-ready; err != nil {
		t.Fatal(err)
	}
	d := newBootstrapDiagnostic(ctx)
	d.setTransport(ctx)
	d.setStage("HANDSHAKE", true)
	closed := make(chan error, 1)
	go func() {
		_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, e := server.Read(make([]byte, 2048))
		if e == nil {
			e = server.Close()
		}
		closed <- e
	}()
	_, err = (&wlbs.RPC{Conn: client, ObserveIO: d.observeIO}).Call(ctx, wlbs.ID{}, []byte(`{"v":1,"op":"challenge"}`))
	// Pion's close notification and in-memory pipe teardown race. Both are
	// terminal read outcomes; neither proves a remote application close reason.
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("remote DTLS close outcome", err)
	}
	readErr := err
	if err = <-closed; err != nil {
		t.Fatal(err)
	}
	m := d.finish(readErr)
	if m["first_close"] != "UNKNOWN" || m["caller_canceled"] != false || m["transport_canceled"] != false || m["handshake_pin_ok"] != true {
		t.Fatal("invented peer evidence", m)
	}
}
