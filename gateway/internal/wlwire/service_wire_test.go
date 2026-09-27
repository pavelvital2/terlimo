package wlwire

import (
	"bytes"
	"testing"
	"time"
)

func svcID(n byte) ID {
	var id ID
	id[0] = n
	id[15] = 0xA5
	return id
}

func TestServiceFramesRoundTripAndIsolation(t *testing.T) {
	for _, size := range []int{1, 5, 1024, 1025, 16384, 65536, 300 * 1024} {
		body := bytes.Repeat([]byte{0x7b}, size)
		fs, err := ServiceFrames(svcID(1), false, body)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		var a ServiceAssembler
		var got []byte
		for _, f := range fs {
			id, out, err := a.Add(f, time.Now())
			if err != nil {
				t.Fatalf("size %d: %v", size, err)
			}
			if out != nil {
				if id != svcID(1) || !bytes.Equal(out, body) {
					t.Fatalf("size %d: mismatch", size)
				}
				got = out
			}
		}
		if got == nil {
			t.Fatalf("size %d: no reassembly", size)
		}
	}
	// Version isolation: service assembler rejects WLBS v1 and vice versa.
	v1, _ := Frames(svcID(2), false, []byte("hello"))
	var sa ServiceAssembler
	if _, _, err := sa.Add(v1[0], time.Now()); err == nil {
		t.Fatal("service assembler accepted a v1 frame")
	}
	v2, _ := ServiceFrames(svcID(2), false, []byte("hello"))
	var wrap Assembler
	if _, _, err := wrap.Add(v2[0], time.Now()); err == nil {
		t.Fatal("WLBS assembler accepted a v2 frame")
	}
}

func TestServiceAssemblerReorderedDuplicateMissingTruncated(t *testing.T) {
	body := bytes.Repeat([]byte{0x01, 0x02, 0x03}, 5000) // 15000 bytes, 15 fragments
	fs, err := ServiceFrames(svcID(3), false, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) < 10 {
		t.Fatalf("expected fragments, got %d", len(fs))
	}
	// Reordered with a duplicate of the first fragment.
	var a ServiceAssembler
	order := append([][]byte{}, fs[5:]...)
	order = append(order, fs[0], fs[0], fs[1], fs[2], fs[3], fs[4])
	var got []byte
	for _, f := range order {
		_, out, err := a.Add(f, time.Now())
		if err != nil {
			t.Fatalf("reordered add: %v", err)
		}
		if out != nil {
			got = out
		}
	}
	if !bytes.Equal(got, body) {
		t.Fatal("reordered assembly mismatch")
	}
	// Conflicting duplicate is rejected.
	b := &ServiceAssembler{}
	if _, _, err := b.Add(fs[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	bad := bytes.Clone(fs[0])
	bad[32] ^= 0xFF
	if _, _, err := b.Add(bad, time.Now()); err == nil {
		t.Fatal("conflicting duplicate accepted")
	}
	// Missing fragment: never completes.
	c := &ServiceAssembler{}
	for _, f := range fs[1:] {
		_, out, err := c.Add(f, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if out != nil {
			t.Fatal("completed without the first fragment")
		}
	}
	// Truncated fragment (short payload) is rejected.
	d := &ServiceAssembler{}
	trunc := fs[0][:len(fs[0])-1]
	if _, _, err := d.Add(trunc, time.Now()); err == nil {
		t.Fatal("truncated fragment accepted")
	}
}

func TestServiceAssemblerCapsBeforeAllocation(t *testing.T) {
	// Claiming a total above ServiceMaxFrame must be rejected from the header alone.
	h := make([]byte, 32+ServiceFragment)
	copy(h, "WLBS")
	h[4] = ServiceVersion
	h[6], h[7] = 0, 32
	binaryPutUint32(h[24:], uint32(ServiceMaxFrame+1))
	binaryPutUint32(h[28:], 0)
	var a ServiceAssembler
	if _, _, err := a.Add(h, time.Now()); err == nil {
		t.Fatal("oversize total accepted")
	}
	if a.parts != nil {
		t.Fatal("assembler allocated state for an oversize frame")
	}
	// ServiceFrames itself refuses an oversize body.
	if _, err := ServiceFrames(svcID(4), false, make([]byte, ServiceMaxFrame+1)); err == nil {
		t.Fatal("oversize body encoded")
	}
}

func binaryPutUint32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}
