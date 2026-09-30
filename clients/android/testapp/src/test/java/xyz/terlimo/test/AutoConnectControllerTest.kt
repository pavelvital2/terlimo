package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** §26.2 pure controller: account fence, consumed generations, rights/pref gating, stages. */
class AutoConnectControllerTest {

    private fun rt(
        enabled: Boolean = true,
        generation: Long,
        accountKnown: Boolean = true,
        accountRef: String? = "acc-1",
        lastNodeId: String? = "node-1",
        nodePresent: Boolean = true,
        entitlementUsable: Boolean = true,
        dataConnected: Boolean = false,
    ) = AutoConnectRuntime(enabled, generation, accountKnown, accountRef, lastNodeId, nodePresent, entitlementUsable, dataConnected)

    private fun noop(effects: List<AutoConnectEffect>): Boolean = effects.all { it is AutoConnectEffect.None }

    @Test fun firstMeAttachIsNotAnIdentitySwitch() {
        assertFalse(AutoConnectAccountFence.isIdentitySwitch(null, "acc-1"))
        assertFalse(AutoConnectAccountFence.isIdentitySwitch("acc-1", "acc-1"))
        assertTrue(AutoConnectAccountFence.isIdentitySwitch("acc-1", "acc-2"))
        assertFalse(AutoConnectAccountFence.isIdentitySwitch(null, null))
    }

    @Test fun consumedGenerationIsNeverRearmed() {
        val c = AutoConnectController()
        c.onLaunch(rt(generation = 5))
        c.onCatalog(rt(generation = 5), "node-1", false)
        c.onSelectOpportunity(rt(generation = 5), consentGranted = true)
        c.onConnected("node-1")
        assertTrue(noop(c.onLaunch(rt(generation = 5))))
        val next = c.onLaunch(rt(generation = 6))
        assertEquals(1, next.size)
        assertTrue(next.single() is AutoConnectEffect.StartAttempt)
    }

    @Test fun staleGenerationDoesNotStartANewRun() {
        val c = AutoConnectController()
        c.onLaunch(rt(generation = 9))
        assertTrue(noop(c.onLaunch(rt(generation = 8))))
        assertTrue(c.isArmedFor(9))
    }

    @Test fun rightsWithdrawalBlocksCatalogAndSelect() {
        val c = AutoConnectController()
        c.onLaunch(rt(generation = 3))
        val catalog = c.onCatalog(rt(generation = 3, entitlementUsable = false), "node-1", false)
        assertTrue(catalog.any { it is AutoConnectEffect.Message && it.code == AutoConnectCode.RIGHTS })
        assertFalse(c.isArmed())

        val c2 = AutoConnectController()
        c2.onLaunch(rt(generation = 4))
        c2.onCatalog(rt(generation = 4), "node-1", false)
        val select = c2.onSelectOpportunity(rt(generation = 4, entitlementUsable = false), consentGranted = true)
        assertTrue(select.any { it is AutoConnectEffect.Message && it.code == AutoConnectCode.RIGHTS })
        assertFalse(c2.isArmed())
    }

    @Test fun disabledPreferenceIsHonouredByCatalogAndSelect() {
        val c = AutoConnectController()
        c.onLaunch(rt(generation = 7))
        val off = rt(enabled = false, generation = 7)
        assertTrue(noop(c.onCatalog(off, "other", false)))
        assertFalse(c.isArmed())
    }

    @Test fun chooseIsSentOnceUntilSelectionConfirms() {
        val c = AutoConnectController()
        c.onLaunch(rt(generation = 8))
        val first = c.onCatalog(rt(generation = 8), selectedNodeId = "other", pendingChoice = false)
        assertEquals(1, first.count { it is AutoConnectEffect.Choose })
        val second = c.onCatalog(rt(generation = 8), selectedNodeId = "other", pendingChoice = false)
        assertTrue(noop(second))
        val confirmed = c.onCatalog(rt(generation = 8), selectedNodeId = "node-1", pendingChoice = false)
        assertEquals(AutoConnectController.Stage.SELECT_READY, c.stage)
        assertTrue(noop(confirmed))
        val select = c.onSelectOpportunity(rt(generation = 8), consentGranted = true)
        assertEquals(1, select.count { it is AutoConnectEffect.Select })
        val duplicate = c.onSelectOpportunity(rt(generation = 8), consentGranted = true)
        assertTrue(noop(duplicate))
    }

    @Test fun catalogWithTargetAlreadySelectedGoesStraightToSelectReady() {
        val c = AutoConnectController()
        c.onLaunch(rt(generation = 2))
        c.onCatalog(rt(generation = 2), selectedNodeId = "node-1", pendingChoice = false)
        assertEquals(AutoConnectController.Stage.SELECT_READY, c.stage)
        val select = c.onSelectOpportunity(rt(generation = 2), consentGranted = true)
        assertEquals(1, select.count { it is AutoConnectEffect.Select })
    }

    @Test fun effectsCarryPlanningContext() {
        val c = AutoConnectController()
        c.onLaunch(rt(generation = 12, accountRef = "acc-X"))
        val choose = c.onCatalog(rt(generation = 12, accountRef = "acc-X"), "other", false)
            .filterIsInstance<AutoConnectEffect.Choose>().single()
        assertEquals(12L, choose.generation)
        assertEquals("node-1", choose.target)
        assertEquals("acc-X", choose.accountRef)
    }
}
