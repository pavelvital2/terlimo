package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class TrafficReadGateTest {
    @Test
    fun `old same-node callback is dropped after a recovery transition`() {
        val gate = TrafficReadGate()
        val sampler = TrafficSampler()
        sampler.onExplicitConnect()
        gate.bump() // explicit connect
        sampler.onPeerSample(1000, 1000, 1000, "node-a") // baseline+count from zero

        // Schedule read A and capture its generation.
        val readA = gate.current()
        // Same-node recovery: baseline transition bumps the token.
        sampler.onRecoveryOrSwitch()
        gate.bump()
        assertFalse(gate.accepts(readA)) // late old-tunnel read is discarded

        // Fresh read after recovery is accepted once.
        val readB = gate.current()
        assertTrue(gate.accepts(readB))
        val applied = sampler.onPeerSample(5000, 5000, 2000, "node-a")
        assertEquals(1000L, applied.rxTotal) // recovery re-baselines; no old bytes credited
        val grown = sampler.onPeerSample(6000, 5000, 3000, "node-a")
        assertEquals(2000L, grown.rxTotal)
    }

    @Test
    fun `disconnect and reconnect bumps the token`() {
        val gate = TrafficReadGate()
        val readA = gate.bump()
        assertTrue(gate.accepts(readA))
        val readB = gate.bump() // disconnect
        assertFalse(gate.accepts(readA))
        assertTrue(gate.accepts(readB))
        val readC = gate.bump() // explicit reconnect
        assertFalse(gate.accepts(readB))
        assertTrue(gate.accepts(readC))
    }
}
