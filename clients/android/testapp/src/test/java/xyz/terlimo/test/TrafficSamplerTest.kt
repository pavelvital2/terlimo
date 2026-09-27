package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class TrafficSamplerTest {
    @Test
    fun `peer counters map to totals and monotonic rate`() {
        val s = TrafficSampler()
        s.onExplicitConnect()
        s.onPeerSample(1000, 500, 1000, "node-a") // baseline
        val snap = s.onPeerSample(3000, 1500, 3000, "node-a")
        assertEquals(3000L, snap.rxTotal)
        assertEquals(1500L, snap.txTotal)
        assertEquals(1000L, snap.rxRateBps)
        assertEquals(500L, snap.txRateBps)
        assertEquals("node-a", snap.sourceEpoch)
    }

    @Test
    fun `explicit connect resets and switch only rebaselines`() {
        val s = TrafficSampler()
        s.onExplicitConnect()
        s.onPeerSample(0, 0, 1000, "n")
        val before = s.onPeerSample(4000, 4000, 2000, "n")
        assertEquals(4000L, before.rxTotal)
        s.onRecoveryOrSwitch()
        s.onPeerSample(9000, 9000, 3000, "n") // re-baseline, no add
        val after = s.onPeerSample(10000, 9000, 4000, "n")
        assertEquals(5000L, after.rxTotal) // 4000 preserved + 1000 delta
        s.onExplicitConnect()
        val fresh = s.onPeerSample(50, 60, 5000, "n")
        assertEquals(50L, fresh.rxTotal) // new session counts from zero
    }

    @Test
    fun `new node epoch resets counters without negative traffic`() {
        val s = TrafficSampler()
        s.onExplicitConnect()
        s.onPeerSample(8000, 8000, 1000, "node-a")
        val moved = s.onPeerSample(10, 20, 2000, "node-b")
        assertEquals(8000L, moved.rxTotal) // prior total kept, new epoch baselined
        assertEquals("node-b", moved.sourceEpoch)
    }

    @Test
    fun `delayed old sample is ignored by monotonic fence`() {
        val s = TrafficSampler()
        s.onExplicitConnect()
        s.onPeerSample(1000, 1000, 5000, "n")
        s.onPeerSample(2000, 2000, 6000, "n")
        val late = s.onPeerSample(9000, 9000, 5500, "n")
        assertEquals(2000L, late.rxTotal) // 1000 from zero + 1000 delta, late ignored
    }

    @Test
    fun `disconnect then new connect before first sample is honest and reset`() {
        val s = TrafficSampler()
        s.onExplicitConnect()
        s.onPeerSample(5000, 5000, 1000, "n")
        val off = s.onDisconnect()
        assertFalse(off.active)
        assertFalse(off.measured)
        assertEquals(0L, off.rxRateBps)
        val fresh = s.onExplicitConnect()
        assertTrue(fresh.active)
        assertEquals(0L, fresh.rxTotal)
        assertEquals(0L, fresh.txTotal)
        assertFalse(fresh.measured) // no fabricated measurement before the first sample
    }

    @Test
    fun `switch or recovery clears the rate and measured flag but keeps totals`() {
        val s = TrafficSampler()
        s.onExplicitConnect()
        s.onPeerSample(0, 0, 1000, "n")
        s.onPeerSample(4000, 2000, 2000, "n")
        val rebased = s.onRecoveryOrSwitch()
        assertEquals(4000L, rebased.rxTotal)
        assertEquals(2000L, rebased.txTotal)
        assertEquals(0L, rebased.rxRateBps)
        assertEquals(0L, rebased.txRateBps)
        assertFalse(rebased.measured)
    }

    @Test
    fun `disconnect stops applying further samples`() {
        val s = TrafficSampler()
        s.onExplicitConnect()
        s.onPeerSample(1000, 1000, 1000, "n")
        s.onDisconnect()
        assertFalse(s.snapshot().active)
        val ignored = s.onPeerSample(5000, 5000, 2000, "n")
        assertEquals(1000L, ignored.rxTotal) // final accounted sum preserved
    }
}
