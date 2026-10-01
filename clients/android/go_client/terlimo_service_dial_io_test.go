package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func ioRecord(ct byte, epoch uint16, seq uint64, payload []byte) []byte {
	b := make([]byte, 13+len(payload))
	b[0], b[1], b[2] = ct, 0xfe, 0xfd
	binary.BigEndian.PutUint16(b[3:], epoch)
	for i := 10; i >= 5; i-- {
		b[i] = byte(seq)
		seq >>= 8
	}
	binary.BigEndian.PutUint16(b[11:], uint16(len(payload)))
	copy(b[13:], payload)
	return b
}
func TestDialIOEndpointNormalization(t *testing.T) {
	for _, tc := range []struct{ host, canonical string }{{"192.0.2.1", "192.0.2.1|3478"}, {"2001:0DB8:0000:0:0:0:0:1", "2001:db8::1|3478"}} {
		got, ok := allocationEndpointHash(&net.UDPAddr{IP: net.ParseIP(tc.host), Port: 3478})
		want := fmt.Sprintf("%x", sha256.Sum256([]byte("whitelist-dtls-join-v1|"+tc.canonical)))
		if !ok || got != want {
			t.Fatal(got, want)
		}
	}
	for _, addr := range []net.Addr{nil, &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 3478}, &net.UDPAddr{Port: 3478}, &net.UDPAddr{IP: net.ParseIP("::1"), Port: 0}, &net.UDPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "eth0"}} {
		if _, ok := allocationEndpointHash(addr); ok {
			t.Fatal("invalid endpoint accepted")
		}
	}
}
func TestDialIORecordsMalformedCoalescedFragments(t *testing.T) {
	// A fragmented plaintext handshake plus encrypted handshake record: headers only.
	fragment := make([]byte, 15)
	fragment[0] = 11
	fragment[3] = 20
	fragment[8] = 5
	fragment[11] = 3
	b := append(ioRecord(22, 0, 7, fragment), ioRecord(22, 1, 123, []byte{0x14, 0xaa, 0xbb})...)
	r, n, bad, limit := parseDialRecords(b, time.Millisecond)
	if bad || limit || n != 2 || r[0].length != 15 || r[1].epoch != 1 || r[1].seq != 123 {
		t.Fatal(r, n, bad, limit)
	}
	for i := 0; i < 13; i++ {
		_, _, bad, _ := parseDialRecords(b[:i], 0)
		if i > 0 && !bad {
			t.Fatal("short record")
		}
	}
	for _, data := range [][]byte{{25, 0xfe, 0xfd}, append(ioRecord(20, 0, 1, []byte{1}), 1), ioRecord(99, 0, 0, nil)} {
		_, _, bad, _ := parseDialRecords(data, 0)
		if !bad {
			t.Fatal("malformed accepted")
		}
	}
	many := bytes.Repeat(ioRecord(20, 0, 0, []byte{1}), 9)
	_, n, bad, limit = parseDialRecords(many, 0)
	if n != 8 || bad || !limit {
		t.Fatal(n, bad, limit)
	}
	// Deterministic malformed length/version coverage; parser never reads payload fields.
	for n := 0; n < 256; n++ {
		data := bytes.Repeat([]byte{byte(n)}, n)
		parseDialRecords(data, 0)
	}
}

type ioFakeConn struct {
	net.PacketConn
	calls int
	data  []byte
	addr  net.Addr
	n     int
	err   error
}

func (f *ioFakeConn) WriteTo(b []byte, a net.Addr) (int, error) {
	f.calls++
	f.data = append([]byte(nil), b...)
	f.addr = a
	return f.n, f.err
}
func TestDialIOHandoffAndTXPassThrough(t *testing.T) {
	tr := newServiceDialTrace()
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.4"), Port: 4000}
	key := make([]byte, wrapKeyLen)
	plain := ioRecord(20, 0, 1, []byte{1})
	wire, err := obfsWrapPacket(key, plain, NewObfsConfig(), NewObfsState())
	if err != nil {
		t.Fatal(err)
	}
	f := &ioFakeConn{n: 3}
	buf := make([]byte, 100)
	for _, from := range []net.Addr{nil, &net.UDPAddr{IP: net.ParseIP("192.0.2.5"), Port: 4000}} {
		if err := serviceRelayHandoff(tr, from, peer, key, wire, buf, f); err != nil {
			t.Fatal(err)
		}
	}
	if err := serviceRelayHandoff(tr, peer, peer, key, []byte{1}, buf, f); err != nil {
		t.Fatal(err)
	}
	if err := serviceRelayHandoff(tr, peer, peer, key, wire, buf, f); err != nil || f.calls != 1 || !bytes.Equal(f.data, plain) || f.addr != peer {
		t.Fatal("handoff semantics")
	}
	sentinel := errors.New("private raw error")
	f.err = sentinel
	if err := serviceRelayHandoff(tr, peer, peer, key, wire, buf, f); err != sentinel {
		t.Fatal("pipe error identity")
	}
	n, err := serviceRelayWrite(tr, f, wire, peer)
	if n != 3 || err != sentinel || f.addr != peer || !bytes.Equal(f.data, wire) {
		t.Fatal("TX pass-through")
	}
	f.err = nil
	serviceRelayWrite(tr, f, wire, peer)
	c := tr.io.counts
	if c[ioRX] != 5 || c[ioPeerDrop] != 2 || c[ioUnwrapDrop] != 1 || c[ioHandoff] != 1 || c[ioHandoffError] != 1 || c[ioTXError] != 1 || c[ioTX] != 1 {
		t.Fatal(c)
	}
	// Nil observer: exact same operations/outcomes without modifying connections.
	before := f.calls
	if err := serviceRelayHandoff(nil, peer, peer, key, wire, buf, f); err != nil || f.calls != before+1 {
		t.Fatal("nil RX")
	}
	n, err = serviceRelayWrite(nil, f, wire, peer)
	if n != 3 || err != nil {
		t.Fatal("nil TX")
	}
}
func TestDialIOTailCountersSealRace(t *testing.T) {
	tr := newServiceDialTrace()
	tr.allocation(&net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 3478}, 1, turnTransportUDP)
	for i := 0; i < 100; i++ {
		tr.ioNote(ioRX)
		tr.records(ioRecord(22, 1, uint64(i), []byte{1}))
	}
	if tr.io.counts[ioRX] != 100 || tr.io.recordCount != 8 || !tr.io.recordTruncated {
		t.Fatal(tr.io)
	}
	var out bytes.Buffer
	tr.finish(nil, &out)
	if !strings.Contains(out.String(), "seq=99") || strings.Contains(out.String(), "seq=0 ") || !strings.Contains(out.String(), "rx=100") || !strings.Contains(out.String(), "truncated=1") || strings.Contains(out.String(), "192.0.2.1") {
		t.Fatal(out.String())
	}
	before := out.String()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				tr.ioNote(ioRX)
				tr.records(ioRecord(22, 1, 200, nil))
				tr.finish(nil, &out)
			}
		}()
	}
	wg.Wait()
	if before != out.String() || tr.io.counts[ioRX] != 100 {
		t.Fatal("late mutation")
	}
	tr = newServiceDialTrace()
	tr.allocation(&net.UDPAddr{IP: net.ParseIP("::1"), Port: 1}, 1, turnTransportUDP)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				tr.ioNote(ioRX)
				tr.records(ioRecord(20, 0, 1, []byte{1}))
			}
		}()
	}
	tr.finish(nil, &bytes.Buffer{})
	wg.Wait()
	// Distinguish a snapshot racing past pre-lock FINISH cutoff; don't invent strict times.
	var b strings.Builder
	s := dialIO{candidate: 1, transport: turnTransportUDP, latest: 2 * time.Millisecond}
	s.records[0] = dialRecord{elapsed: 2 * time.Millisecond}
	s.recordCount = 1
	s.recordNext = 1
	writeDialIO(&b, 1, s, time.Millisecond)
	if !strings.Contains(b.String(), "late=1") || strings.Contains(b.String(), "kind=RECORD") {
		t.Fatal(b.String())
	}
}
func TestDialIOFullBatchAndLineBounds(t *testing.T) {
	tr := newServiceDialTrace()
	tr.allocation(&net.UDPAddr{IP: net.ParseIP("::1"), Port: 1}, 1, turnTransportUDP)
	for i := 0; i < serviceDialEventCap; i++ {
		tr.note(dialSocketEnd, 1, turnTransportUDP, nil, false)
	}
	for i := 0; i < dialRecordCap; i++ {
		tr.records(ioRecord(22, 1, uint64(i), []byte{1}))
	}
	var b bytes.Buffer
	tr.finish(nil, &b)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 76 {
		t.Fatal(len(lines))
	}
	// Maximum fixed numeric field widths still fit the new consumer's 314 cap.
	s := dialIO{candidate: int(^uint(0) >> 1), transport: turnTransportTLS, endpoint: strings.Repeat("f", 64), endpointValid: true, firstRX: time.Duration(1<<63 - 1), lastRX: time.Duration(1<<63 - 1)}
	for i := range s.counts {
		s.counts[i] = ^uint64(0)
	}
	s.parseInvalid = ^uint64(0)
	s.parseLimited = ^uint64(0)
	var w strings.Builder
	writeDialIO(&w, ^uint64(0), s, time.Duration(1<<63-1))
	for _, line := range strings.Split(strings.TrimSpace(w.String()), "\n") {
		if len(line) > 314 {
			t.Fatal(len(line))
		}
	}
}
