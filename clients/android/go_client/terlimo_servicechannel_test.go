package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cbeuw/connutil"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/turn/v5"

	"wg-turn-client/servicechannel"
	"wg-turn-client/wlwire"
)

const (
	localTURNUser     = "local-turn-user"
	localTURNPassword = "local-turn-password"
	localServiceRate  = 10 * time.Second
)

// TestMobileTransportBaselineAndServiceSeed proves the single wiring point: without a
// service seed the accepted HTTPS client is used unchanged, a configured seed routes
// everything through the service Doer, a durable state without the packaged builtin
// seed fails closed instead of falling back to direct HTTPS, and an invalid seed
// fails at wiring.
func TestMobileTransportBaselineAndServiceSeed(t *testing.T) {
	baseline, err := newMobileTransport(managedStart{MobileBaseURL: "https://mobile.invalid"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpsClient, ok := baseline.(*http.Client)
	if !ok || httpsClient.Timeout != mobileHTTPTimeout {
		t.Fatalf("legacy HTTPS baseline changed: %#v", baseline)
	}

	seed := servicechannel.Seed{
		Version:           1,
		Revision:          "1",
		Environment:       "test",
		PeerIP:            "192.0.2.7",
		DTLSPort:          56000,
		DTLSSPKISHA256:    base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		ServiceClassifier: "synthetic-public-classifier",
		VKHashes:          []string{"synthetic-hash"},
		StreamID:          0,
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	service, err := newMobileTransport(managedStart{
		MobileBaseURL:       "https://mobile.invalid",
		MobileEnvironment:   "test",
		ServiceSeed:         string(raw),
		ServiceSeedStateB64: base64.RawURLEncoding.EncodeToString([]byte(`{"namespace":"service_seed_v1","version":1}`)),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := service.(*servicechannel.Doer); !ok {
		t.Fatalf("service seed did not select the service Doer: %#v", service)
	}

	// A durable service state alone is a configuration error: never HTTPS fallback.
	if _, err := newMobileTransport(managedStart{MobileBaseURL: "https://mobile.invalid", ServiceSeedStateB64: base64.RawURLEncoding.EncodeToString([]byte(`{}`))}, nil); !errors.Is(err, servicechannel.ErrSeedMissing) {
		t.Fatalf("state-only service configuration fell back: %v", err)
	}
	if _, err := newMobileTransport(managedStart{MobileBaseURL: "https://mobile.invalid", ServiceSeed: `{"version":9}`}, nil); !errors.Is(err, servicechannel.ErrSeedInvalid) {
		t.Fatalf("invalid service seed accepted: %v", err)
	}
	if _, err := newMobileTransport(managedStart{MobileBaseURL: "http://mobile.invalid", ServiceSeed: string(raw)}, nil); !errors.Is(err, servicechannel.ErrOriginRejected) {
		t.Fatalf("plaintext service base url accepted: %v", err)
	}
}

// TestManagedServiceEstablishLocalRoundTrip is the real local pinned DTLS+WRAP round
// trip through the production establishment and the production channel: a local TURN
// server, a local in-process node listener with the same pin/classifier material, the
// accepted service classifier semantics (deriveWrapKey + the shared service codec),
// and no catalog, grant, probe-map, admission or live network. The mobile Doer is the
// only component above the transport, and it works before any catalog exists.
func TestManagedServiceEstablishLocalRoundTrip(t *testing.T) {
	classifier := "public-service-round-trip"
	peerPort, pin, node := startLocalServiceNode(t, classifier)
	turnURL := startLocalTURNServer(t)
	hash := "round-trip-hash"
	seedLocalServiceCreds(t, hash, turnURL)

	seed := roundTripSeed(peerPort, pin, classifier, hash)
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := newMobileTransport(managedStart{
		MobileBaseURL:     "https://mobile.invalid",
		MobileEnvironment: "test",
		ServiceSeed:       string(raw),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	doer, ok := transport.(*servicechannel.Doer)
	if !ok {
		t.Fatalf("service seed did not select the service Doer: %#v", transport)
	}
	request, err := http.NewRequest("GET", "https://mobile.invalid/api/mobile/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doer.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || string(body) != `{"ok":true}` {
		t.Fatalf("service round trip changed: status=%d body=%q", response.StatusCode, body)
	}
	if node.served.Load() != 1 {
		t.Fatalf("local node did not serve exactly one exchange: %d", node.served.Load())
	}
	if node.unwraps.Load() == 0 || node.rejected.Load() != 0 {
		t.Fatalf("local node wrap counters are wrong: unwraps=%d rejected=%d", node.unwraps.Load(), node.rejected.Load())
	}
	if node.handshakes.Load() != 1 {
		t.Fatalf("local node did not complete exactly one DTLS handshake: %d", node.handshakes.Load())
	}
}

// TestManagedServiceEstablishWrongPinRejected proves a wrong pin fails TRUST_FAILED.
func TestManagedServiceEstablishWrongPinRejected(t *testing.T) {
	classifier := "public-service-wrong-pin"
	peerPort, _, _ := startLocalServiceNode(t, classifier)
	turnURL := startLocalTURNServer(t)
	hash := "wrong-pin-hash"
	seedLocalServiceCreds(t, hash, turnURL)

	other, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(other.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	seed := roundTripSeed(peerPort, base64.RawURLEncoding.EncodeToString(digest[:]), classifier, hash)

	ctx, cancel := context.WithTimeout(context.Background(), localServiceRate)
	defer cancel()
	_, cleanup, err := managedServiceEstablish(ctx, seed)
	if cleanup != nil {
		cleanup()
	}
	if err == nil || err.Error() != "TRUST_FAILED" {
		t.Fatalf("wrong pin not rejected with TRUST_FAILED: %v", err)
	}
}

// TestManagedServiceEstablishWrongWrapKeyRejected proves the public classifier is the
// only accepted WRAP key source: a wrong classifier never completes the node
// handshake, the local node rejects every wrapped packet, and the establishment fails
// closed without silently using another key or transport.
func TestManagedServiceEstablishWrongWrapKeyRejected(t *testing.T) {
	peerPort, pin, node := startLocalServiceNode(t, "public-service-right-key")
	turnURL := startLocalTURNServer(t)
	hash := "wrong-key-hash"
	seedLocalServiceCreds(t, hash, turnURL)

	seed := roundTripSeed(peerPort, pin, "public-service-WRONG-key", hash)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, cleanup, err := managedServiceEstablish(ctx, seed)
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("wrong wrap key completed the service handshake")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wrong wrap key failed with an unexpected error: %v", err)
	}
	if node.unwraps.Load() != 0 || node.rejected.Load() == 0 {
		t.Fatalf("local node did not reject the wrong key: unwraps=%d rejected=%d", node.unwraps.Load(), node.rejected.Load())
	}
	if node.handshakes.Load() != 0 {
		t.Fatal("wrong wrap key reached the node DTLS handshake")
	}
}

// TestManagedServiceEstablishCleanupTerminal proves the returned cleanup closes the
// service connection terminally and is safe to call more than once.
func TestManagedServiceEstablishCleanupTerminal(t *testing.T) {
	classifier := "public-service-cleanup"
	peerPort, pin, node := startLocalServiceNode(t, classifier)
	turnURL := startLocalTURNServer(t)
	hash := "cleanup-hash"
	seedLocalServiceCreds(t, hash, turnURL)

	seed := roundTripSeed(peerPort, pin, classifier, hash)
	ctx, cancel := context.WithTimeout(context.Background(), localServiceRate)
	defer cancel()
	conn, cleanup, err := managedServiceEstablish(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	if conn == nil || cleanup == nil {
		t.Fatal("establishment returned no connection or cleanup")
	}
	if node.handshakes.Load() != 1 {
		t.Fatal("establishment did not complete the pinned handshake")
	}
	cleanup()
	if _, err := conn.Write([]byte("after-cleanup")); err == nil {
		t.Fatal("cleanup left the service connection writable")
	}
	cleanup()
}

// TestManagedServiceEstablishCanceledContextStops proves a canceled caller context
// stops the establishment before any dial, with the fixed product timeouts unchanged.
func TestManagedServiceEstablishCanceledContextStops(t *testing.T) {
	if mobileHTTPTimeout != 15*time.Second || wrapHandshakeTimeout != 8*time.Second {
		t.Fatalf("fixed product timeouts changed: http=%v handshake=%v", mobileHTTPTimeout, wrapHandshakeTimeout)
	}
	classifier := "public-service-canceled"
	peerPort, pin, node := startLocalServiceNode(t, classifier)
	turnURL := startLocalTURNServer(t)
	hash := "canceled-hash"
	seedLocalServiceCreds(t, hash, turnURL)

	seed := roundTripSeed(peerPort, pin, classifier, hash)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, cleanup, err := managedServiceEstablish(ctx, seed)
	if cleanup != nil {
		cleanup()
	}
	if conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled establishment did not stop: conn=%v err=%v", conn, err)
	}
	if node.handshakes.Load() != 0 || node.unwraps.Load() != 0 {
		t.Fatal("canceled establishment still reached the node")
	}
}

type localServiceNode struct {
	conn       *net.UDPConn
	key        []byte
	handshakes atomic.Int64
	served     atomic.Int64
	unwraps    atomic.Int64
	rejected   atomic.Int64
}

// startLocalServiceNode runs the node side of the accepted service classifier
// semantics in-process: deriveWrapKey on the classifier, WRAP unwrap/wrap around a
// pinned DTLS server, and the shared service request/reply codec. No live network.
func startLocalServiceNode(t *testing.T, classifier string) (int, string, *localServiceNode) {
	t.Helper()
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	pin := base64.RawURLEncoding.EncodeToString(digest[:])
	key, err := deriveWrapKey(classifier)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	node := &localServiceNode{conn: conn, key: key}
	t.Cleanup(func() { _ = conn.Close() })
	go node.serve(cert)
	return conn.LocalAddr().(*net.UDPAddr).Port, pin, node
}

func (n *localServiceNode) serve(cert tls.Certificate) {
	pipeA, pipeB := connutil.AsyncPacketPipe()
	var peerAddr atomic.Pointer[net.UDPAddr]
	go func() {
		buf, plain := make([]byte, 2048), make([]byte, 2048)
		for {
			read, from, err := n.conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			peerAddr.Store(from)
			plainLen, err := obfsUnwrapPacket(n.key, buf[:read], plain)
			if err != nil {
				n.rejected.Add(1)
				continue
			}
			n.unwraps.Add(1)
			if _, err := pipeA.WriteTo(plain[:plainLen], from); err != nil {
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 2048)
		config, state := NewObfsConfig(), NewObfsState()
		for {
			read, _, err := pipeA.ReadFrom(buf)
			if err != nil {
				return
			}
			from := peerAddr.Load()
			if from == nil {
				continue
			}
			wrapped, err := obfsWrapPacket(n.key, buf[:read], config, state)
			if err != nil {
				return
			}
			if _, err := n.conn.WriteToUDP(wrapped, from); err != nil {
				return
			}
		}
	}()
	server, err := dtls.Server(pipeB, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}, &dtls.Config{
		Certificates:          []tls.Certificate{cert},
		ExtendedMasterSecret:  dtls.RequireExtendedMasterSecret,
		CipherSuites:          []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator: dtls.OnlySendCIDGenerator(),
		LoggerFactory:         &NullLoggerFactory{},
	})
	if err != nil {
		return
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), localServiceRate)
	defer cancel()
	if err := server.HandshakeContext(ctx); err != nil {
		return
	}
	n.handshakes.Add(1)
	n.serveService(server)
}

// serveService answers service exchanges with the accepted bounded reply shape until
// the peer closes or the deadline expires.
func (n *localServiceNode) serveService(conn *dtls.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(localServiceRate))
	defer conn.SetReadDeadline(time.Time{})
	buf := make([]byte, 32+wlwire.ServiceFragment+1)
	var assembler wlwire.ServiceAssembler
	for {
		read, err := conn.Read(buf)
		if err != nil {
			return
		}
		id, body, err := assembler.Add(buf[:read], time.Now())
		if err != nil {
			return
		}
		if body == nil {
			continue
		}
		var request struct {
			V           int               `json:"v"`
			Op          string            `json:"op"`
			SessionMode string            `json:"session_mode,omitempty"`
			RequestID   string            `json:"request_id"`
			Method      string            `json:"method"`
			Path        string            `json:"path"`
			Query       string            `json:"query"`
			Headers     map[string]string `json:"headers"`
			BodyB64     string            `json:"body_b64"`
		}
		if wlwire.StrictJSON(body, &request) != nil {
			return
		}
		if request.SessionMode != "bounded" {
			return
		}
		payload, err := json.Marshal(map[string]any{
			"v":          1,
			"request_id": request.RequestID,
			"status":     200,
			"headers":    map[string]string{"Content-Type": "application/json"},
			"body_b64":   wlwire.Encode([]byte(`{"ok":true}`)),
		})
		if err != nil {
			return
		}
		frames, err := wlwire.ServiceFrames(id, true, payload)
		if err != nil {
			return
		}
		n.served.Add(1)
		for _, frame := range frames {
			if _, err := conn.Write(frame); err != nil {
				return
			}
		}
	}
}

// startLocalTURNServer runs a loopback-only TURN server with a static relay.
func startLocalTURNServer(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := turn.NewServer(turn.ServerConfig{
		Realm:         "terlimo-local-test",
		LoggerFactory: &NullLoggerFactory{},
		AuthHandler: func(attributes *turn.RequestAttributes) (string, []byte, bool) {
			return attributes.Username, turn.GenerateAuthKey(attributes.Username, attributes.Realm, localTURNPassword), true
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: conn,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: net.IPv4(127, 0, 0, 1),
				Address:      "127.0.0.1",
			},
		}},
	})
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return fmt.Sprintf("turn:127.0.0.1:%d", conn.LocalAddr().(*net.UDPAddr).Port)
}

// seedLocalServiceCreds injects the local TURN credentials into the existing VK
// credential cache so GetCreds exercises its real hash/cache path without VK network.
func seedLocalServiceCreds(t *testing.T, hash, turnURL string) {
	t.Helper()
	cache := getStreamCache(0)
	cache.mutex.Lock()
	cache.creds = TurnCredentials{
		Username:    localTURNUser,
		Password:    localTURNPassword,
		ServerAddrs: []string{turnURL},
		ExpiresAt:   time.Now().Add(time.Minute),
		Link:        hash,
	}
	cache.mutex.Unlock()
	t.Cleanup(func() {
		cache.mutex.Lock()
		cache.creds = TurnCredentials{}
		cache.mutex.Unlock()
	})
}

func roundTripSeed(port int, pin, classifier, hash string) servicechannel.Seed {
	return servicechannel.Seed{
		Version:           1,
		Revision:          "1",
		Environment:       "test",
		PeerIP:            "127.0.0.1",
		DTLSPort:          port,
		DTLSSPKISHA256:    pin,
		ServiceClassifier: classifier,
		VKHashes:          []string{hash},
		StreamID:          0,
	}
}
