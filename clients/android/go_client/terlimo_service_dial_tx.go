package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

// Keep the beginning (including early retries), never payloads or flights.
const dialTXCap = 32
const dialTXLineCap = 64
const dialTXHandshakeCap = 8

type dialTXHandshake struct {
	record                int
	typ                   uint8
	seq                   uint16
	offset, length, total uint32
}
type dialTXDatagram struct {
	pipe, begin, end time.Duration
	began, ended     bool
	n                int
	result           string
	records          [dialRecordsPerDatagram]dialRecord
	recordCount      int
	handshakes       [dialTXHandshakeCap]dialTXHandshake
	handshakeCount   int
	invalid, limited bool
}

func dialUint24(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }

// Reuse record validation; inspect only plaintext handshake headers. No body,
// reassembly, encrypted-message inference, or datagram-to-flight mapping.
func parseDialTX(data []byte, elapsed time.Duration) (d dialTXDatagram) {
	d.pipe = elapsed
	d.records, d.recordCount, d.invalid, d.limited = parseDialRecords(data, elapsed)
	for i, r := range d.records[:d.recordCount] {
		body := data[13 : 13+int(r.length)]
		data = data[13+int(r.length):]
		if r.ct != 22 || r.epoch != 0 {
			continue
		}
		if len(body) == 0 {
			d.invalid = true
		}
		for len(body) > 0 {
			if d.handshakeCount == len(d.handshakes) {
				d.limited = true
				break
			}
			if len(body) < 12 {
				d.invalid = true
				break
			}
			total, offset, length := dialUint24(body[1:4]), dialUint24(body[6:9]), dialUint24(body[9:12])
			if offset > total || length > total-offset || int(length) > len(body)-12 {
				d.invalid = true
				break
			}
			d.handshakes[d.handshakeCount] = dialTXHandshake{i, body[0], binary.BigEndian.Uint16(body[4:6]), offset, length, total}
			d.handshakeCount++
			body = body[12+int(length):]
		}
	}
	return
}

// Called immediately after a successful pipe ReadFrom, before wrapping.
// Token is a buffer slot, not a DTLS flight ID. Zero means unrecorded.
func (t *serviceDialTrace) txPipe(data []byte) int {
	if t == nil {
		return 0
	}
	elapsed := time.Since(t.started)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		return 0
	}
	t.io.txSeen++
	if elapsed > t.io.latest {
		t.io.latest = elapsed
	}
	if t.io.txCount == dialTXCap {
		return 0
	}
	// Parse only an eligible slot while holding the same seal lock.
	// Work is bounded to 8 record / 8 handshake headers; no pump wait.
	d := parseDialTX(data, elapsed)
	t.io.txDatagrams[t.io.txCount] = d
	t.io.txCount++
	return t.io.txCount
}
func (t *serviceDialTrace) txWriteBoundary(token int, elapsed time.Duration, end bool, n int, err error) {
	if t == nil || token < 1 || token > dialTXCap {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed || token > t.io.txCount {
		return
	}
	d := &t.io.txDatagrams[token-1]
	if end {
		d.end, d.ended, d.n, d.result = elapsed, true, n, serviceDialResult(err)
	} else {
		d.begin, d.began = elapsed, true
	}
	if elapsed > t.io.latest {
		t.io.latest = elapsed
	}
}
func serviceRelayWriteObserved(t *serviceDialTrace, token int, relay net.PacketConn, data []byte, peer net.Addr) (int, error) {
	if t != nil {
		t.txWriteBoundary(token, time.Since(t.started), false, 0, nil)
	}
	n, err := relay.WriteTo(data, peer)
	if t != nil {
		t.txWriteBoundary(token, time.Since(t.started), true, n, err)
	}
	if err != nil {
		t.ioNote(ioTXError)
	} else {
		t.ioNote(ioTX)
	}
	return n, err
}
func writeDialTX(b *strings.Builder, prefix string, s dialIO, finish time.Duration) {
	if s.txSeen == 0 {
		return
	}
	details := 0
	for _, d := range s.txDatagrams[:s.txCount] {
		details += d.recordCount + d.handshakeCount
	}
	remaining := dialTXLineCap - 1 - s.txCount
	omitted := 0
	if details > remaining {
		omitted = details - remaining
	}
	fmt.Fprintf(b, "%skind=TXBOUND seen=%d kept=%d omitted_datagrams=%d omitted_headers=%d truncated=%t\n", prefix, s.txSeen, s.txCount, s.txSeen-uint64(s.txCount), omitted, s.txSeen > uint64(s.txCount) || omitted > 0)
	for i, d := range s.txDatagrams[:s.txCount] {
		// Missing/end-after-cutoff never masquerades as a successful completed write.
		begin, end, result := "UNKNOWN", "UNKNOWN", "UNKNOWN"
		if d.began && d.begin <= finish {
			begin = fmt.Sprint(d.begin.Microseconds())
		}
		if d.ended && d.end <= finish {
			end, result = fmt.Sprint(d.end.Microseconds()), d.result
		}
		late := d.pipe > finish || (d.began && d.begin > finish) || (d.ended && d.end > finish)
		n := "UNKNOWN"
		if d.ended && d.end <= finish {
			n = fmt.Sprint(d.n)
		}
		fmt.Fprintf(b, "%skind=TXDAT dir=TX id=%d pipe_us=%d begin_us=%s end_us=%s n=%s result=%s invalid=%t limited=%t late=%t\n", prefix, i+1, d.pipe.Microseconds(), begin, end, n, result, d.invalid, d.limited, late)
	}
	// All timing/results first. Earliest observed plaintext ClientHello headers
	// take priority over other handshake headers, then record headers.
	for pass := 0; pass < 2; pass++ {
		for i, d := range s.txDatagrams[:s.txCount] {
			for _, h := range d.handshakes[:d.handshakeCount] {
				if remaining == 0 {
					return
				}
				if (h.typ == 1) != (pass == 0) {
					continue
				}
				fmt.Fprintf(b, "%skind=TXHS dir=TX id=%d record=%d type=%d message_seq=%d offset=%d length=%d total=%d\n", prefix, i+1, h.record, h.typ, h.seq, h.offset, h.length, h.total)
				remaining--
			}
		}
	}
	for i, d := range s.txDatagrams[:s.txCount] {
		for j, r := range d.records[:d.recordCount] {
			if remaining == 0 {
				return
			}
			hs := "NONE"
			if r.ct == 22 {
				hs = "PLAINTEXT"
				if r.epoch > 0 {
					hs = "UNKNOWN"
				}
			}
			fmt.Fprintf(b, "%skind=TXREC dir=TX id=%d record=%d ct=%d epoch=%d seq=%d len=%d hs=%s\n", prefix, i+1, j, r.ct, r.epoch, r.seq, r.length, hs)
			remaining--
		}
	}
}
