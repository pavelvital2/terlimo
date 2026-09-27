package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

type scriptAddr struct{}

func (scriptAddr) Network() string { return "script" }
func (scriptAddr) String() string  { return "script" }

// scriptConn is a deterministic net.Conn: reads return the scripted records one call at a
// time (never blocking), writes are captured, deadlines are recorded (never enforced).
type scriptConn struct {
	reads          [][]byte
	readIdx        int
	writes         [][]byte
	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

func (c *scriptConn) Read(p []byte) (int, error) {
	if c.readIdx >= len(c.reads) {
		return 0, io.EOF
	}
	record := c.reads[c.readIdx]
	c.readIdx++
	return copy(p, record), nil
}
func (c *scriptConn) Write(p []byte) (int, error) {
	c.writes = append(c.writes, append([]byte(nil), p...))
	return len(p), nil
}
func (c *scriptConn) Close() error                { return nil }
func (c *scriptConn) LocalAddr() net.Addr         { return scriptAddr{} }
func (c *scriptConn) RemoteAddr() net.Addr        { return scriptAddr{} }
func (c *scriptConn) SetDeadline(time.Time) error { return nil }
func (c *scriptConn) SetReadDeadline(t time.Time) error {
	c.readDeadlines = append(c.readDeadlines, t)
	return nil
}
func (c *scriptConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadlines = append(c.writeDeadlines, t)
	return nil
}

func TestIsExactKeepaliveStrict(t *testing.T) {
	if !isExactKeepalive([]byte{0xFF}) {
		t.Fatal("single 0xFF must be keepalive")
	}
	for _, rec := range [][]byte{{}, {0x00}, {0xFF, 0x00}, {0x00, 0xFF}, {0xFF, 0xFF}} {
		if isExactKeepalive(rec) {
			t.Fatalf("record %v must not be a keepalive", rec)
		}
	}
}

func TestFirstRecordKeepaliveThenMUXNoDeadlineRefresh(t *testing.T) {
	conn := &scriptConn{reads: [][]byte{[]byte(multipathRelayHello)}}
	buf := make([]byte, 1600)
	buf[0] = dtlsKeepaliveByte
	n, ok := consumeFirstKeepalives(context.Background(), conn, buf, 1)
	if !ok || string(buf[:n]) != multipathRelayHello {
		t.Fatalf("expected MUX record, got n=%d ok=%v %q", n, ok, buf[:n])
	}
	if len(conn.writes) != 1 || !bytes.Equal(conn.writes[0], []byte{0xFF}) {
		t.Fatalf("keepalive was not mirrored exactly once: %v", conn.writes)
	}
	if len(conn.readDeadlines) != 0 {
		t.Fatalf("helper refreshed the read deadline: %v", conn.readDeadlines)
	}
	if len(conn.writeDeadlines) != 2 {
		t.Fatalf("keepalive write deadline not set/cleared: %v", conn.writeDeadlines)
	}
}

func TestFirstRecordKeepaliveThenValidWGNotForwarded(t *testing.T) {
	data := make([]byte, 32)
	data[0] = 4
	conn := &scriptConn{reads: [][]byte{data}}
	buf := make([]byte, 1600)
	buf[0] = dtlsKeepaliveByte
	n, ok := consumeFirstKeepalives(context.Background(), conn, buf, 1)
	if !ok || n != len(data) || !bytes.Equal(buf[:n], data) {
		t.Fatalf("WG record not surfaced: n=%d ok=%v", n, ok)
	}
	if isExactKeepalive(buf[:n]) {
		t.Fatal("keepalive leaked into the WG record")
	}
}

func TestUnknownShortRecordsRemainFailClosed(t *testing.T) {
	for _, rec := range [][]byte{{0x00}, {0x00, 0x01}, {0x01, 0x02, 0x03}} {
		conn := &scriptConn{reads: [][]byte{rec}}
		buf := make([]byte, 1600)
		copy(buf, rec)
		n, ok := consumeFirstKeepalives(context.Background(), conn, buf, len(rec))
		if !ok || n != len(rec) {
			t.Fatalf("record %v wrongly consumed: n=%d ok=%v", rec, n, ok)
		}
		if len(conn.writes) != 0 {
			t.Fatalf("record %v mirrored/forwarded as control: %v", rec, conn.writes)
		}
	}
	// A managed identity with a 2-byte first record is still rejected by the WG guard.
	identity, _, _ := managedFixture(t)
	allowed, class, reason := clientWGDecision(identity, []byte{0x00, 0x01}, &wgKeys{})
	if allowed || class != wgClassShort || reason != wgReasonUnsupported {
		t.Fatalf("2-byte record not fail-closed: allowed=%v class=%s reason=%s", allowed, class, reason)
	}
}

func TestRepeatedKeepaliveCannotWaitForever(t *testing.T) {
	reads := make([][]byte, maxFirstKeepalives+10)
	for i := range reads {
		reads[i] = []byte{0xFF}
	}
	conn := &scriptConn{reads: reads}
	buf := make([]byte, 1600)
	buf[0] = dtlsKeepaliveByte
	n, ok := consumeFirstKeepalives(context.Background(), conn, buf, 1)
	if ok {
		t.Fatal("unbounded keepalives must stop the first-record path")
	}
	if conn.readIdx != maxFirstKeepalives || len(conn.writes) != maxFirstKeepalives {
		t.Fatalf("hard cap not applied: reads=%d writes=%d", conn.readIdx, len(conn.writes))
	}
	if n != 0 {
		t.Fatalf("expected n=0 on stop, got %d", n)
	}
}

func TestRepeatedKeepaliveHonoursCancellation(t *testing.T) {
	reads := make([][]byte, 8)
	for i := range reads {
		reads[i] = []byte{0xFF}
	}
	conn := &scriptConn{reads: reads}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	buf := make([]byte, 1600)
	buf[0] = dtlsKeepaliveByte
	if _, ok := consumeFirstKeepalives(ctx, conn, buf, 1); ok {
		t.Fatal("cancelled ctx must stop keepalive skipping")
	}
	if conn.readIdx != 0 {
		t.Fatalf("kept reading after cancel: %d", conn.readIdx)
	}
}

func TestFirstRecordDiagnosticOnceBoundedAndSecretFree(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	trace := newManagedConnTrace()
	conn := &clientTestConn{trace: trace}
	traceManagedFirstRecord(conn, []byte{0xFF})
	traceManagedFirstRecord(conn, []byte(multipathRelayHello))
	traceManagedFirstRecord(conn, make([]byte, 200))

	out := buf.String()
	if strings.Count(out, "[MANAGED_TRACE]") != 1 {
		t.Fatalf("first-record diagnostic not once: %q", out)
	}
	if !strings.Contains(out, "stage=first_record length_bucket=1 is_keepalive=true") {
		t.Fatalf("unexpected diagnostic: %q", out)
	}
	for _, bad := range []string{"WDTT_MUX1", "0xff", "\\xff", "192.0.2", "GRANT"} {
		if strings.Contains(out, bad) {
			t.Fatalf("sensitive content %q in %q", bad, out)
		}
	}
}

func TestLengthBucketValues(t *testing.T) {
	want := map[int]string{0: "0", 1: "1", 2: "2-3", 3: "2-3", 4: "4+", 1600: "4+"}
	for n, w := range want {
		if got := lengthBucket(n); got != w {
			t.Fatalf("lengthBucket(%d)=%s want %s", n, got, w)
		}
	}
}

func TestClassifyFirstCandidateSkipsControlLogsKeepalive(t *testing.T) {
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(old)

	trace := newManagedConnTrace()
	conn := &clientTestConn{trace: trace}
	classifyFirstCandidate(conn, []byte("GETCONF:9000|reg|pw|info|sess")) // known control: no log
	classifyFirstCandidate(conn, []byte{0xFF})                            // first candidate, before consume
	classifyFirstCandidate(conn, []byte(multipathRelayHello))             // once-only: no second log

	s := out.String()
	if strings.Count(s, "[MANAGED_TRACE]") != 1 {
		t.Fatalf("diagnostic not once: %q", s)
	}
	if !strings.Contains(s, "length_bucket=1 is_keepalive=true") {
		t.Fatalf("keepalive candidate not classified before consume: %q", s)
	}
	if strings.Contains(s, "GETCONF") || strings.Contains(s, multipathRelayHello) {
		t.Fatalf("control/raw content leaked: %q", s)
	}
}

func TestClassifyFirstCandidateREADYThenKeepaliveThenWG(t *testing.T) {
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(old)

	conn := &clientTestConn{trace: newManagedConnTrace()}
	classifyFirstCandidate(conn, []byte("READY")) // known control: no log
	classifyFirstCandidate(conn, []byte{0xFF})    // candidate keepalive logged before consume

	s := out.String()
	if strings.Count(s, "[MANAGED_TRACE]") != 1 || !strings.Contains(s, "length_bucket=1 is_keepalive=true") {
		t.Fatalf("READY->0xFF classification wrong: %q", s)
	}
}

func TestGETCONFThenKeepaliveThenMUXDiagnosticBeforeConsume(t *testing.T) {
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(old)

	conn := &clientTestConn{trace: newManagedConnTrace()}
	classifyFirstCandidate(conn, []byte("GETCONF:9000|reg|pw|info|sess"))
	buf := make([]byte, 1600)
	buf[0] = dtlsKeepaliveByte
	classifyFirstCandidate(conn, buf[:1])
	script := &scriptConn{reads: [][]byte{[]byte(multipathRelayHello)}}
	n, ok := consumeFirstKeepalives(context.Background(), script, buf, 1)
	if !ok || string(buf[:n]) != multipathRelayHello {
		t.Fatalf("GETCONF->0xFF->MUX did not reach MUX: n=%d ok=%v", n, ok)
	}
	if strings.Count(out.String(), "[MANAGED_TRACE]") != 1 ||
		!strings.Contains(out.String(), "length_bucket=1 is_keepalive=true") {
		t.Fatalf("GETCONF->0xFF diagnostic wrong: %q", out.String())
	}
}

func j01GrantConn(t *testing.T, records [][]byte) (*clientTestConn, accessIdentity, ClientTestGrant) {
	t.Helper()
	identity, grant, _ := managedFixture(t)
	return &clientTestConn{trace: newManagedConnTrace(), Conn: &scriptConn{reads: records}, identity: identity, grant: grant, configured: true, done: make(chan struct{})}, identity, grant
}

func TestKeepaliveSeriesStopsBeforeDataOnExpiry(t *testing.T) {
	data := make([]byte, 32)
	data[0] = 4
	conn, identity, _ := j01GrantConn(t, [][]byte{{0xFF}, data})
	dbMutex.Lock()
	db.Passwords[identity.password].ExpiresAt = time.Now().Add(-time.Second).Unix()
	dbMutex.Unlock()
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 || err == nil || err.Error() != "LEASE_EXPIRED" {
		t.Fatalf("expired keepalive series not stopped before data: n=%d err=%v", n, err)
	}
}

func TestKeepaliveSeriesStopsBeforeDataOnRevoke(t *testing.T) {
	data := make([]byte, 32)
	data[0] = 4
	conn, identity, _ := j01GrantConn(t, [][]byte{{0xFF}, data})
	dbMutex.Lock()
	db.Passwords[identity.password].ClientTest.Revoked = true
	dbMutex.Unlock()
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 || err == nil || err.Error() != "LEASE_EXPIRED" {
		t.Fatalf("revoked keepalive series not stopped before data: n=%d err=%v", n, err)
	}
}

func TestKeepaliveSeriesActiveThenRevokeStopsBeforeData(t *testing.T) {
	data := make([]byte, 32)
	data[0] = 4
	conn, identity, _ := j01GrantConn(t, [][]byte{{0xFF}, data})
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || n != 1 || buf[0] != dtlsKeepaliveByte {
		t.Fatalf("active keepalive not surfaced: n=%d err=%v", n, err)
	}
	dbMutex.Lock()
	db.Passwords[identity.password].ClientTest.Revoked = true
	dbMutex.Unlock()
	n, err = conn.Read(buf)
	if n != 0 || err == nil || err.Error() != "LEASE_EXPIRED" {
		t.Fatalf("revoke mid-series did not stop before data: n=%d err=%v", n, err)
	}
}

func TestKeepaliveSeriesActiveThenExpiryStopsBeforeData(t *testing.T) {
	data := make([]byte, 32)
	data[0] = 4
	conn, identity, _ := j01GrantConn(t, [][]byte{{0xFF}, data})
	buf := make([]byte, 64)
	if n, err := conn.Read(buf); err != nil || n != 1 {
		t.Fatalf("active keepalive not surfaced: n=%d err=%v", n, err)
	}
	dbMutex.Lock()
	db.Passwords[identity.password].ExpiresAt = time.Now().Add(-time.Second).Unix()
	dbMutex.Unlock()
	n, err := conn.Read(buf)
	if n != 0 || err == nil || err.Error() != "LEASE_EXPIRED" {
		t.Fatalf("expiry mid-series did not stop before data: n=%d err=%v", n, err)
	}
}
