package wlwire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestVectors(t *testing.T) {
	b, e := os.ReadFile("../../testdata/wl_wire_vectors_v2.json")
	if e != nil {
		t.Fatal(e)
	}
	var doc struct {
		Vectors []struct {
			Kind      string   `json:"kind"`
			Key       string   `json:"public_key_spki"`
			RID       string   `json:"request_id_hex"`
			Challenge string   `json:"challenge_id"`
			Nonce     string   `json:"nonce"`
			Exporter  string   `json:"synthetic_exporter_hex"`
			Payload   string   `json:"payload_b64"`
			T         string   `json:"transcript_hex"`
			Proof     string   `json:"proof_b64"`
			Frames    []string `json:"frames_hex"`
		}
	}
	if e = json.Unmarshal(b, &doc); e != nil {
		t.Fatal(e)
	}
	for _, v := range doc.Vectors {
		key, _ := baseDecode(v.Key)
		p, _ := baseDecode(v.Payload)
		proof, _ := baseDecode(v.Proof)
		c, _ := Decode(v.Challenge, 16)
		n, _ := Decode(v.Nonce, 32)
		binding, _ := hex.DecodeString(v.RID)
		domain := "WLBS-POP-1"
		if v.Kind == "vpn" {
			domain = "WL-VPN-POP-1"
			binding, _ = hex.DecodeString(v.Exporter)
		}
		tr := Transcript(domain, binding, c, n, p)
		expected, _ := hex.DecodeString(v.T)
		if !bytes.Equal(tr, expected) {
			t.Fatal("transcript mismatch")
		}
		if e = Verify(key, tr, proof); e != nil {
			t.Fatal(e)
		}
		tr[15] ^= 1
		if Verify(key, tr, proof) == nil {
			t.Fatal("accepted changed binding")
		}
		var a Assembler
		for _, h := range v.Frames {
			frame, _ := hex.DecodeString(h)
			_, body, e := a.Add(frame, time.Now())
			if e != nil {
				t.Fatal(e)
			}
			if body != nil && !json.Valid(body) {
				t.Fatal("body")
			}
		}
	}
}
func baseDecode(s string) ([]byte, error) { return Decode(s, lenMust(s)) }
func lenMust(s string) int                { return len(s) * 6 / 8 }
func TestBoundsAndDuplicates(t *testing.T) {
	b := bytes.Repeat([]byte("a"), MaxBody)
	fs, _ := Frames(ID{}, false, b)
	var a Assembler
	now := time.Now()
	if _, _, e := a.Add(fs[0], now); e != nil {
		t.Fatal(e)
	}
	if _, _, e := a.Add(fs[0], now.Add(10*time.Second)); e == nil {
		t.Fatal("deadline extended")
	}
	a = Assembler{}
	for i := len(fs) - 1; i >= 0; i-- {
		_, out, e := a.Add(fs[i], now)
		if e != nil {
			t.Fatal(e)
		}
		if i == 0 && !bytes.Equal(out, b) {
			t.Fatal("reassembly")
		}
	}
	fs[0][32] ^= 1
	if _, _, e := a.Add(fs[0], now); e == nil {
		t.Fatal("conflicting duplicate")
	}
	if _, e := Frames(ID{}, false, make([]byte, MaxBody+1)); e == nil {
		t.Fatal("size")
	}
}
func TestStrict(t *testing.T) {
	for _, s := range []string{`{"a":1,"a":2}`, `{"a":{"x":1,"x":2}}`, `{} {}`, `{"a":NaN}`} {
		var out map[string]any
		if StrictJSON([]byte(s), &out) == nil {
			t.Fatal(s)
		}
	}
}
