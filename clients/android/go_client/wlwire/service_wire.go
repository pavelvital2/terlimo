// Service-only wire codec: a bounded fragmentation/reassembly format that mirrors the
// accepted WLBS framing algorithm but with a distinct version byte and its own size
// bounds. It deliberately does not touch wlwire.MaxBody or the bootstrap/evidence format.
package wlwire

import (
	"bytes"
	"encoding/binary"
	"time"
)

const (
	// ServiceVersion marks service-only frames; accepted WLBS frames use version 1.
	ServiceVersion = 2
	// ServiceFragment is the per-fragment payload ceiling (same as WLBS).
	ServiceFragment = 1024
	// ServiceMaxFrame is the decoded service JSON frame ceiling (base64 + headers).
	ServiceMaxFrame = 1536 * 1024
)

// ServiceFrames splits a bounded service frame into version-2 fragments.
func ServiceFrames(id ID, response bool, body []byte) ([][]byte, error) {
	if len(body) < 1 || len(body) > ServiceMaxFrame {
		return nil, ErrMessage
	}
	var out [][]byte
	for off := 0; off < len(body); off += ServiceFragment {
		end := off + ServiceFragment
		if end > len(body) {
			end = len(body)
		}
		b := make([]byte, 32+end-off)
		copy(b, "WLBS")
		b[4] = ServiceVersion
		if response {
			b[5] = 1
		}
		binary.BigEndian.PutUint16(b[6:], 32)
		copy(b[8:], id[:])
		binary.BigEndian.PutUint32(b[24:], uint32(len(body)))
		binary.BigEndian.PutUint32(b[28:], uint32(off))
		copy(b[32:], body[off:end])
		out = append(out, b)
	}
	return out, nil
}

// ServiceAssembler reassembles version-2 fragments with jitter/duplicate tolerance and
// hard caps applied before the output buffer is allocated.
type ServiceAssembler struct {
	started   time.Time
	id        ID
	direction byte
	total     int
	parts     map[int][]byte
}

// Add consumes one fragment. A nil body means "more fragments expected".
func (a *ServiceAssembler) Add(b []byte, now time.Time) (ID, []byte, error) {
	var id ID
	if len(b) < 33 || string(b[:4]) != "WLBS" || b[4] != ServiceVersion || b[5] > 1 || binary.BigEndian.Uint16(b[6:]) != 32 {
		return id, nil, ErrMessage
	}
	copy(id[:], b[8:24])
	total := int(binary.BigEndian.Uint32(b[24:28]))
	off := int(binary.BigEndian.Uint32(b[28:32]))
	size := total - off
	if size > ServiceFragment {
		size = ServiceFragment
	}
	if total < 1 || total > ServiceMaxFrame || off < 0 || off >= total || off%ServiceFragment != 0 || len(b)-32 != size {
		return id, nil, ErrMessage
	}
	if a.parts == nil {
		a.started = now
		a.id = id
		a.direction = b[5]
		a.total = total
		a.parts = make(map[int][]byte)
	}
	if now.Sub(a.started) >= 10*time.Second || id != a.id || a.direction != b[5] || total != a.total {
		return id, nil, ErrMessage
	}
	if old, ok := a.parts[off]; ok && !bytes.Equal(old, b[32:]) {
		return id, nil, ErrMessage
	}
	a.parts[off] = bytes.Clone(b[32:])
	if len(a.parts) != (total+ServiceFragment-1)/ServiceFragment {
		return id, nil, nil
	}
	out := make([]byte, total)
	for o, part := range a.parts {
		copy(out[o:], part)
	}
	return id, out, nil
}
