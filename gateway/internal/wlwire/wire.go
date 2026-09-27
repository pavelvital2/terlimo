// Package wlwire implements the bounded TEST WLBS framing and signing transcript.
package wlwire

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const MaxBody = 16384
const Fragment = 1024

var ErrMessage = errors.New("BAD_MESSAGE")
var ErrProof = errors.New("PROOF_INVALID")

type ID [16]byte

func Decode(s string, n int) ([]byte, error) {
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	if e != nil || len(b) != n {
		return nil, ErrMessage
	}
	return b, nil
}
func Encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func Frames(id ID, response bool, body []byte) ([][]byte, error) {
	if len(body) < 1 || len(body) > MaxBody {
		return nil, ErrMessage
	}
	var out [][]byte
	for off := 0; off < len(body); off += Fragment {
		end := off + Fragment
		if end > len(body) {
			end = len(body)
		}
		b := make([]byte, 32+end-off)
		copy(b, "WLBS")
		b[4] = 1
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

type Assembler struct {
	started   time.Time
	id        ID
	direction byte
	total     int
	parts     map[int][]byte
}

func (a *Assembler) Add(b []byte, now time.Time) (ID, []byte, error) {
	var id ID
	if len(b) < 33 || string(b[:4]) != "WLBS" || b[4] != 1 || b[5] > 1 || binary.BigEndian.Uint16(b[6:]) != 32 {
		return id, nil, ErrMessage
	}
	copy(id[:], b[8:24])
	total := int(binary.BigEndian.Uint32(b[24:28]))
	off := int(binary.BigEndian.Uint32(b[28:32]))
	size := total - off
	if size > Fragment {
		size = Fragment
	}
	if total < 1 || total > MaxBody || off < 0 || off >= total || off%Fragment != 0 || len(b)-32 != size {
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
	if len(a.parts) != (total+Fragment-1)/Fragment {
		return id, nil, nil
	}
	out := make([]byte, total)
	for off, part := range a.parts {
		copy(out[off:], part)
	}
	return id, out, nil
}

// StrictJSON rejects duplicate object keys at every nesting depth and trailing data.
func StrictJSON(b []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := value(d); err != nil {
		return ErrMessage
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrMessage
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrMessage
	}
	return nil
}
func value(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	v, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch v {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return ErrMessage
			}
			seen[s] = true
			if e = value(d); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e = value(d); e != nil {
				return e
			}
		}
	default:
		return ErrMessage
	}
	_, e = d.Token()
	return e
}
func PublicKey(spki []byte) (*ecdsa.PublicKey, error) {
	k, e := x509.ParsePKIXPublicKey(spki)
	if e != nil {
		return nil, ErrProof
	}
	p, ok := k.(*ecdsa.PublicKey)
	if !ok || p.Curve != elliptic.P256() {
		return nil, ErrProof
	}
	return p, nil
}
func Transcript(domain string, binding, challenge, nonce, payload []byte) []byte {
	h := sha256.Sum256(payload)
	t := append([]byte(domain+"\x00"), binding...)
	t = append(t, challenge...)
	t = append(t, nonce...)
	return append(t, h[:]...)
}
func Verify(spki, transcript, sig []byte) error {
	p, e := PublicKey(spki)
	if e != nil {
		return e
	}
	h := sha256.Sum256(transcript)
	if !ecdsa.VerifyASN1(p, h[:], sig) {
		return ErrProof
	}
	return nil
}
