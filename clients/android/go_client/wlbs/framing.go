// Package wlbs implements the approved TEST wire contract. It does not dial,
// provision credentials, trust issuers implicitly, or activate a VPN interface.
package wlbs

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"time"
)

const (
	HeaderSize   = 32
	FragmentSize = 1024
	MaxBody      = 16384
	MaxAttempts  = 3
)
const AssemblyTimeout = 10 * time.Second

type ID [16]byte

func NewID() (ID, error)     { var id ID; _, err := rand.Read(id[:]); return id, err }
func (id ID) String() string { return base64.RawURLEncoding.EncodeToString(id[:]) }
func ParseID(s string) (ID, error) {
	var id ID
	b, e := DecodeBinary(s, 16)
	if e == nil {
		copy(id[:], b)
	}
	return id, e
}

// Error only contains protocol-safe codes; never include bodies or credentials.
type Error struct {
	Code         string `json:"code"`
	Retryable    bool   `json:"retryable"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
}

func (e *Error) Error() string  { return e.Code }
func failure(code string) error { return &Error{Code: code} }

func Frames(id ID, response bool, body []byte) ([][]byte, error) {
	if len(body) < 1 || len(body) > MaxBody {
		return nil, failure("BAD_MESSAGE")
	}
	frames := make([][]byte, 0, (len(body)+FragmentSize-1)/FragmentSize)
	for off := 0; off < len(body); off += FragmentSize {
		end := off + FragmentSize
		if end > len(body) {
			end = len(body)
		}
		f := make([]byte, HeaderSize+end-off)
		copy(f, "WLBS")
		f[4] = 1
		if response {
			f[5] = 1
		}
		binary.BigEndian.PutUint16(f[6:8], HeaderSize)
		copy(f[8:24], id[:])
		binary.BigEndian.PutUint32(f[24:28], uint32(len(body)))
		binary.BigEndian.PutUint32(f[28:32], uint32(off))
		copy(f[32:], body[off:end])
		frames = append(frames, f)
	}
	return frames, nil
}

// Reassembler is bounded to one message on one connection/direction. Its absolute
// deadline is not extended by duplicates; discard it after any error/completion.
type Reassembler struct {
	ID       ID
	Response bool
	started  time.Time
	total    int
	body     []byte
	seen     [16]bool
	count    int
	failed   bool
}

func NewReassembler(id ID, response bool) *Reassembler {
	return &Reassembler{ID: id, Response: response}
}
func (a *Reassembler) Add(record []byte, now time.Time) ([]byte, bool, error) {
	bad := func() ([]byte, bool, error) { a.failed = true; return nil, false, failure("BAD_MESSAGE") }
	if a.failed {
		return bad()
	}
	if !a.started.IsZero() && !now.Before(a.started.Add(AssemblyTimeout)) {
		a.failed = true
		return nil, false, failure("TRANSPORT_TIMEOUT")
	}
	if len(record) < HeaderSize || len(record) > HeaderSize+FragmentSize || string(record[:4]) != "WLBS" || record[4] != 1 || record[5] > 1 || binary.BigEndian.Uint16(record[6:8]) != HeaderSize {
		return bad()
	}
	if (record[5] == 1) != a.Response || !bytes.Equal(record[8:24], a.ID[:]) {
		return bad()
	}
	total := int(binary.BigEndian.Uint32(record[24:28]))
	off := int(binary.BigEndian.Uint32(record[28:32]))
	if total < 1 || total > MaxBody || off < 0 || off >= total || off%FragmentSize != 0 {
		return bad()
	}
	n := total - off
	if n > FragmentSize {
		n = FragmentSize
	}
	if len(record) != HeaderSize+n {
		return bad()
	}
	if a.started.IsZero() {
		a.started = now
		a.total = total
		a.body = make([]byte, total)
	} else if total != a.total {
		return bad()
	}
	index := off / FragmentSize
	if a.seen[index] {
		if !bytes.Equal(a.body[off:off+n], record[HeaderSize:]) {
			return bad()
		}
	} else {
		copy(a.body[off:], record[HeaderSize:])
		a.seen[index] = true
		a.count++
	}
	if a.count == (total+FragmentSize-1)/FragmentSize {
		return append([]byte(nil), a.body...), true, nil
	}
	return nil, false, nil
}
