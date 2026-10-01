package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/transport/v4/packetio"
	"golang.org/x/net/ipv4"
)

func preacceptTestListener(o *udpPreacceptObserver) *opportunisticUDPListener {
	return &opportunisticUDPListener{observer: o, accepting: true, acceptCh: make(chan *opportunisticUDPConn, 1), doneCh: make(chan struct{}), readDoneCh: make(chan struct{}), conns: make(map[string]*opportunisticUDPConn)}
}
func preacceptTestAddr(port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: port}
}
func TestUDPPreacceptOFFAndTriggerDelivery(t *testing.T) {
	t.Setenv(udpPreacceptFlag, "")
	if newUDPPreacceptObserver() != nil {
		t.Fatal("default must be OFF")
	}
	for _, on := range []bool{false, true} {
		for _, before := range []bool{false, true} {
			var o *udpPreacceptObserver
			if on {
				o = &udpPreacceptObserver{start: time.Now()}
			}
			l := preacceptTestListener(o)
			addr := preacceptTestAddr(9)
			payload := []byte("first triggering datagram")
			var c *opportunisticUDPConn
			if before {
				var rx time.Time
				hash := ""
				if on {
					rx = time.Now()
					hash = o.receive(addr, rx)
				}
				var ok bool
				c, ok, _ = l.connectionFor(addr, payload, hash, rx)
				if !ok {
					t.Fatal("admission")
				}
				got, _, err := l.Accept()
				if err != nil || got != c {
					t.Fatal("accept before write")
				}
				finished := make(chan []byte, 1)
				go func() {
					buf := make([]byte, 64)
					n, _, e := c.ReadFrom(buf)
					if e == nil {
						finished <- buf[:n]
					} else {
						finished <- nil
					}
				}()
				n, e := c.buffer.Write(payload)
				if n != len(payload) || e != nil {
					t.Fatal("trigger write")
				}
				if on {
					o.record(hash, c.observerSeq, udpPreacceptBufferOutcome(e), rx)
				}
				select {
				case got := <-finished:
					if !bytes.Equal(got, payload) {
						t.Fatal("trigger not delivered")
					}
				case <-time.After(time.Second):
					t.Fatal("lost wakeup")
				}
			} else {
				l.dispatchMessage(addr, payload)
				got, _, err := l.Accept()
				if err != nil {
					t.Fatal(err)
				}
				c = got.(*opportunisticUDPConn)
				buf := make([]byte, 64)
				n, _, e := c.ReadFrom(buf)
				if e != nil || !bytes.Equal(buf[:n], payload) {
					t.Fatal("trigger after write")
				}
			}
			if !on && (c.observerSeq != 0 || !c.observerRX.IsZero() || c.observerHash != "") {
				t.Fatal("OFF observer work")
			}
			c.SetReadDeadline(time.Now())
			if n, _, e := c.ReadFrom(make([]byte, 64)); n != 0 || e == nil {
				t.Fatal("trigger delivered twice")
			}
			c.Close()
		}
	}
}
func TestUDPPreacceptDispositionsAndHash(t *testing.T) {
	o := &udpPreacceptObserver{start: time.Now()}
	l := preacceptTestListener(o)
	a := preacceptTestAddr(9)
	h := sha256.Sum256([]byte("whitelist-dtls-join-v1|192.0.2.1|9"))
	if udpPreacceptEndpointHash(a) != hex.EncodeToString(h[:]) {
		t.Fatal("canonical hash mismatch")
	}
	l.acceptFilter = func([]byte) bool { return false }
	l.dispatchMessage(a, []byte("rejected"))
	if len(l.acceptCh) != 0 {
		t.Fatal("filter parity")
	}
	l.acceptFilter = nil
	l.accepting = false
	l.dispatchMessage(a, []byte("not accepting"))
	l.accepting = true
	l.dispatchMessage(a, []byte("first"))
	c := l.conns[a.String()]
	firstSeq := c.observerSeq
	c.buffer.SetLimitCount(1)
	l.dispatchMessage(a, []byte("full"))
	// Buffer close without conn.closed simulates close between lookup and Write.
	c.buffer.Close()
	l.dispatchMessage(a, []byte("closed"))
	l.dispatchMessage(preacceptTestAddr(10), []byte("backlog"))
	got, _, e := l.Accept()
	if e != nil {
		t.Fatal(e)
	}
	got.Close()
	l.dispatchMessage(a, []byte("same endpoint new conn"))
	next, _, e := l.Accept()
	if e != nil {
		t.Fatal(e)
	}
	n := next.(*opportunisticUDPConn)
	if n.observerSeq == firstSeq || n.observerHash != c.observerHash {
		t.Fatal("endpoint is not conn sequence")
	}
	next.Close()
	for _, want := range []string{"FILTER_DROP", "NOT_ACCEPTING", "NEW_QUEUED", "EXISTING", "BACKLOG_DROP", "BUFFER_FULL", "BUFFER_CLOSED", "ACCEPT_DEQUEUE"} {
		found := false
		for _, v := range o.events[:o.eventN] {
			if v.Stage == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s", want)
		}
	}
	if udpPreacceptBufferOutcome(errors.New("other")) != "BUFFER_OTHER" || udpPreacceptBufferOutcome(packetio.ErrFull) != "BUFFER_FULL" || udpPreacceptBufferOutcome(io.ErrClosedPipe) != "BUFFER_CLOSED" {
		t.Fatal("error enum")
	}
}
func TestUDPPreacceptBoundsSummaryAndConcurrency(t *testing.T) {
	var summaries []udpPreacceptSummary
	var sinkMu sync.Mutex
	o := &udpPreacceptObserver{start: time.Now(), sink: func(s udpPreacceptSummary) { sinkMu.Lock(); summaries = append(summaries, s); sinkMu.Unlock() }}
	o.summary("START", false)
	for i := 0; i < 9; i++ {
		o.receive(preacceptTestAddr(i), time.Now())
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 32; j++ {
				rx := time.Now()
				h := o.receive(preacceptTestAddr(0), rx)
				o.record(h, o.sequence(), "BUFFER_OK", rx)
				o.summary("CONN_CLOSE", false)
			}
		}()
	}
	wg.Wait()
	o.summary("END_LISTENER_CLOSE", true)
	last := summaries[len(summaries)-1]
	if len(last.Events) != 64 || o.peerN != 8 || last.PeerOverflow == 0 || last.EventOverflow == 0 || last.Omitted == 0 || last.Complete || !last.Ended || last.SummaryOmitted == 0 {
		t.Fatalf("bounds %+v", last)
	}
	if len(summaries) > 10 {
		t.Fatal("unbounded summaries")
	}
	if last.MissingRX == "" {
		t.Fatal("missing unknown interpretation")
	}
	for _, v := range last.Events {
		if v.MonoNS < 0 || v.RXSpanNS < 0 {
			t.Fatal("negative monotonic span")
		}
	}
	clean := &udpPreacceptObserver{start: time.Now(), sink: func(s udpPreacceptSummary) { last = s }}
	clean.summary("START", false)
	if last.Complete {
		t.Fatal("open capture cannot prove absence")
	}
	clean.summary("END_LISTENER_CLOSE", true)
	if !last.Complete || last.RX != 0 {
		t.Fatal("empty intact userspace capture")
	}
	clean.receive(preacceptTestAddr(1), time.Now())
	clean.summary("CONN_CLOSE", false)
	if last.Complete || last.AfterEnd != 1 {
		t.Fatal("post-end observations must be omitted")
	}
}

type preacceptBatchReader struct{}

func (preacceptBatchReader) ReadBatch(ms []ipv4.Message, _ int) (int, error) {
	for i := 0; i < 2; i++ {
		copy(ms[i].Buffers[0], []byte("batch"))
		ms[i].N = 5
		ms[i].Addr = preacceptTestAddr(20 + i)
	}
	return 2, io.EOF
}
func TestUDPPreacceptBatchTimestampAndReadError(t *testing.T) {
	o := &udpPreacceptObserver{start: time.Now()}
	l := preacceptTestListener(o)
	l.acceptCh = make(chan *opportunisticUDPConn, 4)
	l.batchRead = preacceptBatchReader{}
	l.readBatchSize = 2
	if e := l.readBatchLoop(); !errors.Is(e, io.EOF) {
		t.Fatal(e)
	}
	var times []int64
	for _, e := range o.events[:o.eventN] {
		if e.Stage == "RX" {
			times = append(times, e.MonoNS)
		}
	}
	if len(times) != 2 || times[0] != times[1] {
		t.Fatal("RX must share batch return timestamp")
	}
	var summary udpPreacceptSummary
	o.sink = func(s udpPreacceptSummary) { summary = s }
	o.summary("READ_ERROR", true)
	if summary.Complete || !summary.ReadError || !summary.Ended {
		t.Fatal("read error cannot claim completeness")
	}
}
func TestUDPPreacceptAdmissionOFFParity(t *testing.T) {
	for _, on := range []bool{false, true} {
		var o *udpPreacceptObserver
		if on {
			o = &udpPreacceptObserver{start: time.Now()}
		}
		l := preacceptTestListener(o)
		a := preacceptTestAddr(9)
		l.acceptFilter = func([]byte) bool { return false }
		l.dispatchMessage(a, []byte("bad"))
		if len(l.acceptCh) != 0 || len(l.conns) != 0 {
			t.Fatal("filter")
		}
		l.acceptFilter = nil
		l.accepting = false
		l.dispatchMessage(a, []byte("stop"))
		if len(l.acceptCh) != 0 {
			t.Fatal("not accepting")
		}
		l.accepting = true
		l.dispatchMessage(a, []byte("trigger"))
		l.dispatchMessage(preacceptTestAddr(10), []byte("backlog"))
		if len(l.conns) != 1 || len(l.acceptCh) != 1 {
			t.Fatal("backlog altered")
		}
		c := l.conns[a.String()]
		c.buffer.SetLimitCount(1)
		l.dispatchMessage(a, []byte("full"))
		buf := make([]byte, 64)
		n, _, err := c.ReadFrom(buf)
		if err != nil || string(buf[:n]) != "trigger" {
			t.Fatal("full altered initial payload")
		}
		c.buffer.Close()
		l.dispatchMessage(a, []byte("closed"))
		if _, _, err := c.ReadFrom(buf); !errors.Is(err, io.EOF) {
			t.Fatal("closed outcome altered")
		}
	}
}
