package main

import (
	"context"
	"encoding/binary"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	wireGuardTransportDataType = 4
	wireGuardEmptyDataSize     = 32
)

func isWireGuardUserDataPacket(packet []byte) bool {
	return len(packet) > wireGuardEmptyDataSize &&
		binary.LittleEndian.Uint32(packet[:4]) == wireGuardTransportDataType
}

var pktPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 2048)
	},
}

func getPktBuf(size int) []byte {
	b := pktPool.Get().([]byte)
	if cap(b) < size {
		b = make([]byte, size)
	}
	return b[:size]
}

func putPktBuf(b []byte) {
	if cap(b) < 2048 {
		return
	}
	pktPool.Put(b[:cap(b)])
}

const (
	returnChBuf           = 384
	deviceWakeHealthGrace = 60 * time.Second

	// chunkSize — количество последовательных пакетов, отправляемых в один worker
	// перед переключением на следующий.
	//
	// Зачем: при round-robin (chunk=1) каждый пакет летит через разный TURN relay
	// с разным latency, что приводит к reorder на сервере. TCP внутри WireGuard
	// интерпретирует reorder как потери → cwnd collapse → скорость single-flow
	// падает до ~8 KB/s.
	//
	// С chunk=16 пакеты реже перескакивают между путями с разной задержкой. Это
	// уменьшает reorder на активной многоканальной сессии, сохраняя равномерную
	// загрузку всех workers.
	// Reorder возможен только между chunk-границами, что покрывается WG replay
	// window (2048 пакетов).
	//
	// Все workers по-прежнему получают одинаковую долю трафика за полный цикл;
	// фактический выигрыш зависит от различия задержек между TURN-путями.
	chunkSize = 16
)

type WorkerSlot struct {
	ID                     int
	SendCh                 chan []byte
	WakeCh                 chan uint64
	SleepCh                chan struct{}
	WakeAckCh              chan uint64
	WakeVerifiedGeneration atomic.Uint64
}

type Dispatcher struct {
	localConn   net.PacketConn
	clientAddr  atomic.Pointer[net.Addr]
	workers     atomic.Pointer[[]*WorkerSlot]
	mu          sync.Mutex // Используется только для записи
	rrIndex     int
	rrCount     int
	ReturnCh    chan []byte
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	stats       *Stats
	diagnostics *managedDiagnostics
	// firstUnansweredUserTxAt is global for the dispatcher because a reply may
	// return through a different worker than the one that sent the request.
	// Keeping the first (rather than latest) unanswered send also prevents a
	// continuous stream of retries from postponing stall detection forever.
	firstUnansweredUserTxAt atomic.Int64
	stalledUserTraffic      atomic.Bool
	deviceSleeping          atomic.Bool
	wakeGeneration          atomic.Uint64
	wakeHealthGraceUntil    atomic.Int64
	// connectedEcho is the live config session's echo probe path, replaced on
	// every reconnect and cleared on session exit. The Connected event loop
	// reaches the live DTLS session only through this registration.
	connectedEcho atomic.Pointer[connectedEcho]
}

func NewDispatcher(ctx context.Context, localConn net.PacketConn, stats *Stats) *Dispatcher {
	dctx, dcancel := context.WithCancel(ctx)
	d := &Dispatcher{
		localConn: localConn,
		ReturnCh:  make(chan []byte, returnChBuf),
		ctx:       dctx,
		cancel:    dcancel,
		stats:     stats,
	}
	if managed := managedTransport(ctx); managed != nil {
		d.diagnostics = managed.Diagnostics
	}

	empty := make([]*WorkerSlot, 0)
	d.workers.Store(&empty)

	d.wg.Add(2)
	go d.readLoop()
	go d.writeLoop()
	return d
}

func (d *Dispatcher) Shutdown() {
	d.cancel()
	d.wg.Wait()
}

func (d *Dispatcher) Register(w *WorkerSlot) {
	d.mu.Lock()
	oldWorkers := d.workers.Load()
	newWorkers := make([]*WorkerSlot, len(*oldWorkers)+1)
	copy(newWorkers, *oldWorkers)
	newWorkers[len(*oldWorkers)] = w
	d.workers.Store(&newWorkers)
	d.mu.Unlock()
	log.Printf("[ДИСП] Воркер #%d зарегистрирован (всего: %d)", w.ID, len(newWorkers))
	generation := d.wakeGeneration.Load()
	if generation > 0 && !d.deviceSleeping.Load() && w.WakeCh != nil {
		offerLatestGeneration(w.WakeCh, generation)
	}
}

func (d *Dispatcher) Unregister(slot *WorkerSlot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	oldWorkers := d.workers.Load()
	newWorkers := make([]*WorkerSlot, 0, len(*oldWorkers))
	for _, w := range *oldWorkers {
		if w != slot {
			newWorkers = append(newWorkers, w)
		}
	}
	d.workers.Store(&newWorkers)
	log.Printf("[ДИСП] Воркер #%d отключён (осталось: %d)", slot.ID, len(newWorkers))
}

// registerConnectedEcho publishes a connected session as the manual echo
// target. A registration of an older generation can never displace a newer
// one: the session generation fence rejects any result produced by a session
// that was replaced while the measurement was in flight.
func (d *Dispatcher) registerConnectedEcho(e *connectedEcho) {
	if e == nil {
		return
	}
	for {
		current := d.connectedEcho.Load()
		if current != nil && current.generation > e.generation {
			return
		}
		if d.connectedEcho.CompareAndSwap(current, e) {
			return
		}
	}
}

// unregisterConnectedEcho clears exactly that session; a stale unregister can
// never remove a newer registration.
func (d *Dispatcher) unregisterConnectedEcho(e *connectedEcho) {
	if e == nil {
		return
	}
	d.connectedEcho.CompareAndSwap(e, nil)
}

// RequestConnectedRTT measures the echo RTT on the registered live session
// without opening a new connection. It fails closed when no config session is
// registered or when the session was replaced while the echo was in flight.
func (d *Dispatcher) RequestConnectedRTT(ctx context.Context) (time.Duration, error) {
	session := d.connectedEcho.Load()
	if session == nil {
		return 0, errEchoProbeUnavailable
	}
	rtt, err := session.RequestRTT(ctx)
	if err != nil {
		return 0, err
	}
	if d.connectedEcho.Load() != session {
		return 0, errEchoSessionStale
	}
	return rtt, nil
}

func (d *Dispatcher) noteUserTrafficSent(now time.Time) {
	d.firstUnansweredUserTxAt.CompareAndSwap(0, now.UnixNano())
}

func (d *Dispatcher) noteUserTrafficResponse() bool {
	d.firstUnansweredUserTxAt.Store(0)
	return d.stalledUserTraffic.Swap(false)
}

func (d *Dispatcher) resetUserTrafficHealth() {
	d.firstUnansweredUserTxAt.Store(0)
	d.stalledUserTraffic.Store(false)
}

func (d *Dispatcher) noteDeviceSleep() {
	d.deviceSleeping.Store(true)
	d.wakeHealthGraceUntil.Store(0)
	d.resetUserTrafficHealth()
	workers := d.workers.Load()
	if workers != nil {
		for _, worker := range *workers {
			if worker != nil && worker.SleepCh != nil {
				select {
				case worker.SleepCh <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (d *Dispatcher) noteDeviceWake(now time.Time) uint64 {
	d.resetUserTrafficHealth()
	d.wakeHealthGraceUntil.Store(now.Add(deviceWakeHealthGrace).UnixNano())
	generation := d.wakeGeneration.Add(1)
	d.deviceSleeping.Store(false)

	workers := d.workers.Load()
	if workers == nil {
		return generation
	}
	for _, worker := range *workers {
		if worker.WakeCh == nil {
			continue
		}
		// A queued sleep belongs to the generation that wake supersedes. Drain it
		// before publishing the new generation so it cannot cancel the new probe.
		if worker.SleepCh != nil {
			select {
			case <-worker.SleepCh:
			default:
			}
		}
		offerLatestGeneration(worker.WakeCh, generation)
	}
	return generation
}

func workerEligibleForWakeGeneration(worker *WorkerSlot, generation uint64, deviceSleeping bool) bool {
	return worker != nil && (generation == 0 || deviceSleeping || worker.WakeVerifiedGeneration.Load() >= generation)
}

func (d *Dispatcher) ActiveWorkers() int {
	workers := d.workers.Load()
	if workers == nil {
		return 0
	}
	active := 0
	for _, worker := range *workers {
		if worker != nil {
			active++
		}
	}
	return active
}

func (d *Dispatcher) wakeStatus(generation uint64) (ready int, total int) {
	workers := d.workers.Load()
	if workers == nil {
		return 0, 0
	}
	for _, worker := range *workers {
		if worker == nil {
			continue
		}
		total++
		if worker.WakeVerifiedGeneration.Load() >= generation {
			ready++
		}
	}
	return ready, total
}

func (d *Dispatcher) acknowledgeWorkerWake(worker *WorkerSlot, generation uint64) {
	if worker == nil || generation == 0 || generation != d.wakeGeneration.Load() || d.deviceSleeping.Load() {
		return
	}
	worker.WakeVerifiedGeneration.Store(generation)
	offerLatestGeneration(worker.WakeAckCh, generation)
}

func offerLatestGeneration(ch chan uint64, generation uint64) {
	if ch == nil {
		return
	}
	select {
	case ch <- generation:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- generation:
	default:
	}
}

func (d *Dispatcher) shouldSuppressTransportHealth(now time.Time) bool {
	if d.deviceSleeping.Load() {
		return true
	}
	graceUntil := d.wakeHealthGraceUntil.Load()
	return graceUntil > 0 && now.UnixNano() < graceUntil
}

func (d *Dispatcher) claimStalledUserTraffic(now time.Time, timeout time.Duration) (time.Duration, bool) {
	startedAt := d.firstUnansweredUserTxAt.Load()
	if startedAt == 0 {
		return 0, false
	}
	stalledFor := now.Sub(time.Unix(0, startedAt))
	if stalledFor <= timeout || !d.firstUnansweredUserTxAt.CompareAndSwap(startedAt, 0) {
		return 0, false
	}
	d.stalledUserTraffic.Store(true)
	return stalledFor, true
}

// readLoop читает WireGuard-пакеты и распределяет по workers chunk'ами.
//
// Логика: отправляем chunkSize подряд пакетов в один worker, потом переходим
// к следующему. Если текущий worker перегружен (канал полный) — немедленно
// ищем свободный worker и начинаем новый chunk на нём. Это гарантирует:
//   - В рамках chunk пакеты идут через один TURN relay → in-order delivery
//   - Между chunks — разные relay → максимальная агрегатная скорость
//   - Нет блокировки, нет буферизации, нет дополнительного latency
func (d *Dispatcher) readLoop() {
	defer d.wg.Done()

	for {
		if err := d.ctx.Err(); err != nil {
			return
		}

		pkt := getPktBuf(2048)

		n, addr, err := d.localConn.ReadFrom(pkt)
		if err != nil {
			putPktBuf(pkt)
			if d.ctx.Err() != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		pkt = pkt[:n]
		d.diagnostics.noteLocalRX(n)

		d.clientAddr.Store(&addr)
		d.stats.TotalBytesUp.Add(int64(n))

		workersPtr := d.workers.Load()
		if workersPtr == nil || len(*workersPtr) == 0 {
			putPktBuf(pkt)
			continue
		}

		ws := *workersPtr
		nw := len(ws)
		wakeGeneration := d.wakeGeneration.Load()
		deviceSleeping := d.deviceSleeping.Load()

		sent := false
		idx := d.rrIndex % nw

		for offset := 0; offset < nw; offset++ {
			candidateIdx := (idx + offset) % nw
			candidate := ws[candidateIdx]
			if !workerEligibleForWakeGeneration(candidate, wakeGeneration, deviceSleeping) {
				continue
			}
			select {
			case candidate.SendCh <- pkt:
				sent = true
				if offset == 0 {
					d.rrCount++
				} else {
					d.rrIndex, d.rrCount = candidateIdx, 1
				}
				if d.rrCount >= chunkSize {
					d.rrIndex, d.rrCount = (candidateIdx+1)%nw, 0
				}
			default:
			}
			if sent {
				break
			}
		}

		if !sent {
			// Все workers перегружены — сдвигаем указатель, пакет дропается
			d.rrIndex = (idx + 1) % nw
			d.rrCount = 0
			putPktBuf(pkt)
		}
	}
}

func (d *Dispatcher) writeLoop() {
	defer d.wg.Done()

	for {
		select {
		case <-d.ctx.Done():
			return
		case pkt := <-d.ReturnCh:
			addrPtr := d.clientAddr.Load()
			if addrPtr == nil {
				putPktBuf(pkt)
				continue
			}
			addr := *addrPtr
			if err := d.writeLocalPacket(pkt, addr); err != nil {
				if d.ctx.Err() != nil {
					putPktBuf(pkt)
					return
				}
			}
			putPktBuf(pkt)
		}
	}
}

func (d *Dispatcher) writeLocalPacket(pkt []byte, addr net.Addr) error {
	n, err := d.localConn.WriteTo(pkt, addr)
	d.diagnostics.noteLocalWrite(len(pkt), n, err)
	// Preserve legacy accounting; managed evidence counts only whole successful
	// UDP writes, never failed/partial writes as delivered downlink traffic.
	legacyCount := d.diagnostics == nil && !(err != nil && d.ctx.Err() != nil)
	if legacyCount || (err == nil && n == len(pkt)) {
		d.stats.TotalBytesDown.Add(int64(len(pkt)))
	}
	return err
}
