package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func txHandshake(typ byte, seq uint16, total, offset, length uint32) []byte {
	b := make([]byte, 12+length)
	b[0] = typ
	binary.BigEndian.PutUint16(b[4:6], seq)
	for _, f := range []struct {
		at int
		n  uint32
	}{{1, total}, {6, offset}, {9, length}} {
		b[f.at], b[f.at+1], b[f.at+2] = byte(f.n>>16), byte(f.n>>8), byte(f.n)
	}
	copy(b[12:], []byte("PRIVATE-BODY"))
	return b
}
func TestDialTXParseHeadersFragmentsAndUnknown(t *testing.T) {
	first := append(txHandshake(1, 7, 50, 5, 12), txHandshake(11, 8, 0, 0, 0)...)
	data := append(ioRecord(22, 0, 15, first), ioRecord(22, 1, 16, txHandshake(20, 9, 12, 0, 12))...)
	data = append(data, ioRecord(20, 0, 17, []byte{1})...)
	d := parseDialTX(data, 42*time.Microsecond)
	if d.invalid || d.limited || d.recordCount != 3 || d.handshakeCount != 2 || d.pipe != 42*time.Microsecond {
		t.Fatal(d)
	}
	h := d.handshakes[0]
	if h.typ != 1 || h.seq != 7 || h.offset != 5 || h.length != 12 || h.total != 50 || h.record != 0 {
		t.Fatal(h)
	}
	s := dialIO{txSeen: 1, txCount: 1}
	s.txDatagrams[0] = d
	var b strings.Builder
	writeDialTX(&b, "", s, time.Second)
	out := b.String()
	if !strings.Contains(out, "epoch=1 seq=16 len=24 hs=UNKNOWN") || strings.Contains(out, "Finished") || strings.Contains(out, "type=20") || strings.Contains(out, "PRIVATE") || !strings.Contains(out, "dir=TX") {
		t.Fatal(out)
	}
	// Plaintext body mutation is immaterial; headers are the only retained data.
	changed := append([]byte(nil), data...)
	for i := 25; i < 37; i++ {
		changed[i] ^= 0xff
	}
	if got := parseDialTX(changed, 42*time.Microsecond); got != d {
		t.Fatal("body retained")
	}
}
func TestDialTXParseMalformedAndBounded(t *testing.T) {
	valid := ioRecord(22, 0, 1, txHandshake(1, 0, 12, 0, 12))
	for i := 1; i < len(valid); i++ {
		if !parseDialTX(valid[:i], 0).invalid {
			t.Fatalf("truncated %d", i)
		}
	}
	for _, h := range [][]byte{nil, make([]byte, 11), txHandshake(1, 0, 2, 3, 0), txHandshake(1, 0, 2, 1, 2)} {
		if !parseDialTX(ioRecord(22, 0, 1, h), 0).invalid {
			t.Fatal("invalid fragment accepted")
		}
	}
	many := bytes.Repeat(ioRecord(20, 0, 1, []byte{1}), 9)
	d := parseDialTX(many, 0)
	if d.invalid || !d.limited || d.recordCount != 8 {
		t.Fatal(d)
	}
	manyHS := bytes.Repeat(txHandshake(1, 0, 0, 0, 0), 9)
	d = parseDialTX(ioRecord(22, 0, 1, manyHS), 0)
	if d.invalid || !d.limited || d.handshakeCount != 8 {
		t.Fatal(d)
	}
	// Complete zero-length fragments remain bounded and consume their headers.
	for n := 0; n < 256; n++ {
		parseDialTX(bytes.Repeat([]byte{byte(n)}, n), 0)
	}
}
func TestDialTXWriteParityAndSpans(t *testing.T) {
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1234}
	sentinel := errors.New("PRIVATE-ERROR")
	for _, tc := range []struct {
		n   int
		err error
	}{{3, nil}, {1, nil}, {1, sentinel}, {0, sentinel}} {
		for _, observed := range []bool{false, true} {
			var tr *serviceDialTrace
			token := 0
			if observed {
				tr = newServiceDialTrace()
				token = tr.txPipe(ioRecord(22, 0, 0, txHandshake(1, 0, 0, 0, 0)))
			}
			f := &ioFakeConn{n: tc.n, err: tc.err}
			wire := []byte{9, 8, 7}
			n, err := serviceRelayWriteObserved(tr, token, f, wire, peer)
			if n != tc.n || err != tc.err || f.calls != 1 || f.addr != peer || !bytes.Equal(f.data, wire) {
				t.Fatal("I/O semantics")
			}
			if tr == nil {
				continue
			}
			d := tr.io.txDatagrams[0]
			if !d.began || !d.ended || d.pipe > d.begin || d.begin > d.end || d.n != tc.n || d.result != serviceDialResult(tc.err) {
				t.Fatal(d)
			}
			if tc.err == nil && tr.io.counts[ioTX] != 1 || tc.err != nil && tr.io.counts[ioTXError] != 1 {
				t.Fatal(tr.io.counts)
			}
			var b strings.Builder
			writeDialTX(&b, "", tr.io, time.Hour)
			if strings.Contains(b.String(), "PRIVATE") || strings.Contains(b.String(), "192.0.2") {
				t.Fatal(b.String())
			}
		}
	}
}
func TestDialTXBoundSealAndMissingBoundary(t *testing.T) {
	tr := newServiceDialTrace()
	tr.allocation(&net.UDPAddr{IP: net.ParseIP("::1"), Port: 1}, 1, turnTransportUDP)
	data := ioRecord(22, 0, 0, txHandshake(1, 0, 0, 0, 0))
	for i := 0; i < dialTXCap+5; i++ {
		token := tr.txPipe(data)
		if i < dialTXCap && token != i+1 || i >= dialTXCap && token != 0 {
			t.Fatal(token)
		}
	}
	if tr.io.txSeen != 37 || tr.io.txCount != 32 {
		t.Fatal(tr.io.txSeen, tr.io.txCount)
	}
	var b bytes.Buffer
	tr.finish(nil, &b)
	before := tr.io
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); tr.txPipe(data); tr.txWriteBoundary(1, time.Hour, true, 1, nil) }()
	}
	wg.Wait()
	if before != tr.io || !strings.Contains(b.String(), "truncated=true") || !strings.Contains(b.String(), "begin_us=UNKNOWN end_us=UNKNOWN n=UNKNOWN result=UNKNOWN") {
		t.Fatal("seal/bounds", b.String())
	}
	s := dialIO{txSeen: 1, txCount: 1}
	s.txDatagrams[0] = dialTXDatagram{pipe: time.Millisecond, begin: 2 * time.Millisecond, end: 4 * time.Millisecond, began: true, ended: true, n: 3, result: "OK"}
	var w strings.Builder
	writeDialTX(&w, "", s, 3*time.Millisecond)
	if !strings.Contains(w.String(), "begin_us=2000 end_us=UNKNOWN n=UNKNOWN result=UNKNOWN") || !strings.Contains(w.String(), "late=true") {
		t.Fatal(w.String())
	}
}

func TestDialTXOutputBounds(t *testing.T) {
	s := dialIO{candidate: int(^uint(0) >> 1), transport: turnTransportTLS, txSeen: ^uint64(0), txCount: dialTXCap}
	for i := range s.txDatagrams {
		d := &s.txDatagrams[i]
		d.pipe, d.begin, d.end = time.Duration(1<<63-1), time.Duration(1<<63-1), time.Duration(1<<63-1)
		d.began, d.ended, d.n, d.result = true, true, -int(^uint(0)>>1)-1, "CANCELED"
		d.recordCount, d.handshakeCount = 8, 8
		for j := 0; j < 8; j++ {
			d.records[j] = dialRecord{ct: 22, epoch: 65535, seq: 1<<48 - 1, length: 65535}
			d.handshakes[j] = dialTXHandshake{record: j, typ: 255, seq: 65535, offset: 1<<24 - 1, length: 1<<24 - 1, total: 1<<24 - 1}
		}
	}
	var b strings.Builder
	writeDialIO(&b, ^uint64(0), s, time.Duration(1<<63-1))
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 3+dialTXLineCap {
		t.Fatal(len(lines))
	}
	for _, line := range lines {
		if len(line) > 314 {
			t.Fatalf("line %d: %s", len(line), line)
		}
	}
}

// Offline parser-poison check runs this test in a temporary source copy.
func TestDialTXIneligible(t *testing.T) {
	tr := newServiceDialTrace()
	tr.io.txCount, tr.io.txSeen = dialTXCap, dialTXCap
	if tr.txPipe([]byte{1, 2, 3}) != 0 || tr.io.txSeen != dialTXCap+1 {
		t.Fatal("full")
	}
	tr.sealed = true
	before := tr.io
	if tr.txPipe([]byte{1, 2, 3}) != 0 || tr.io != before {
		t.Fatal("sealed")
	}
}
func TestDialTXGeneratedCombinedWorstCase(t *testing.T) {
	tr := newServiceDialTrace()
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1234}
	tr.allocation(peer, 1, turnTransportUDP)
	for i := 0; i < serviceDialEventCap; i++ {
		tr.note(dialSocketEnd, 1, turnTransportUDP, nil, false)
	}
	var data []byte
	for j := 0; j < 8; j++ {
		data = append(data, ioRecord(22, 0, uint64(j), txHandshake(1, uint16(j), 12, 0, 12))...)
	}
	for i := 0; i < 40; i++ {
		token := tr.txPipe(data)
		serviceRelayWriteObserved(tr, token, &ioFakeConn{n: len(data)}, data, peer)
	}
	for i := 0; i < 8; i++ {
		tr.ioNote(ioRX)
		tr.records(ioRecord(22, 1, uint64(i), []byte{1}))
	}
	var b bytes.Buffer
	tr.finish(nil, &b)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 140 {
		t.Fatal(len(lines))
	}
	dat, hs := 0, 0
	for _, line := range lines {
		if len(line) > 314 {
			t.Fatal("line overflow")
		}
		if strings.Contains(line, "kind=TXDAT ") {
			dat++
		}
		if strings.Contains(line, "kind=TXHS ") {
			hs++
			if !strings.Contains(line, "type=1") {
				t.Fatal("priority")
			}
		}
	}
	if dat != 32 || hs != 31 || !strings.Contains(b.String(), "seen=40 kept=32 omitted_datagrams=8 omitted_headers=481 truncated=true") {
		t.Fatal(dat, hs, b.String())
	}
	if path := os.Getenv("DIAL_TX_FIXTURE"); path != "" {
		if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDialTXConcurrentFinish(t *testing.T) {
	tr := newServiceDialTrace()
	tr.allocation(&net.UDPAddr{IP: net.ParseIP("::1"), Port: 1}, 1, turnTransportUDP)
	data := ioRecord(22, 0, 0, txHandshake(1, 0, 0, 0, 0))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				token := tr.txPipe(data)
				tr.txWriteBoundary(token, time.Since(tr.started), false, 0, nil)
				tr.txWriteBoundary(token, time.Since(tr.started), true, 3, nil)
			}
		}()
	}
	var b bytes.Buffer
	tr.finish(nil, &b)
	tr.mu.Lock()
	before := tr.io
	tr.mu.Unlock()
	wg.Wait()
	if tr.io != before {
		t.Fatal("post-seal mutation")
	}
}
