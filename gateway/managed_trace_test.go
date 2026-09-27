package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagedTraceRedactsAndKeepsFirstExit(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	trace := newManagedConnTrace()
	trace.exit("test_read", errors.New("fixture-secret private-address arbitrary payload"))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); trace.exit("later_close", net.ErrClosed) }()
	}
	wg.Wait()
	out := buf.String()
	if strings.Count(out, "[MANAGED_TRACE]") != 1 || !strings.Contains(out, "stage=test_read class=other") {
		t.Fatalf("unexpected terminal events: %q", out)
	}
	for _, secret := range []string{"fixture-secret", "private-address", "payload", "later_close"} {
		if strings.Contains(out, secret) {
			t.Fatal("unallowlisted content or duplicate terminal escaped")
		}
	}
}
func TestManagedTraceErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "none"}, {io.EOF, "eof"}, {net.ErrClosed, "closed"}, {context.Canceled, "cancelled"}, {context.DeadlineExceeded, "timeout"},
		{errors.New("LEASE_EXPIRED"), "LEASE_EXPIRED"}, {errors.New("AUTH_REQUIRED secret"), "other"},
		{&net.OpError{Op: "read", Addr: &net.IPAddr{IP: net.IPv4(192, 0, 2, 1)}, Err: context.DeadlineExceeded}, "timeout"},
	} {
		if got := managedErrorClass(tc.err); got != tc.want {
			t.Errorf("class got %s want %s", got, tc.want)
		}
	}
}
func TestManagedTraceDeadlineForwarding(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	c := &clientTestConn{Conn: server, trace: newManagedConnTrace()}
	if err := c.SetDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := server.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatal("deadline was not forwarded")
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := client.Write([]byte{1}); done <- e }()
	if n, e := server.Read(make([]byte, 1)); n != 1 || e != nil {
		t.Fatal("deadline clear changed forwarding")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
