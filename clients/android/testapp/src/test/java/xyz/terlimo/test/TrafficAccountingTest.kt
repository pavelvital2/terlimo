package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class TrafficAccountingTest {
    private fun sample(rx: Long, tx: Long, ms: Long, epoch: String = "grant-1", seq: Long = 1L) =
        TrafficSample(rx, tx, ms, epoch, seq)

    @Test
    fun `explicit connect counts the first sample from zero`() {
        val t = TrafficAccounting()
        t.onExplicitConnect()
        val first = t.onSample(sample(1000, 500, 1000)) // new peer, counters start at zero
        assertEquals(1000L, first.rxTotal)
        assertEquals(500L, first.txTotal)
        assertEquals(0L, first.rxRateBps) // no earlier monotonic point yet
        val snap = t.onSample(sample(3000, 1500, 3000, seq = 2))
        assertEquals(3000L, snap.rxTotal)
        assertEquals(1500L, snap.txTotal)
        assertEquals(1000L, snap.rxRateBps) // 2000 bytes / 2000 ms
        assertEquals(500L, snap.txRateBps)
    }

    @Test
    fun `recovery or switch keeps totals and rebaselines without double count`() {
        val t = TrafficAccounting()
        t.onExplicitConnect()
        t.onSample(sample(1000, 1000, 1000))
        val before = t.onSample(sample(3000, 3000, 3000, seq = 2))
        assertEquals(3000L, before.rxTotal)
        t.onRecoveryOrSwitch()
        val rebase = t.onSample(sample(9000, 7000, 8000, seq = 3)) // new raw baseline
        assertEquals(3000L, rebase.rxTotal) // accumulated preserved, delta dropped
        val after = t.onSample(sample(9500, 7200, 8500, seq = 4))
        assertEquals(3500L, after.rxTotal)
        assertEquals(3200L, after.txTotal) // 3000 accumulated + 200 delta
    }

    @Test
    fun `negative delta starts a new source epoch without negative traffic`() {
        val t = TrafficAccounting()
        t.onExplicitConnect()
        t.onSample(sample(5000, 5000, 1000))
        val reset = t.onSample(sample(10, 20, 2000, epoch = "grant-2", seq = 1))
        assertEquals(5000L, reset.rxTotal) // no negative; prior total kept, new epoch baselined
        assertEquals("grant-2", reset.sourceEpoch)
        val grown = t.onSample(sample(1010, 1020, 3000, epoch = "grant-2", seq = 2))
        assertEquals(6000L, grown.rxTotal)
        assertEquals(6000L, grown.txTotal)
    }

    @Test
    fun `stale sequence and foreign epoch never double count`() {
        val t = TrafficAccounting()
        t.onExplicitConnect()
        t.onSample(sample(1000, 1000, 1000, seq = 5))
        val grown = t.onSample(sample(2000, 2000, 2000, seq = 6))
        assertEquals(2000L, grown.rxTotal)
        // Stale sequence for the same epoch is ignored.
        val stale = t.onSample(sample(9000, 9000, 2500, seq = 6))
        assertEquals(2000L, stale.rxTotal)
        // Foreign epoch re-baselines instead of adding a bogus delta.
        val foreign = t.onSample(sample(50, 50, 3000, epoch = "grant-9", seq = 1))
        assertEquals(2000L, foreign.rxTotal)
        assertEquals("grant-9", foreign.sourceEpoch)
    }

    @Test
    fun `disconnect ends the session and samples are ignored`() {
        val t = TrafficAccounting()
        t.onExplicitConnect()
        t.onSample(sample(1000, 1000, 1000))
        t.onSample(sample(2000, 2000, 2000, seq = 2))
        val off = t.onDisconnect()
        assertFalse(off.active)
        assertEquals(0L, off.rxRateBps)
        val ignored = t.onSample(sample(9000, 9000, 3000, seq = 3))
        assertEquals(off.rxTotal, ignored.rxTotal)
    }

    @Test
    fun `delayed old same-epoch sample after recovery never becomes the baseline`() {
        val t = TrafficAccounting()
        t.onExplicitConnect()
        t.onSample(sample(1000, 1000, 1000, seq = 1))
        t.onSample(sample(3000, 3000, 3000, seq = 2)) // +2000
        t.onRecoveryOrSwitch()
        t.onSample(sample(5000, 5000, 6000, seq = 6)) // rebaseline, totals stay 3000
        val delayed = t.onSample(sample(4000, 4000, 5500, seq = 5)) // stale seq + older monotonic
        assertEquals(3000L, delayed.rxTotal)
        val fresh = t.onSample(sample(6000, 6000, 7000, seq = 7))
        assertEquals(4000L, fresh.rxTotal) // exactly +1000, no double count
        assertEquals(4000L, fresh.txTotal)
    }

    @Test
    fun `late previous-epoch sample cannot replace the new epoch baseline`() {
        val t = TrafficAccounting()
        t.onExplicitConnect()
        t.onSample(sample(1000, 1000, 1000, epoch = "A", seq = 1))
        t.onSample(sample(2000, 2000, 2000, epoch = "A", seq = 2)) // +1000
        t.onSample(sample(10, 10, 3000, epoch = "B", seq = 1)) // new epoch baseline
        val lateOld = t.onSample(sample(9000, 9000, 4000, epoch = "A", seq = 3))
        assertEquals(2000L, lateOld.rxTotal) // retired epoch ignored
        val grown = t.onSample(sample(20, 20, 5000, epoch = "B", seq = 2))
        assertEquals(2010L, grown.rxTotal)
    }

    @Test
    fun `non-increasing monotonic timestamp is ignored`() {
        val t = TrafficAccounting()
        t.onExplicitConnect()
        t.onSample(sample(1000, 1000, 1000, seq = 1))
        val stuck = t.onSample(sample(5000, 5000, 1000, seq = 2))
        assertEquals(1000L, stuck.rxTotal)
        assertEquals(0L, stuck.rxRateBps)
    }

    @Test
    fun `explicit connect clears epoch and sequence fences`() {
        val t = TrafficAccounting()
        t.onExplicitConnect()
        t.onSample(sample(1000, 1000, 1000, epoch = "A", seq = 1))
        t.onSample(sample(2, 2, 2000, epoch = "B", seq = 1)) // retires A
        t.onExplicitConnect()
        val fresh = t.onSample(sample(50, 60, 3000, epoch = "A", seq = 1))
        assertEquals("A", fresh.sourceEpoch)
        assertEquals(50L, fresh.rxTotal) // fresh session counts from zero, no carry-over
    }

    @Test
    fun `measured flag is true only after a real sample`() {
        val t = TrafficAccounting()
        assertFalse(t.snapshot().measured)
        t.onExplicitConnect()
        assertFalse(t.snapshot().measured)
        t.onSample(sample(100, 100, 1000, seq = 1))
        assertTrue(t.snapshot().measured)
        assertEquals(100L, t.snapshot().rxTotal) // counted from zero on the new peer
        t.onRecoveryOrSwitch()
        assertFalse(t.snapshot().measured)
        assertEquals(100L, t.snapshot().rxTotal) // totals preserved
    }

    @Test
    fun `snapshot exposes the current source epoch and totals`() {
        val t = TrafficAccounting()
        assertFalse(t.snapshot().active)
        t.onExplicitConnect()
        t.onSample(sample(100, 200, 10, epoch = "grant-7", seq = 1))
        t.onSample(sample(400, 700, 210, epoch = "grant-7", seq = 2))
        val snap = t.snapshot()
        assertTrue(snap.active)
        assertEquals("grant-7", snap.sourceEpoch)
        assertEquals(400L, snap.rxTotal)
        assertEquals(700L, snap.txTotal)
        assertEquals(1500L, snap.rxRateBps) // 300 bytes / 200 ms
        assertEquals(2500L, snap.txRateBps)
    }
}
