package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

const dialRecordCap = 8 // last records survive early retransmissions
const dialRecordsPerDatagram = 8

type dialIOKind uint8

const (
	ioRX dialIOKind = iota
	ioReadError
	ioPeerDrop
	ioUnwrapDrop
	ioHandoff
	ioHandoffError
	ioTX
	ioTXError
	ioWrapError
	ioTXReadError
	ioKindCount
)

type dialRecord struct {
	elapsed time.Duration
	ct      uint8
	epoch   uint16
	seq     uint64
	length  uint16
}
type dialIO struct {
	txDatagrams                [dialTXCap]dialTXDatagram
	txCount                    int
	txSeen                     uint64
	candidate                  int
	transport                  turnTransport
	endpoint                   string
	endpointValid              bool
	counts                     [ioKindCount]uint64
	firstRX, lastRX, latest    time.Duration
	records                    [dialRecordCap]dialRecord
	recordCount, recordNext    int
	recordTruncated            bool
	parseInvalid, parseLimited uint64
}

// Call only on the PacketConn returned by TURN Allocate, before wrapping it.
// Pion TURN UDPConn.LocalAddr is its XOR-RELAYED-ADDRESS, not the socket address.
func allocationEndpointHash(addr net.Addr) (string, bool) {
	u, ok := addr.(*net.UDPAddr)
	if !ok || u == nil || u.Zone != "" || u.Port < 1 || u.Port > 65535 {
		return "", false
	}
	ip, err := netip.ParseAddr(u.IP.String())
	if err != nil {
		return "", false
	}
	host := ip.String() // UDPAddr textual host, normalized as Python ipaddress
	sum := sha256.Sum256([]byte(fmt.Sprintf("whitelist-dtls-join-v1|%s|%d", host, u.Port)))
	return fmt.Sprintf("%x", sum), true
}
func (t *serviceDialTrace) allocation(addr net.Addr, candidate int, tr turnTransport) {
	if t == nil {
		return
	}
	elapsed := time.Since(t.started)
	hash, valid := allocationEndpointHash(addr)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		return
	}
	t.io.candidate, t.io.transport = candidate, tr
	t.io.endpoint, t.io.endpointValid = hash, valid
	t.io.latest = elapsed
}
func (t *serviceDialTrace) ioNote(kind dialIOKind) {
	if t == nil {
		return
	}
	elapsed := time.Since(t.started)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		return
	}
	t.io.counts[kind]++
	if kind == ioRX {
		if t.io.counts[ioRX] == 1 {
			t.io.firstRX = elapsed
		}
		if elapsed > t.io.lastRX {
			t.io.lastRX = elapsed
		}
		if elapsed < t.io.firstRX {
			t.io.firstRX = elapsed
		}
	}
	if elapsed > t.io.latest {
		t.io.latest = elapsed
	}
}

// Headers only. No reassembly, decryption, or handshake type inference: epoch>0
// content type 22 is encrypted handshake, never proof of a Finished message.
// Fragmented handshakes are ordinary records and need no payload inspection.
func parseDialRecords(data []byte, elapsed time.Duration) (out [dialRecordsPerDatagram]dialRecord, count int, invalid, limited bool) {
	for len(data) > 0 {
		if count == len(out) {
			return out, count, false, true
		}
		if len(data) < 13 || (data[0] < 20 || data[0] > 23) || data[1] != 0xfe || (data[2] != 0xfd && data[2] != 0xff) {
			return out, count, true, false
		}
		n := int(binary.BigEndian.Uint16(data[11:13]))
		if n > len(data)-13 {
			return out, count, true, false
		}
		var seq uint64
		for _, b := range data[5:11] {
			seq = seq<<8 | uint64(b)
		}
		out[count] = dialRecord{elapsed, data[0], binary.BigEndian.Uint16(data[3:5]), seq, uint16(n)}
		count++
		data = data[13+n:]
	}
	return
}
func (t *serviceDialTrace) records(data []byte) {
	if t == nil {
		return
	}
	elapsed := time.Since(t.started)
	parsed, n, invalid, limited := parseDialRecords(data, elapsed)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		return
	}
	if invalid {
		t.io.parseInvalid++
	}
	if limited {
		t.io.parseLimited++
	}
	for _, rec := range parsed[:n] {
		if t.io.recordCount == dialRecordCap {
			t.io.recordTruncated = true
		} else {
			t.io.recordCount++
		}
		t.io.records[t.io.recordNext] = rec
		t.io.recordNext = (t.io.recordNext + 1) % dialRecordCap
	}
	if elapsed > t.io.latest {
		t.io.latest = elapsed
	}
}

// Same receive policy and n/error/address semantics as the original pump.
func serviceRelayHandoff(t *serviceDialTrace, from, peer net.Addr, key, wire, plain []byte, pipe net.PacketConn) error {
	t.ioNote(ioRX)
	if from == nil || from.String() != peer.String() {
		t.ioNote(ioPeerDrop)
		return nil
	}
	n, err := obfsUnwrapPacket(key, wire, plain)
	if err != nil {
		t.ioNote(ioUnwrapDrop)
		return nil
	}
	t.records(plain[:n])
	_, err = pipe.WriteTo(plain[:n], peer)
	if err != nil {
		t.ioNote(ioHandoffError)
	} else {
		t.ioNote(ioHandoff)
	}
	return err
}
func writeDialIO(b *strings.Builder, call uint64, s dialIO, finish time.Duration) {
	if s.candidate == 0 {
		return
	} // no allocation/pumps observed
	prefix := fmt.Sprintf("dialio: call=%d candidate=%d transport=%s ", call, s.candidate, serviceDialTransportName(s.candidate, s.transport))
	hash := s.endpoint
	if !s.endpointValid {
		hash = "NONE"
	}
	fmt.Fprintf(b, "%skind=ENDPOINT hash=%s\n", prefix, hash)
	// Counters are a seal-time snapshot. If a boundary raced after FINISH capture
	// but before seal, late=1 explicitly prevents claiming a strict FINISH cutoff.
	late := 0
	if s.latest > finish {
		late = 1
	}
	c := s.counts
	fmt.Fprintf(b, "%skind=RX rx=%d readerr=%d peer=%d unwrap=%d handoff=%d pipeerr=%d first_ms=%d last_ms=%d late=%d\n", prefix, c[ioRX], c[ioReadError], c[ioPeerDrop], c[ioUnwrapDrop], c[ioHandoff], c[ioHandoffError], s.firstRX.Milliseconds(), s.lastRX.Milliseconds(), late)
	truncated := 0
	if s.recordTruncated {
		truncated = 1
	}
	fmt.Fprintf(b, "%skind=TX tx=%d txerr=%d wraperr=%d readerr=%d invalid=%d limited=%d truncated=%d\n", prefix, c[ioTX], c[ioTXError], c[ioWrapError], c[ioTXReadError], s.parseInvalid, s.parseLimited, truncated)
	start := (s.recordNext - s.recordCount + dialRecordCap) % dialRecordCap
	for i := 0; i < s.recordCount; i++ {
		r := s.records[(start+i)%dialRecordCap]
		if r.elapsed > finish {
			continue
		}
		fmt.Fprintf(b, "%skind=RECORD ct=%d epoch=%d seq=%d len=%d elapsed_ms=%d\n", prefix, r.ct, r.epoch, r.seq, r.length, r.elapsed.Milliseconds())
	}
	writeDialTX(b, prefix, s, finish)
}

func serviceRelayWrite(t *serviceDialTrace, relay net.PacketConn, data []byte, peer net.Addr) (int, error) {
	return serviceRelayWriteObserved(t, 0, relay, data, peer)
}
