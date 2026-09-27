package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
)

type diagnosticPacketConn struct {
	net.PacketConn
	written int
	err     error
	calls   int
}

func (c *diagnosticPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	c.calls++
	return c.written, c.err
}

func TestManagedDiagnosticLocalWriteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		size, written                        int
		err                                  error
		packets, bytes, failures, response92 uint64
	}{
		{"response", 92, 92, nil, 1, 92, 0, 1},
		{"other", 148, 148, nil, 1, 148, 0, 0},
		{"failed", 92, 0, errors.New("private endpoint and payload"), 0, 0, 1, 0},
		{"partial", 92, 12, nil, 0, 0, 1, 0},
		{"error_after_write", 92, 92, errors.New("private"), 0, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &diagnosticPacketConn{written: tc.written, err: tc.err}
			diag := &managedDiagnostics{}
			d := &Dispatcher{localConn: conn, stats: NewStats(), diagnostics: diag, ctx: context.Background()}
			if err := d.writeLocalPacket(make([]byte, tc.size), nil); err != tc.err || conn.calls != 1 {
				t.Fatal("local write semantics changed")
			}
			m := diag.message()
			for field, want := range map[string]uint64{
				"local_udp_write_packets": tc.packets, "local_udp_write_bytes": tc.bytes,
				"local_udp_write_errors": tc.failures, "local_udp_write_92_packets": tc.response92,
			} {
				if m[field] != want {
					t.Fatalf("%s = %v, want %d", field, m[field], want)
				}
			}
			if d.stats.TotalBytesDown.Load() != int64(tc.bytes) {
				t.Fatal("failed/partial write counted as delivered downlink")
			}
			raw, _ := json.Marshal(m)
			if strings.Contains(string(raw), "private") {
				t.Fatal("error text escaped")
			}
		})
	}
}

func TestManagedDiagnosticBridgeAllowlist(t *testing.T) {
	d := &managedDiagnostics{}
	d.noteLocalRX(148)
	d.noteRelayRX(92)
	d.noteRelayRX(32)
	d.noteWorker(true, false)
	d.noteWorker(true, true)
	d.noteWorker(false, false)
	d.noteWorker(false, false)
	d.noteWorker(false, true)
	d.noteLocalWrite(92, 92, nil)
	want := map[string]uint64{
		"local_udp_rx_packets": 1, "local_udp_rx_bytes": 148,
		"relay_rx_packets": 2, "relay_rx_bytes": 124, "relay_rx_92_packets": 1,
		"local_udp_write_packets": 1, "local_udp_write_bytes": 92,
		"local_udp_write_errors": 0, "local_udp_write_92_packets": 1,
		"config_worker_start_count": 1, "config_worker_ready_count": 1,
		"data_worker_start_count": 2, "data_worker_ready_count": 1,
		"data_dial_success_count": 0, "data_dial_failure_count": 0,
		"data_auth_success_count": 0, "data_auth_failure_count": 0,
		"data_session_failure_count": 0, "data_canceled_count": 0,
		"data_error_transport_timeout_count": 0, "data_error_proof_invalid_count": 0,
		"data_error_session_not_ready_count": 0, "data_error_lease_conflict_count": 0,
		"data_error_auth_required_count": 0, "data_error_other_count": 0,
	}
	b := newManagedBridge(runnerTestWriter(func(p []byte) (int, error) {
		var got map[string]any
		if err := json.Unmarshal(p, &got); err != nil {
			t.Fatal(err)
		}
		envelope := map[string]bool{"v": true, "type": true, "attempt_id": true,
			"vpn_stage": true, "vpn_error_class": true, "vpn_auth_code": true}
		if len(got) != len(want)+len(envelope) || got["v"] != float64(1) || got["type"] != "diagnostic" ||
			got["attempt_id"] != "attempt" || got["vpn_stage"] != "PENDING" ||
			got["vpn_error_class"] != "NONE" {
			t.Fatal("invalid diagnostic envelope or unexpected fields")
		}
		for key := range envelope {
			if _, present := got[key]; !present {
				t.Fatalf("missing diagnostic envelope field %s", key)
			}
		}
		for key, value := range want {
			if got[key] != float64(value) {
				t.Fatalf("%s = %v, want %d", key, got[key], value)
			}
		}
		return len(p), nil
	}), "attempt", func() {})
	if err := b.send(d.message()); err != nil {
		t.Fatal(err)
	}
}

func TestManagedVPNDiagnosticUsesOnlyClosedStageAndError(t *testing.T) {
	d := &managedDiagnostics{}
	for _, test := range []struct {
		stage                managedVPNStage
		err                  error
		wantStage, wantError string
	}{
		{vpnStageHandshake, context.DeadlineExceeded, "HANDSHAKE", "TIMEOUT"},
		{vpnStageConfig, context.Canceled, "CONFIG", "CANCELED"},
		{vpnStageBridge, errors.New("private endpoint and credential"), "BRIDGE", "FAILED"},
		{vpnStageAuth, nil, "AUTH", "NONE"},
	} {
		d.noteVPN(test.stage, test.err)
		stage, failure := d.vpnEvidence()
		if stage != test.wantStage || failure != test.wantError {
			t.Fatalf("got %s/%s want %s/%s", stage, failure, test.wantStage, test.wantError)
		}
		message := d.message()
		if message["vpn_stage"] != test.wantStage || message["vpn_error_class"] != test.wantError {
			t.Fatal("closed VPN evidence was not projected")
		}
		if strings.Contains(fmt.Sprint(message), "private") {
			t.Fatal("raw error escaped closed projection")
		}
	}
}

func TestManagedVPNDiagnosticRetainsOnlyAllowlistedAuthCode(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{errors.New("LEASE_CONFLICT"), "LEASE_CONFLICT"},
		{errors.New("PROOF_INVALID"), "PROOF_INVALID"},
		{errors.New("private server detail"), "UNKNOWN"},
	} {
		d := &managedDiagnostics{}
		d.noteVPN(vpnStageAuth, test.err)
		stage, failure, code := d.vpnSnapshot()
		if stage != "AUTH" || failure != "FAILED" || code != test.want {
			t.Fatalf("got %s/%s/%s want AUTH/FAILED/%s", stage, failure, code, test.want)
		}
		message := d.message()
		if message["vpn_auth_code"] != test.want || strings.Contains(fmt.Sprint(message), "private") {
			t.Fatal("auth diagnostic escaped the closed projection")
		}
	}
}

func TestManagedVPNDiagnosticDoesNotCollapseWorkerFailureToContextCancel(t *testing.T) {
	d := &managedDiagnostics{}
	d.noteVPN(vpnStageAuth, errors.New("private authentication detail"))
	d.noteVPNCancelIfNoFailure(vpnStageConfig, context.Canceled)
	stage, failure := d.vpnEvidence()
	if stage != "AUTH" || failure != "FAILED" {
		t.Fatalf("worker terminal collapsed to %s/%s", stage, failure)
	}

	fresh := &managedDiagnostics{}
	fresh.noteVPNCancelIfNoFailure(vpnStageConfig, context.Canceled)
	stage, failure = fresh.vpnEvidence()
	if stage != "CONFIG" || failure != "CANCELED" {
		t.Fatalf("caller cancellation lost: %s/%s", stage, failure)
	}
}

func TestManagedVPNDiagnosticDeterministicFailureThenCancel(t *testing.T) {
	d := &managedDiagnostics{}
	failureCommitted := make(chan struct{})
	done := make(chan struct{})
	go func() {
		d.noteVPN(vpnStageAuth, errors.New("private"))
		close(failureCommitted)
	}()
	go func() {
		<-failureCommitted
		d.noteVPNCancelIfNoFailure(vpnStageConfig, context.Canceled)
		close(done)
	}()
	<-done
	stage, failure := d.vpnEvidence()
	if stage != "AUTH" || failure != "FAILED" {
		t.Fatalf("cancel overwrote concrete failure: %s/%s", stage, failure)
	}
}

func TestManagedVPNDiagnosticDeterministicCancelThenFailure(t *testing.T) {
	d := &managedDiagnostics{}
	cancelCommitted := make(chan struct{})
	done := make(chan struct{})
	go func() {
		d.noteVPNCancelIfNoFailure(vpnStageConfig, context.Canceled)
		close(cancelCommitted)
	}()
	go func() {
		<-cancelCommitted
		d.noteVPN(vpnStageAuth, errors.New("private"))
		close(done)
	}()
	<-done
	stage, failure := d.vpnEvidence()
	if stage != "AUTH" || failure != "FAILED" {
		t.Fatalf("later concrete failure did not win: %s/%s", stage, failure)
	}
}

func TestManagedVPNDiagnosticConcurrentSnapshotsAreValidPairs(t *testing.T) {
	d := &managedDiagnostics{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100_000; i++ {
			d.noteVPN(vpnStageHandshake, context.DeadlineExceeded)
			d.noteVPN(vpnStageBridge, errors.New("private"))
		}
	}()
	for {
		stage, failure := d.vpnEvidence()
		if (stage != "PENDING" || failure != "NONE") &&
			(stage != "HANDSHAKE" || failure != "TIMEOUT") &&
			(stage != "BRIDGE" || failure != "FAILED") {
			t.Fatalf("mixed VPN evidence pair: %s/%s", stage, failure)
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func TestManagedVPNFinalEvidenceIsClosedAndMarkedForPreTerminalRetention(t *testing.T) {
	d := &managedDiagnostics{}
	d.noteVPN(vpnStageHandshake, context.DeadlineExceeded)
	message := d.terminalMessage()
	if message["vpn_terminal"] != true || message["vpn_stage"] != "HANDSHAKE" || message["vpn_error_class"] != "TIMEOUT" {
		t.Fatalf("unexpected terminal evidence: %#v", message)
	}
	if _, ok := message["raw_error"]; ok {
		t.Fatal("raw error field escaped")
	}
}

func TestManagedDiagnosticNilDoesNotAffectLegacy(t *testing.T) {
	var diag *managedDiagnostics
	diag.noteLocalRX(148)
	diag.noteRelayRX(92)
	diag.noteWorker(false, true)
	diag.noteLocalWrite(92, 0, errors.New("failure"))
	diag.noteDataStage(false, managedDataDial, errors.New("private"))
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		conn := &diagnosticPacketConn{err: errors.New("failure")}
		d := &Dispatcher{localConn: conn, stats: NewStats(), ctx: ctx}
		_ = d.writeLocalPacket(make([]byte, 92), nil)
		want := int64(92)
		if cancelled {
			want = 0
		}
		if d.stats.TotalBytesDown.Load() != want {
			t.Fatal("legacy accounting changed")
		}
		cancel()
	}
}

func TestManagedDiagnosticDataStagesAndSafeCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"timeout", context.DeadlineExceeded, "transport_timeout"},
		{"proof", errors.New("PROOF_INVALID"), "proof_invalid"},
		{"not_ready", errors.New("SESSION_NOT_READY"), "session_not_ready"},
		{"lease", errors.New("LEASE_CONFLICT"), "lease_conflict"},
		{"auth", errors.New("AUTH_REQUIRED"), "auth_required"},
		{"unknown", errors.New("private access URL and ID"), "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &managedDiagnostics{}
			d.noteDataStage(false, managedDataDial, nil)
			d.noteDataStage(false, managedDataAuth, nil)
			for _, stage := range []managedDataStage{managedDataDial, managedDataAuth, managedDataSession} {
				d.noteDataStage(false, stage, tc.err)
				d.noteDataStage(true, stage, tc.err) // Config attempts must not contaminate data evidence.
			}
			d.noteDataStage(false, managedDataSession, nil)
			m := d.message()
			for _, field := range []string{"data_dial_success_count", "data_auth_success_count", "data_dial_failure_count", "data_auth_failure_count", "data_session_failure_count"} {
				if m[field] != uint64(1) {
					t.Fatalf("%s = %v", field, m[field])
				}
			}
			if m["data_error_"+tc.code+"_count"] != uint64(3) {
				t.Fatal("wrong fixed code count")
			}
			if m["data_canceled_count"] != uint64(0) {
				t.Fatal("failure mislabeled canceled")
			}
			raw, _ := json.Marshal(m)
			if strings.Contains(string(raw), "private") {
				t.Fatal("raw error escaped")
			}
		})
	}
	d := &managedDiagnostics{}
	for _, stage := range []managedDataStage{managedDataDial, managedDataAuth, managedDataSession} {
		d.noteDataStage(false, stage, fmt.Errorf("private: %w", context.Canceled))
	}
	if d.message()["data_canceled_count"] != uint64(3) || d.message()["data_error_other_count"] != uint64(0) || d.message()["data_dial_failure_count"] != uint64(0) {
		t.Fatal("cancellation counted as failure")
	}
}

func TestManagedDiagnosticAuthGateStages(t *testing.T) {
	for _, mode := range []string{"data", "getconf"} {
		for _, branch := range []string{"success", "auth_failure", "session_failure", "late_cancel"} {
			t.Run(mode+"_"+branch, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				d := &managedDiagnostics{}
				cfg := &ManagedTransportConfig{Diagnostics: d, Authenticate: func(context.Context, *dtls.Conn, string, int) error {
					if branch == "auth_failure" {
						return errors.New("PROOF_INVALID")
					}
					if branch == "late_cancel" {
						cancel()
					}
					return nil
				}}
				calls := 0
				_, _ = managedAuthenticateAndRun(ctx, nil, cfg, mode, 1, func() (bool, error) {
					calls++
					if branch == "session_failure" {
						return false, errors.New("SESSION_NOT_READY")
					}
					return true, nil
				})
				wantCalls := 1
				if branch == "auth_failure" || branch == "late_cancel" {
					wantCalls = 0
				}
				if calls != wantCalls {
					t.Fatal("auth gate semantics changed")
				}
				var authOK, authFail, sessionFail, canceled uint64
				if mode == "data" {
					switch branch {
					case "success":
						authOK = 1
					case "auth_failure":
						authFail = 1
					case "session_failure":
						authOK, sessionFail = 1, 1
					case "late_cancel":
						canceled = 1
					}
				}
				for field, want := range map[string]uint64{"data_auth_success_count": authOK, "data_auth_failure_count": authFail, "data_session_failure_count": sessionFail, "data_canceled_count": canceled} {
					if d.message()[field] != want {
						t.Fatalf("%s = %v want %d", field, d.message()[field], want)
					}
				}
			})
		}
	}
}

func TestManagedDiagnosticHostAllowlistMatches(t *testing.T) {
	raw, err := os.ReadFile("../testapp/src/main/java/xyz/terlimo/test/SessionService.kt")
	if err != nil {
		t.Fatal(err)
	}
	parseAllowlist := func(name string) map[string]bool {
		_, fields, found := strings.Cut(string(raw), "private val "+name+" = setOf(")
		if !found {
			t.Fatalf("host allowlist %s missing", name)
		}
		fields, _, found = strings.Cut(fields, ")")
		if !found {
			t.Fatalf("host allowlist %s malformed", name)
		}
		out := map[string]bool{}
		for _, field := range strings.Split(fields, ",") {
			out[strings.Trim(strings.TrimSpace(field), "\"")] = true
		}
		return out
	}
	host := parseAllowlist("RELAY_FIELDS")
	for field := range parseAllowlist("VPN_DIAGNOSTIC_FIELDS") {
		host[field] = true
	}
	message := (&managedDiagnostics{}).message()
	delete(message, "type")
	if len(host) != len(message) {
		t.Fatal("host/native diagnostic field count differs")
	}
	for field := range message {
		if !host[field] {
			t.Fatalf("host rejects %s", field)
		}
	}
}

func TestManagedDiagnosticPeriodicAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writes := make(chan time.Time, 4)
	b := newManagedBridge(runnerTestWriter(func(p []byte) (int, error) {
		writes <- time.Now()
		return len(p), nil
	}), "attempt", cancel)
	done := make(chan struct{})
	started := time.Now()
	go func() { b.emitDiagnostics(ctx, &managedDiagnostics{}); close(done) }()
	select {
	case at := <-writes:
		if at.Sub(started) < time.Second {
			t.Fatal("diagnostic emitted faster than once per second")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no periodic diagnostic")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("diagnostic observer did not stop")
	}
	if len(writes) != 0 {
		t.Fatal("unexpected extra diagnostic")
	}
}
