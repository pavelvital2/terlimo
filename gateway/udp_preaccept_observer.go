package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pion/transport/v4/packetio"
)

const udpPreacceptFlag = "TERLIMO_UDP_PREACCEPT_PROBE"
const udpPreacceptPeerCap = 8
const udpPreacceptEventCap = 64

type udpPreacceptEvent struct {
	Hash     string `json:"endpoint_hash"`
	Conn     uint64 `json:"conn_seq"`
	Stage    string `json:"stage"`
	MonoNS   int64  `json:"capture_elapsed_ns"`
	RXSpanNS int64  `json:"rx_span_ns"`
}
type udpPreacceptSummary struct {
	Reason         string              `json:"reason"`
	StartUTC       string              `json:"start_utc"`
	EndUTC         string              `json:"summary_utc"`
	Ended          bool                `json:"capture_ended"`
	ReadError      bool                `json:"read_error"`
	Complete       bool                `json:"ledger_complete"`
	RX             uint64              `json:"rx_count"`
	Omitted        uint64              `json:"omitted_events"`
	PeerOverflow   uint64              `json:"peer_overflow_rx"`
	EventOverflow  uint64              `json:"event_overflow"`
	AfterEnd       uint64              `json:"after_end_rx"`
	SummaryOmitted uint64              `json:"omitted_conn_summaries"`
	Counters       map[string]uint64   `json:"counters"`
	Events         []udpPreacceptEvent `json:"events"`
	MissingRX      string              `json:"missing_rx_interpretation"`
}

// One listener-owned bounded ledger. Never holds raw endpoint or payload.
type udpPreacceptObserver struct {
	mu                                                 sync.Mutex
	start                                              time.Time
	ended                                              bool
	readError                                          bool
	peers                                              [udpPreacceptPeerCap]string
	peerN                                              int
	events                                             [udpPreacceptEventCap]udpPreacceptEvent
	eventN                                             int
	nextSeq                                            uint64
	rx, omitted, peerOverflow, eventOverflow, afterEnd uint64
	summaryN, summaryOmitted                           uint64
	counters                                           [12]uint64
	sink                                               func(udpPreacceptSummary)
}

var udpPreacceptStages = [12]string{"RX", "EXISTING", "NEW_QUEUED", "FILTER_DROP", "NOT_ACCEPTING", "BACKLOG_DROP", "BUFFER_OK", "BUFFER_FULL", "BUFFER_CLOSED", "BUFFER_OTHER", "ACCEPT_DEQUEUE", "CONN_CLOSE"}

func newUDPPreacceptObserver() *udpPreacceptObserver {
	if os.Getenv(udpPreacceptFlag) != "1" {
		return nil
	}
	o := &udpPreacceptObserver{start: time.Now(), sink: func(s udpPreacceptSummary) { b, _ := json.Marshal(s); log.Printf("[UDPPREACCEPT] %s", b) }}
	o.summary("START", false)
	return o
}
func udpPreacceptEndpointHash(addr net.Addr) string {
	a, ok := addr.(*net.UDPAddr)
	if !ok || a == nil {
		return ""
	}
	ip := a.IP.String()
	if a.IP == nil {
		return ""
	}
	// Canonical IP follows UDPAddr.String (also used by existing endpoint joins).
	h := sha256.Sum256([]byte("whitelist-dtls-join-v1|" + ip + "|" + strconv.Itoa(a.Port)))
	return hex.EncodeToString(h[:])
}
func (o *udpPreacceptObserver) receive(addr net.Addr, at time.Time) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rx++
	o.counters[0]++
	if o.ended {
		o.afterEnd++
		o.omitted++
		return ""
	}
	if o.eventN == udpPreacceptEventCap {
		o.eventOverflow++
		o.omitted++
		return ""
	}
	hash := udpPreacceptEndpointHash(addr)
	if hash == "" {
		o.omitted++
		return ""
	}
	found := false
	for i := 0; i < o.peerN; i++ {
		if o.peers[i] == hash {
			found = true
			break
		}
	}
	if !found {
		if o.peerN == udpPreacceptPeerCap {
			o.peerOverflow++
			o.omitted++
			return ""
		}
		o.peers[o.peerN] = hash
		o.peerN++
	}
	o.appendLocked(hash, 0, "RX", at, at)
	return hash
}
func (o *udpPreacceptObserver) sequence() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.nextSeq++
	return o.nextSeq
}
func (o *udpPreacceptObserver) appendLocked(hash string, seq uint64, stage string, at, rx time.Time) {
	if o.eventN == udpPreacceptEventCap {
		o.eventOverflow++
		o.omitted++
		return
	}
	o.events[o.eventN] = udpPreacceptEvent{Hash: hash, Conn: seq, Stage: stage, MonoNS: at.Sub(o.start).Nanoseconds(), RXSpanNS: at.Sub(rx).Nanoseconds()}
	o.eventN++
}
func (o *udpPreacceptObserver) record(hash string, seq uint64, stage string, rx time.Time) {
	if o == nil {
		return
	}
	at := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, s := range udpPreacceptStages {
		if s == stage {
			o.counters[i]++
			break
		}
	}
	if o.ended || hash == "" {
		o.omitted++
		return
	}
	o.appendLocked(hash, seq, stage, at, rx)
}
func (o *udpPreacceptObserver) summary(reason string, end bool) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if reason == "READ_ERROR" {
		o.readError = true
	}
	if end && o.ended {
		o.mu.Unlock()
		return
	}
	if reason == "CONN_CLOSE" {
		if o.summaryN == 8 {
			o.summaryOmitted++
			o.mu.Unlock()
			return
		}
		o.summaryN++
	}
	if end {
		o.ended = true
	}
	counts := make(map[string]uint64, len(udpPreacceptStages))
	for i, s := range udpPreacceptStages {
		counts[s] = o.counters[i]
	}
	events := append([]udpPreacceptEvent(nil), o.events[:o.eventN]...)
	s := udpPreacceptSummary{Reason: reason, StartUTC: o.start.UTC().Format(time.RFC3339Nano), EndUTC: time.Now().UTC().Format(time.RFC3339Nano), Ended: o.ended, ReadError: o.readError, Complete: o.ended && !o.readError && o.omitted == 0 && o.summaryOmitted == 0, RX: o.rx, Omitted: o.omitted, PeerOverflow: o.peerOverflow, EventOverflow: o.eventOverflow, AfterEnd: o.afterEnd, SummaryOmitted: o.summaryOmitted, Counters: counts, Events: events, MissingRX: "NOT_OBSERVED; UNKNOWN if incomplete; never proof of no kernel ingress"}
	sink := o.sink
	o.mu.Unlock()
	if sink != nil {
		sink(s)
	}
}
func udpPreacceptBufferOutcome(err error) string {
	switch {
	case err == nil:
		return "BUFFER_OK"
	case errors.Is(err, packetio.ErrFull):
		return "BUFFER_FULL"
	case errors.Is(err, io.ErrClosedPipe):
		return "BUFFER_CLOSED"
	default:
		return "BUFFER_OTHER"
	}
}
