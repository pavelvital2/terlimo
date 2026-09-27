package main

// Pre-admission first-connect tests: the real mobile feed fixture (in-process HTTP
// account-access backend), the real bridge reader, the real onboarding flow/client and
// a node-compatible in-process bootstrap listener. They prove the fresh-install path
// without any catalogue: the attempt stays alive, only the explicit after-consent
// command reaches the intent/start RPC, the request_key is durably persisted before
// the first network call, and the started hour then resumes the normal catalog path.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wg-turn-client/onboarding"
	"wg-turn-client/servicechannel"
	"wg-turn-client/wlwire"
)

// TestBridgeExplicitConnectCommand proves the separate pre-admission command reaches
// the runner through the real bridge reader, and that lifecycle frames never arm it.
func TestBridgeExplicitConnectCommand(t *testing.T) {
	b := newManagedBridge(io.Discard, "attempt", func() {})
	b.read(context.Background(), bufio.NewScanner(strings.NewReader(
		`{"v":1,"attempt_id":"attempt","type":"device_wake","lifecycle_revision":2}`+"\n"+
			`{"v":1,"attempt_id":"attempt","type":"explicit_connect"}`+"\n")))
	select {
	case <-b.explicit:
	default:
		t.Fatal("explicit_connect must reach the pre-admission runner channel")
	}
	if len(b.explicit) != 0 {
		t.Fatal("explicit_connect must be coalesced")
	}
	b.read(context.Background(), bufio.NewScanner(strings.NewReader(
		`{"v":1,"attempt_id":"attempt","type":"device_sleep","lifecycle_revision":3}`+"\n"+
			`{"v":1,"attempt_id":"attempt","type":"device_wake","lifecycle_revision":4}`+"\n")))
	select {
	case <-b.explicit:
		t.Fatal("background lifecycle must never arm the explicit first connect")
	default:
	}
}

// preAdmissionBackend serves the ready intent shape around the mobile feed fixture and
// records whether the durable persist already happened when the intent was called. A
// requested gateway_key is echoed as the assigned intent gateway, so the immutable
// gateway pairing of the explicit Connect can be asserted end-to-end.
type preAdmissionBackend struct {
	fixture   *mobileFeedFixture
	host      *feedHost
	intents   int32
	lastKey   chan string
	startedCh chan struct{}
}

const preAdmissionIntentID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

const preAdmissionDefaultGateway = "terlimo-035-node"

func (b *preAdmissionBackend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/mobile/v1/onboarding/intents", func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		proof, _ := body["proof"].(map[string]any)
		signed, _ := proof["signed_payload_b64"].(string)
		raw, err := base64.RawURLEncoding.DecodeString(signed)
		if err != nil {
			http.Error(writer, "bad proof", http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if json.Unmarshal(raw, &payload) != nil || payload["op"] != onboarding.IntentOp {
			http.Error(writer, "bad payload", http.StatusBadRequest)
			return
		}
		requestKey, _ := payload["request_key"].(string)
		if requestKey == "" {
			http.Error(writer, "missing request key", http.StatusBadRequest)
			return
		}
		gatewayID := preAdmissionDefaultGateway
		if selected, _ := payload["gateway_key"].(string); selected != "" {
			gatewayID = selected
		}
		if b.host.persistCount() < 1 {
			t := b.fixture.t
			t.Errorf("intent called before the durable request_key persist")
		}
		atomic.AddInt32(&b.intents, 1)
		b.lastKey <- requestKey
		now := time.Now().UTC()
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"request_id":     "1f2e3d4c5b6a79880716253443526170",
			"server_time":    now.Format("2006-01-02T15:04:05.999999+00:00"),
			"schema_version": "0001_core", "status": "ready", "state": "ready",
			"intent_id": preAdmissionIntentID, "request_key": requestKey,
			"expires_at": now.Add(10 * time.Minute).Format("2006-01-02T15:04:05.999999+00:00"),
			"gateway": map[string]any{"node_id": gatewayID,
				"endpoint": map[string]any{"peer_ip": "127.0.0.1", "dtls_port": 56002,
					"dtls_spki_sha256": "Xw6MLz1LWml4h5altMPS4fChssPU5fYHGCk6S1xtfo8"}},
			"credential_id": "0d1c2b3a-4f5e-6978-8796-a5b4c3d2e1f0",
			"bootstrap": map[string]any{"credential_id": "0d1c2b3a-4f5e-6978-8796-a5b4c3d2e1f0",
				"secret": "ready-secret"},
			"start_challenge": map[string]any{"challenge_id": "00112233445566778899aabbccddeeff",
				"nonce_b64":  base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
				"expires_at": now.Add(5 * time.Minute).Format("2006-01-02T15:04:05.999999+00:00")},
		})
	})
	mux.Handle("/", b.fixture.handler())
	return mux
}

// preAdmissionListener speaks the node handler protocol: it reassembles one WLBS
// frame and answers with the backend golden ok-reply on the same tunnel id. It is
// the in-process stand-in for the trusted assigned-gateway bootstrap.
type preAdmissionListener struct {
	ln       net.Listener
	received chan []byte
	session  string
	starts   int32
}

func newPreAdmissionListener(t *testing.T) *preAdmissionListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	listener := &preAdmissionListener{ln: ln, received: make(chan []byte, 8), session: hex.EncodeToString(raw)}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var assembler wlwire.Assembler
				frameID, body, err := readPreAdmissionFrame(conn, &assembler)
				if err != nil {
					return
				}
				listener.received <- append([]byte(nil), body...)
				atomic.AddInt32(&listener.starts, 1)
				now := time.Now().UTC()
				reply, err := json.Marshal(map[string]any{
					"status": "ok", "state": "started", "replay": false,
					"intent_id":     preAdmissionIntentID,
					"credential_id": "0d1c2b3a-4f5e-6978-8796-a5b4c3d2e1f0",
					"started_at":    now.Format("2006-01-02T15:04:05.999999+00:00"),
					"not_after":     now.Add(time.Hour).Format("2006-01-02T15:04:05.999999+00:00"),
					"message_hash":  strings.Repeat("ab", 32),
				})
				if err != nil {
					return
				}
				frames, err := wlwire.Frames(frameID, true, reply)
				if err != nil {
					return
				}
				for _, frame := range frames {
					if _, err := conn.Write(frame); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return listener
}

// readPreAdmissionFrame reads exactly one header-declared WLBS frame (32-byte header
// plus min(remaining, Fragment) payload) from a TCP stream. A localhost read may split
// or coalesce frames, so the frame boundary comes from the header, never from one read:
// parsing a partial read as a whole frame intermittently reset the connection and made
// the pre-admission test flaky at the base revision.
func readPreAdmissionFrame(conn net.Conn, assembler *wlwire.Assembler) (wlwire.ID, []byte, error) {
	header := make([]byte, 32)
	for {
		if _, err := io.ReadFull(conn, header); err != nil {
			return wlwire.ID{}, nil, err
		}
		if string(header[:4]) != "WLBS" {
			return wlwire.ID{}, nil, wlwire.ErrMessage
		}
		total := int(binary.BigEndian.Uint32(header[24:28]))
		off := int(binary.BigEndian.Uint32(header[28:32]))
		size := total - off
		if size > wlwire.Fragment {
			size = wlwire.Fragment
		}
		if size < 1 {
			return wlwire.ID{}, nil, wlwire.ErrMessage
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return wlwire.ID{}, nil, err
		}
		id, body, err := assembler.Add(append(header, payload...), time.Now())
		if err != nil || body != nil {
			return id, body, err
		}
	}
}

func (l *preAdmissionListener) dialer() onboarding.BootstrapDialer {
	return func(ctx context.Context, _ onboarding.GatewayEndpoint, _ onboarding.Bootstrap) (net.Conn, func(), error) {
		conn, err := net.Dial("tcp", l.ln.Addr().String())
		if err != nil {
			return nil, nil, err
		}
		return conn, func() { _ = conn.Close() }, nil
	}
}

// TestPreAdmissionExplicitConnectWithoutCatalog drives the whole pre-admission path:
// a fresh installation with no data right and no catalogue stays alive, ignores
// background lifecycle, and only the explicit after-consent command performs the
// real intent/start RPC; the started hour then resumes the normal /me → catalog path.
func TestPreAdmissionExplicitConnectWithoutCatalog(t *testing.T) {
	fixture := newMobileFeedFixture(t, 0, mobileFeedOptions{dataAccess: "none"})
	backend := &preAdmissionBackend{fixture: fixture, lastKey: make(chan string, 4), startedCh: make(chan struct{}, 1)}
	server := httptest.NewServer(backend.handler())
	defer server.Close()
	start := mobileFeedStart(t, fixture, server, "gw-0")
	listener := newPreAdmissionListener(t)
	previousDial := onboardingBootstrapDial
	onboardingBootstrapDial = func(*servicechannel.Store) onboarding.BootstrapDialer { return listener.dialer() }
	defer func() { onboardingBootstrapDial = previousDial }()
	controller, _, host, cancel, errCh := newMobileFeedRun(t, fixture, start)
	backend.host = host

	// The first verified no-right snapshot must not end the attempt.
	feedWaitFor(t, func() bool {
		me, _ := fixture.calls()
		return me > 0
	})
	select {
	case err := <-errCh:
		t.Fatalf("pre-admission state must not end the mobile attempt: %v", err)
	default:
	}

	// Background lifecycle is not the explicit user funnel.
	host.send(t, map[string]any{"type": "device_wake", "lifecycle_revision": 1})
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&backend.intents) != 0 {
		t.Fatal("background lifecycle must never begin an onboarding intent")
	}

	// The explicit after-consent command reaches intent/start through the real path.
	host.send(t, map[string]any{"type": "explicit_connect"})
	var requestKey string
	select {
	case requestKey = <-backend.lastKey:
	case <-time.After(5 * time.Second):
		t.Fatal("explicit connect never reached the intent endpoint")
	}
	if requestKey == "" {
		t.Fatal("intent request_key empty")
	}
	select {
	case body := <-listener.received:
		var startBody map[string]any
		if json.Unmarshal(body, &startBody) != nil || startBody["op"] != onboarding.StartOp ||
			startBody["request_key"] != requestKey || startBody["intent_id"] != preAdmissionIntentID {
			t.Fatalf("start RPC body wrong: %s", body)
		}
		if _, ok := startBody["proof"].(map[string]any); !ok {
			t.Fatalf("start RPC must carry the signed proof: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start RPC never reached the assigned gateway bootstrap")
	}
	// Repeat tap after `started` stays idempotent: the recorded hour returns without a
	// second network call and without a new request_key.
	host.send(t, map[string]any{"type": "explicit_connect"})
	select {
	case repeat := <-backend.lastKey:
		t.Fatalf("repeated tap after started must not create a new intent: %q", repeat)
	case <-time.After(500 * time.Millisecond):
	}
	select {
	case err := <-errCh:
		t.Fatalf("idempotent repeat must not end the attempt: %v", err)
	default:
	}

	// Normal path resumes: a later verified hour publishes the catalogue.
	now := time.Now().UTC()
	fixture.setMe(mobileFeedMeBody(mobileFeedOptions{dataAccess: "onboarding_hour", deadline: time.Hour, revision: "9"}, now))
	gateway := mobileFeedGatewayBody(fixture.fingerprint, 0, fixture.validUntil, mobileFeedOptions{})
	fixture.setCatalog("9", []map[string]any{gateway})
	controller.mobile.runner.Trigger("manual")
	host.waitMessage(t, "catalog")
	cancel()
	<-errCh
}
