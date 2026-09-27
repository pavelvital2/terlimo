package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tlsclient "github.com/bogdanfinn/tls-client"
)

func installTestSocketBinding(t *testing.T, bind func(int) error) {
	t.Helper()
	previous := managedSockets.Swap(&managedSocketBinding{bind: bind})
	t.Cleanup(func() { managedSockets.Store(previous) })
}

func TestManagedSocketBindingBeforeUDPAndTCPConnect(t *testing.T) {
	for _, network := range []string{"udp4", "tcp4"} {
		t.Run(network, func(t *testing.T) {
			var calls atomic.Int32
			installTestSocketBinding(t, func(fd int) error {
				calls.Add(1)
				if _, err := syscall.Getpeername(fd); err == nil {
					return errors.New("already connected before bind")
				}
				return nil
			})
			address := "127.0.0.1:9"
			if network == "tcp4" {
				listener, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				address = listener.Addr().String()
			}
			dialer := managedNetworkDialer(time.Second)
			conn, err := dialer.DialContext(context.Background(), network, address)
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			if calls.Load() != 1 {
				t.Fatalf("bind calls=%d", calls.Load())
			}
		})
	}
}

func TestManagedSocketBindingFailureNeverFallsBack(t *testing.T) {
	denied := errors.New("synthetic binding denied")
	var calls atomic.Int32
	installTestSocketBinding(t, func(int) error { calls.Add(1); return denied })
	for _, network := range []string{"udp4", "tcp4"} {
		dialer := managedNetworkDialer(time.Second)
		conn, err := dialer.DialContext(context.Background(), network, "127.0.0.1:9")
		if conn != nil || !errors.Is(err, denied) {
			t.Fatalf("unbound fallback: %v", err)
		}
	}
	listener := net.ListenConfig{Control: managedSocketControl}
	conn, err := listener.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if conn != nil || !errors.Is(err, denied) {
		t.Fatalf("unbound relay fallback: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("bind calls=%d", calls.Load())
	}
}

func TestManagedLoopbackBindingBeforeListenAndReturn(t *testing.T) {
	var calls atomic.Int32
	installTestSocketBinding(t, func(fd int) error {
		calls.Add(1)
		addr, err := syscall.Getsockname(fd)
		if err != nil {
			return err
		}
		if addr.(*syscall.SockaddrInet4).Port != 0 {
			return errors.New("relay already bound")
		}
		return nil
	})
	lc := net.ListenConfig{Control: managedSocketControl}
	relay, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peer.SetDeadline(time.Now().Add(time.Second))
	relay.SetDeadline(time.Now().Add(time.Second))
	if _, err = peer.WriteTo([]byte("request"), relay.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	_, addr, err := relay.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = relay.WriteTo([]byte("response"), addr); err != nil {
		t.Fatal(err)
	}
	n, _, err := peer.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "response" || calls.Load() != 1 {
		t.Fatalf("return/bind failure: %v", err)
	}
}

func TestManagedVKClientUsesSocketControl(t *testing.T) {
	var calls atomic.Int32
	installTestSocketBinding(t, func(int) error { calls.Add(1); return nil })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(),
		tlsclient.WithTimeoutSeconds(2), tlsclient.WithDialer(managedNetworkDialer(2*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	client.CloseIdleConnections()
	if response.StatusCode != 204 || calls.Load() != 1 {
		t.Fatalf("VK dialer bypassed binding: calls=%d", calls.Load())
	}
	// Both real provider constructors must retain the tested dependency option.
	for _, path := range []string{"creds.go", "creds_vkcalls.go"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "tlsclient.WithDialer(managedNetworkDialer(") {
			t.Fatalf("%s lacks binding", path)
		}
	}
}
