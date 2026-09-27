package main

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

func TestDeviceWGRelayKeepsStableSourceAndChunksRepliesAcrossPaths(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	relay, first, err := acquireDeviceWGRelay("test-device", listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.release(first)
	_, second, err := acquireDeviceWGRelay("test-device", listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.release(second)

	read := func() *net.UDPAddr {
		t.Helper()
		buf := make([]byte, 64)
		if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		_, addr, err := listener.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		return addr
	}
	if err := relay.writeFrom(first, []byte("first")); err != nil {
		t.Fatal(err)
	}
	firstSource := read()
	if err := relay.writeFrom(second, []byte("second")); err != nil {
		t.Fatal(err)
	}
	secondSource := read()
	if firstSource.Port != secondSource.Port {
		t.Fatalf("shared relay changed WireGuard source port: %d -> %d", firstSource.Port, secondSource.Port)
	}
	if _, err := listener.WriteToUDP([]byte("reply"), secondSource); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-first.downstream:
		if string(got) != "reply" {
			t.Fatalf("unexpected reply %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first scheduled path did not receive WireGuard reply")
	}
}

func TestDeviceWGRelayConcurrentAttachReadRemove(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	deviceID := fmt.Sprintf("race-test-%d", time.Now().UnixNano())
	relay, anchor, err := acquireDeviceWGRelay(deviceID, listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.release(anchor)
	const iterations = 2000
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < iterations; i++ {
			_, attachment, acquireErr := acquireDeviceWGRelay(deviceID, listener.LocalAddr().String())
			if acquireErr != nil {
				t.Errorf("acquire relay: %v", acquireErr)
				return
			}
			relay.release(attachment)
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < iterations; i++ {
			_ = relay.nextAttachment()
		}
	}()
	close(start)
	workers.Wait()
}
