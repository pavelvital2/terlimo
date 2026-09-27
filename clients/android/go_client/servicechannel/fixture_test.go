package servicechannel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"wg-turn-client/wlwire"
)

func newTestRequest(method, target string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	return http.NewRequest(method, target, reader)
}

type dummyAddr string

func (d dummyAddr) Network() string { return "fixture" }
func (d dummyAddr) String() string  { return string(d) }

// loopConn is a datagram-preserving in-memory net.Conn used to exercise the channel
// without any real socket. Closing either end terminates both.
type loopConn struct {
	in     chan []byte
	out    chan []byte
	closed chan struct{}
	once   sync.Once

	mu        sync.Mutex
	deadlines []time.Time
}

func newLoopPair() (*loopConn, *loopConn) {
	first, second := make(chan []byte, 64), make(chan []byte, 64)
	a := &loopConn{in: second, out: first, closed: make(chan struct{})}
	b := &loopConn{in: first, out: second, closed: make(chan struct{})}
	return a, b
}

func (c *loopConn) Read(p []byte) (int, error) {
	select {
	case msg := <-c.in:
		n := copy(p, msg)
		if n != len(msg) {
			return n, net.ErrClosed
		}
		return n, nil
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *loopConn) Write(p []byte) (int, error) {
	copied := append([]byte(nil), p...)
	select {
	case c.out <- copied:
		return len(p), nil
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *loopConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *loopConn) LocalAddr() net.Addr  { return dummyAddr("local") }
func (c *loopConn) RemoteAddr() net.Addr { return dummyAddr("remote") }

func (c *loopConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, deadline)
	c.mu.Unlock()
	return nil
}

func (c *loopConn) SetReadDeadline(deadline time.Time) error  { return c.SetDeadline(deadline) }
func (c *loopConn) SetWriteDeadline(deadline time.Time) error { return c.SetDeadline(deadline) }

func (c *loopConn) deadlineResets() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	resets := 0
	for _, deadline := range c.deadlines {
		if deadline.IsZero() {
			resets++
		}
	}
	return resets
}

func validSeed() Seed {
	return Seed{
		Version:           1,
		Revision:          "1",
		Environment:       "test",
		PeerIP:            "192.0.2.7",
		DTLSPort:          56000,
		DTLSSPKISHA256:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		ServiceClassifier: "synthetic-public-classifier",
		VKHashes:          []string{"synthetic-hash"},
		StreamID:          0,
	}
}

func seedJSON(t *testing.T, seed Seed) []byte {
	t.Helper()
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// fixtureResponse tells the fixture server how to answer one request.
type fixtureResponse struct {
	body      []byte
	rawFrames [][]byte
	block     bool
	keepOpen  bool
}

type fixtureEstablisher struct {
	t        *testing.T
	reply    func(frame requestFrame, id wlwire.ID) fixtureResponse
	mu       sync.Mutex
	dials    int
	seeds    []Seed
	received []requestFrame
	raws     [][]byte
	ids      []wlwire.ID
	invalid  []error
	conns    []*loopConn
}

func newFixtureEstablisher(t *testing.T, reply func(requestFrame, wlwire.ID) fixtureResponse) *fixtureEstablisher {
	t.Helper()
	return &fixtureEstablisher{t: t, reply: reply}
}

func (f *fixtureEstablisher) Establish(_ context.Context, seed Seed) (net.Conn, func(), error) {
	client, server := newLoopPair()
	f.mu.Lock()
	f.dials++
	f.seeds = append(f.seeds, seed)
	f.conns = append(f.conns, client, server)
	f.mu.Unlock()
	go f.serve(server)
	return client, func() { _ = client.Close() }, nil
}

func (f *fixtureEstablisher) serve(conn *loopConn) {
	defer conn.Close()
	assembler := wlwire.ServiceAssembler{}
	buf := make([]byte, readChunk)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		id, body, err := assembler.Add(buf[:n], time.Now())
		if err != nil {
			f.mu.Lock()
			f.invalid = append(f.invalid, err)
			f.mu.Unlock()
			return
		}
		if body == nil {
			continue
		}
		assembler = wlwire.ServiceAssembler{}
		var frame requestFrame
		if err := wlwire.StrictJSON(body, &frame); err != nil {
			f.mu.Lock()
			f.invalid = append(f.invalid, err)
			f.mu.Unlock()
			return
		}
		var validationErr error
		if _, err := validateRequest(body, id); err != nil {
			validationErr = err
		}
		f.mu.Lock()
		f.received = append(f.received, frame)
		f.raws = append(f.raws, append([]byte(nil), body...))
		f.ids = append(f.ids, id)
		f.invalid = append(f.invalid, validationErr)
		f.mu.Unlock()
		response := f.reply(frame, id)
		if response.block {
			continue
		}
		frames := response.rawFrames
		if frames == nil {
			if len(response.body) == 0 {
				return
			}
			frames, err = wlwire.ServiceFrames(id, true, response.body)
			if err != nil {
				return
			}
		}
		for _, frame := range frames {
			if _, err := conn.Write(frame); err != nil {
				return
			}
		}
		if !response.keepOpen && frame.SessionMode != "bounded" {
			return
		}
	}
}

func (f *fixtureEstablisher) snapshot() ([]requestFrame, [][]byte, []wlwire.ID, []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	received := append([]requestFrame(nil), f.received...)
	raws := append([][]byte(nil), f.raws...)
	ids := append([]wlwire.ID(nil), f.ids...)
	invalid := append([]error(nil), f.invalid...)
	return received, raws, ids, invalid
}

func (f *fixtureEstablisher) dialCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials
}

// fixtureDoer wires the established fixture channel into a Doer with a trusted origin.
func fixtureDoer(t *testing.T, fixture *fixtureEstablisher) *Doer {
	t.Helper()
	store := NewStore(Config{Environment: "test"})
	if err := store.Update(SourceBuiltin, seedJSON(t, validSeed())); err != nil {
		t.Fatal(err)
	}
	doer, err := NewDoer("https://mobile.example.test", store, NewChannel(fixture))
	if err != nil {
		t.Fatal(err)
	}
	return doer
}

func encodeRawURL(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

// responseJSON builds one well-formed response frame for an exact request identity.
func responseJSON(t *testing.T, id wlwire.ID, status int, headers map[string]string, body []byte) []byte {
	t.Helper()
	if body == nil {
		body = []byte{}
	}
	raw, err := json.Marshal(responseFrame{
		V:         1,
		RequestID: RequestID(id),
		Status:    status,
		Headers:   headers,
		BodyB64:   base64.RawURLEncoding.EncodeToString(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// errorJSON builds one well-formed transport error frame for an exact request identity.
func errorJSON(t *testing.T, id wlwire.ID, code string, retryable bool) []byte {
	t.Helper()
	raw, err := json.Marshal(errorFrame{
		V:         1,
		RequestID: RequestID(id),
		Error:     errorDetail{Code: code, Retryable: retryable},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
