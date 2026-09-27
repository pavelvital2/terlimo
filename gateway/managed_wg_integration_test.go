package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// In-memory TUN: real WireGuard crypto/UDP, no host interface, routes or privileges.
type memoryTUN struct {
	in, out chan []byte
	done    chan struct{}
	events  chan tun.Event
	once    sync.Once
}

func newMemoryTUN() *memoryTUN {
	return &memoryTUN{make(chan []byte, 4), make(chan []byte, 4), make(chan struct{}), make(chan tun.Event), sync.Once{}}
}
func (t *memoryTUN) File() *os.File           { return nil }
func (t *memoryTUN) MTU() (int, error)        { return 1420, nil }
func (t *memoryTUN) Name() (string, error)    { return "memory-only", nil }
func (t *memoryTUN) Events() <-chan tun.Event { return t.events }
func (t *memoryTUN) BatchSize() int           { return 1 }
func (t *memoryTUN) Close() error             { t.once.Do(func() { close(t.done); close(t.events) }); return nil }
func (t *memoryTUN) Read(p [][]byte, s []int, o int) (int, error) {
	select {
	case b := <-t.in:
		s[0] = copy(p[0][o:], b)
		return 1, nil
	case <-t.done:
		return 0, net.ErrClosed
	}
}
func (t *memoryTUN) Write(p [][]byte, o int) (int, error) {
	for _, b := range p {
		select {
		case t.out <- append([]byte(nil), b[o:]...):
		case <-t.done:
			return 0, net.ErrClosed
		}
	}
	return len(p), nil
}
func TestManagedRealWGWithMemoryTUN(t *testing.T) {
	st, ct := newMemoryTUN(), newMemoryTUN()
	sb, cb := &loopbackBind{}, &loopbackBind{}
	s := device.NewDevice(st, sb, device.NewLogger(device.LogLevelSilent, ""))
	defer s.Close()
	c := device.NewDevice(ct, cb, device.NewLogger(device.LogLevelSilent, ""))
	defer c.Close()
	spr, spu, _ := generateKeyPair()
	cpr, cpu, _ := generateKeyPair()
	sh, _ := b64ToHex(spr)
	ch, _ := b64ToHex(cpr)
	sph, _ := b64ToHex(spu)
	cph, _ := b64ToHex(cpu)
	if e := s.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=0\npublic_key=%s\nallowed_ip=10.67.67.2/32\n", sh, cph)); e != nil {
		t.Fatal(e)
	}
	if e := s.Up(); e != nil {
		t.Fatal(e)
	}
	sb.mu.Lock()
	port := sb.socket.LocalAddr().(*net.UDPAddr).Port
	sb.mu.Unlock()
	if e := c.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=0\npublic_key=%s\nallowed_ip=0.0.0.0/0\nendpoint=127.0.0.1:%d\n", ch, sph, port)); e != nil {
		t.Fatal(e)
	}
	if e := c.Up(); e != nil {
		t.Fatal(e)
	}
	packet := make([]byte, 28)
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], 28)
	packet[8] = 64
	packet[9] = 17
	copy(packet[12:16], []byte{10, 67, 67, 2})
	copy(packet[16:20], []byte{192, 0, 2, 1})
	copy(packet[20:], []byte("offline!"))
	ct.in <- packet
	select {
	case got := <-st.out:
		if !bytes.Equal(got, packet) {
			t.Fatal("decrypted packet mismatch")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("real WG handshake/data timeout")
	}
	if e := s.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", cph)); e != nil {
		t.Fatal(e)
	}
	ct.in <- packet
	select {
	case <-st.out:
		t.Fatal("removed peer still delivered")
	case <-time.After(100 * time.Millisecond):
	}
}
