package main

import (
	"encoding/binary"
	"errors"
	"golang.zx2c4.com/wireguard/conn"
	"net"
	"net/netip"
	"sync"
)

// Explicit UDP4 loopback bind; no wildcard socket and no IPv6 fallback.
type loopbackBind struct {
	mu     sync.Mutex
	socket *net.UDPConn
}
type loopbackEndpoint struct{ addr netip.AddrPort }

func (e *loopbackEndpoint) ClearSrc()           {}
func (e *loopbackEndpoint) SrcToString() string { return "" }
func (e *loopbackEndpoint) DstToString() string { return e.addr.String() }
func (e *loopbackEndpoint) DstToBytes() []byte {
	b := e.addr.Addr().AsSlice()
	return binary.BigEndian.AppendUint16(b, e.addr.Port())
}
func (e *loopbackEndpoint) DstIP() netip.Addr { return e.addr.Addr() }
func (e *loopbackEndpoint) SrcIP() netip.Addr { return netip.Addr{} }
func (b *loopbackBind) BatchSize() int        { return 1 }
func (b *loopbackBind) SetMark(mark uint32) error {
	if mark != 0 {
		return errors.New("MANAGED_MARK_UNSUPPORTED")
	}
	return nil
}
func (b *loopbackBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	a, e := netip.ParseAddrPort(s)
	if e != nil || a.Addr() != netip.MustParseAddr("127.0.0.1") || a.Port() == 0 {
		return nil, errors.New("MANAGED_ENDPOINT_INVALID")
	}
	return &loopbackEndpoint{a}, nil
}
func (b *loopbackBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.socket != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	s, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
	if err != nil {
		return nil, 0, err
	}
	b.socket = s
	recv := func(p [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		n, a, err := s.ReadFromUDPAddrPort(p[0])
		if err != nil {
			return 0, err
		}
		if a.Addr() != netip.MustParseAddr("127.0.0.1") {
			return 0, nil
		}
		sizes[0] = n
		eps[0] = &loopbackEndpoint{a}
		return 1, nil
	}
	return []conn.ReceiveFunc{recv}, uint16(s.LocalAddr().(*net.UDPAddr).Port), nil
}
func (b *loopbackBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.socket == nil {
		return nil
	}
	err := b.socket.Close()
	b.socket = nil
	return err
}
func (b *loopbackBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	e, ok := ep.(*loopbackEndpoint)
	if !ok || e.addr.Addr() != netip.MustParseAddr("127.0.0.1") {
		return conn.ErrWrongEndpointType
	}
	b.mu.Lock()
	s := b.socket
	b.mu.Unlock()
	if s == nil {
		return net.ErrClosed
	}
	for _, p := range bufs {
		if _, err := s.WriteToUDPAddrPort(p, e.addr); err != nil {
			return err
		}
	}
	return nil
}
