package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"wg-turn-client/internal/wlwire"
)

// Record reader fixture only: the production assembler handles all frame validation.
// This is not a short-write or runtime missing-ME reproduction.
type serviceDiagRecords struct {
	frames    [][]byte
	terminal  error
	reads     int
	lengths   []int
	deadlines []time.Time
}

func (c *serviceDiagRecords) Read(b []byte) (int, error) {
	c.reads++
	c.lengths = append(c.lengths, len(b))
	if len(c.frames) == 0 {
		return 0, c.terminal
	}
	f := c.frames[0]
	c.frames = c.frames[1:]
	return copy(b, f), nil
}
func (*serviceDiagRecords) Write([]byte) (int, error)   { panic("unexpected Write") }
func (*serviceDiagRecords) Close() error                { return nil }
func (*serviceDiagRecords) LocalAddr() net.Addr         { return &net.UDPAddr{} }
func (*serviceDiagRecords) RemoteAddr() net.Addr        { return &net.UDPAddr{} }
func (*serviceDiagRecords) SetDeadline(time.Time) error { panic("unexpected SetDeadline") }
func (c *serviceDiagRecords) SetReadDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return nil
}
func (*serviceDiagRecords) SetWriteDeadline(time.Time) error { panic("unexpected SetWriteDeadline") }

func serviceDiagLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	old := log.Writer()
	flags := log.Flags()
	prefix := log.Prefix()
	log.SetOutput(&b)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() { log.SetOutput(old); log.SetFlags(flags); log.SetPrefix(prefix) })
	return &b
}

func TestServiceFrameDiagCodecParity(t *testing.T) {
	out := serviceDiagLog(t)
	var id wlwire.ID
	id[0] = 0x47
	id[15] = 0x19
	secret := "DIAG_SECRET_body_token_bearer_identity_address"
	payload := []byte(strings.Repeat(secret, 35))
	frames, err := wlwire.ServiceFrames(id, false, payload)
	if err != nil {
		t.Fatal(err)
	}
	conflict := append([]byte(nil), frames[0]...)
	conflict[32] ^= 1
	invalid := append([]byte(nil), frames[0]...)
	invalid[4] = 99
	timeout := &net.DNSError{Err: secret, IsTimeout: true}
	cases := []struct {
		name      string
		frames    [][]byte
		terminal  error
		wantErr   error
		result    string
		fragments int
	}{
		{"complete", frames, io.EOF, nil, "COMPLETE", len(frames)},
		{"partial_timeout", frames[:1], timeout, timeout, "TIMEOUT", 1},
		{"invalid", [][]byte{invalid}, io.EOF, wlwire.ErrMessage, "INVALID", 0},
		{"duplicates", append([][]byte{frames[0]}, frames...), io.EOF, nil, "COMPLETE", len(frames) + 1},
		{"conflicting_duplicate", [][]byte{frames[0], conflict}, io.EOF, wlwire.ErrMessage, "INVALID", 1},
		{"no_fragments_timeout", nil, timeout, timeout, "TIMEOUT", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var outcomes [2]struct {
				id   wlwire.ID
				body []byte
				err  error
			}
			var readers [2]*serviceDiagRecords
			for i := 0; i < 2; i++ {
				out.Reset()
				capture := newServiceFrameCapture(i == 1, time.Now())
				d := newServiceFrameDiag(capture, 9, 5)
				c := &serviceDiagRecords{frames: append([][]byte(nil), tc.frames...), terminal: tc.terminal}
				readers[i] = c
				gotID, body, e := serviceReadFrameDiag(c, d)
				outcomes[i].id = gotID
				outcomes[i].body = body
				outcomes[i].err = e
				if !errors.Is(e, tc.wantErr) {
					t.Fatalf("err=%v want=%v", e, tc.wantErr)
				}
				if len(c.deadlines) != 2 || c.deadlines[0].IsZero() || !c.deadlines[1].IsZero() {
					t.Fatal("read deadline parity")
				}
				for _, size := range c.lengths {
					if size != 1057 {
						t.Fatalf("buffer changed %d", size)
					}
				}
				if i == 0 {
					if out.Len() != 0 || d != nil {
						t.Fatal("OFF emitted or allocated capture")
					}
				} else {
					text := out.String()
					if !strings.Contains(text, "phase=terminal gen=9 seq=5 result="+tc.result) {
						t.Fatal(text)
					}
					if d.fragments != tc.fragments {
						t.Fatalf("fragments=%d", d.fragments)
					}
					if strings.Contains(text, secret) || strings.Contains(text, "bearer") || strings.Contains(text, "identity") || strings.Contains(text, "address") {
						t.Fatal("secret in diagnostic")
					}
					if tc.name == "invalid" && strings.Contains(text, "wire_id=4700") {
						t.Fatal("unvalidated wire ID emitted")
					}
					if tc.fragments > 0 && !strings.Contains(text, "wire_id=47000000000000000000000000000019") {
						t.Fatal("accepted ID absent")
					}
				}
			}
			if outcomes[0].id != outcomes[1].id || !bytes.Equal(outcomes[0].body, outcomes[1].body) || readers[0].reads != readers[1].reads {
				t.Fatal("ON/OFF codec/read parity")
			}
		})
	}
}

func TestServiceFrameDiagBudgetDoesNotLimitReads(t *testing.T) {
	out := serviceDiagLog(t)
	var id wlwire.ID
	frames, _ := wlwire.ServiceFrames(id, false, []byte("private"))
	for _, window := range []bool{false, true} {
		out.Reset()
		start := time.Now()
		if window {
			start = start.Add(-serviceFrameDiagWindow)
		}
		cap := newServiceFrameCapture(true, start)
		if !window {
			cap.lines = serviceFrameDiagLines
		}
		for i := 0; i < 2; i++ {
			c := &serviceDiagRecords{frames: frames, terminal: io.EOF}
			_, body, err := serviceReadFrameDiag(c, newServiceFrameDiag(cap, 1, i+1))
			if err != nil || string(body) != "private" {
				t.Fatal("capture cap changed product")
			}
		}
		if strings.Count(out.String(), "truncated=true") != 1 {
			t.Fatal(out.String())
		}
		reason := "LINE_LIMIT"
		if window {
			reason = "WINDOW_END"
		}
		if !strings.Contains(out.String(), reason) || strings.Contains(out.String(), "private") {
			t.Fatal(out.String())
		}
	}
}

func TestServiceFrameDiagWatcherLabels(t *testing.T) {
	out := serviceDiagLog(t)
	cases := []struct {
		err   error
		frame []byte
		want  string
		ok    bool
	}{
		{io.EOF, nil, "EOF", false},
		{&net.DNSError{Err: "secret", IsTimeout: true}, nil, "TIMEOUT", false},
		{context.Canceled, nil, "CANCELED", false},
		{net.ErrClosed, nil, "CLOSED", false},
		{errors.New("body_token_address_private"), nil, "OTHER", false},
		{nil, []byte{1}, "OK", true},
	}
	for _, tc := range cases {
		out.Reset()
		c := &serviceDiagRecords{terminal: tc.err}
		if tc.frame != nil {
			c.frames = [][]byte{tc.frame}
		}
		d := newServiceFrameDiag(newServiceFrameCapture(true, time.Now()), 9, 4)
		got := serviceWatcherRead(c, d)
		if got != tc.ok || c.reads != 1 || c.lengths[0] != 1 || len(c.deadlines) != 0 {
			t.Fatal("watcher behavior changed")
		}
		if !strings.Contains(out.String(), "phase=watcher_end gen=9 seq=4 result="+tc.want) || strings.Contains(out.String(), "private") {
			t.Fatal(out.String())
		}
	}
}

// Actual Pion error from a decrypted application record too large for watcher buffer.
func TestServiceFrameDiagPionWatcherBufferTooSmall(t *testing.T) {
	out := serviceDiagLog(t)
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := dtls.Listen("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &dtls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		if e = c.(*dtls.Conn).HandshakeContext(ctx); e != nil {
			done <- e
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		d := newServiceFrameDiag(newServiceFrameCapture(true, time.Now()), 9, 4)
		if serviceWatcherRead(c, d) {
			done <- errors.New("unexpected watcher success")
			return
		}
		done <- nil
	}()
	client, err := dtls.Dial("udp", listener.Addr().(*net.UDPAddr), &dtls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Write([]byte("SECRET_PION_DECRYPTED_BODY")); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !strings.Contains(out.String(), "result=BUFFER_TOO_SMALL") || strings.Contains(out.String(), "SECRET_PION") {
		t.Fatal(out.String())
	}
}

func TestServiceFrameDiagSequentialWatcherIntegration(t *testing.T) {
	out := serviceDiagLog(t)
	old := serviceFrameCapture
	serviceFrameCapture = newServiceFrameCapture(true, time.Now())
	defer func() { serviceFrameCapture = old }()
	// Existing real DTLS→Unix relay sequential scenario exercises the changed loop.
	TestServiceBoundedSequentialSession(t)
	text := out.String()
	for _, mark := range []string{"result=COMPLETE", "phase=watcher_begin", "phase=watcher_end", "reason=RELAY_DONE_DEADLINE"} {
		if !strings.Contains(text, mark) {
			t.Fatalf("missing %s: %s", mark, text)
		}
	}
}

func TestServiceFrameDiagOffHasNoDiagnosticAllocations(t *testing.T) {
	fixtureErr := errors.New("not formatted")
	allocs := testing.AllocsPerRun(100, func() {
		d := newServiceFrameDiag(nil, 9, 5)
		d.emit("read_end", serviceFrameDiagErrIfOn(d, fixtureErr), "NONE", 0)
	})
	if allocs != 0 {
		t.Fatalf("OFF diagnostic allocations=%v", allocs)
	}
}

func TestServiceFrameDiagConcurrentLineLimit(t *testing.T) {
	out := serviceDiagLog(t)
	c := newServiceFrameCapture(true, time.Now())
	d := newServiceFrameDiag(c, 9, 4)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 80; j++ {
				d.emit("read_begin", "WAIT", "NONE", 0)
			}
		}()
	}
	wg.Wait()
	if c.lines != serviceFrameDiagLines || strings.Count(out.String(), "[SVCFRAME]") != serviceFrameDiagLines+1 || strings.Count(out.String(), "truncated=true") != 1 {
		t.Fatal("global line cap not enforced")
	}
}
