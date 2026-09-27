package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ActiveNodeSwitchSourceTest {
    private fun source(path: String): String = listOf(File(path), File("testapp/$path"), File("../$path"))
        .first { it.isFile }.readText()

    @Test fun nativeKeepsCurrentRuntimeUntilAuthenticatedReplacementAck() {
        val runner = source("go_client/terlimo_runner.go")
        assertTrue(runner.contains("case switchRequest := <-c.bridge.switchNode:"))
        assertTrue(runner.contains("c.rollbackAllowed(node, registration)"))
        assertTrue(runner.contains("result.string(\"switch_id\") != operation.ID"))
        assertTrue(runner.contains("result.string(\"catalog_revision\") != operation.Revision"))
        assertTrue(runner.contains("sameManagedAdmission("))
        val replacementBlock = runner.substringAfter("if replace != nil")
        val fenceAt = replacementBlock.indexOf("if !c.mobileAdmissionFence(")
        val chooseRel = replacementBlock.indexOf("c.chooseNode(commitCtx, node.NodeID)")
        val replaceRel = replacementBlock.indexOf("replace()")
        assertTrue(fenceAt > 0)
        assertTrue(chooseRel > fenceAt)
        assertTrue(replaceRel > chooseRel)
        assertFalse(runner.contains("switchErr = c.vpn(parent, parentCancel, target, registration, cancel)"))
        assertTrue(runner.contains("return errManagedRuntimeHandedOff"))
        assertTrue(runner.indexOf("if replace != nil") > runner.indexOf("result, e := c.bridge.request"))
        assertTrue(runner.indexOf("if replace != nil") > runner.indexOf("result, e := c.bridge.request"))
    }

    @Test fun hostRollsBackAndHasOneServiceOwner() {
        val service = source("testapp/src/main/java/xyz/terlimo/test/SessionService.kt")
        val backend = source("testapp/src/main/java/xyz/terlimo/test/SafeGoBackend.java")
        assertTrue(service.contains("private fun rollbackSwitch("))
        assertTrue(service.contains("restoreVpn(wireguard, previousConfig, previousPlan)"))
        assertTrue(service.contains("completeSwitch(attempt, id, switchId, revision)"))
        assertTrue(service.contains("val wireguard = if (switching) SafeGoBackend(this)"))
        assertTrue(service.contains("rollbackAllowed"))
        assertTrue(service.indexOf("oldBackend?.retireState(tunnel)") > service.indexOf("private fun completeSwitch"))
        assertTrue(service.contains("if (!rollbackAllowed) stopAttempt(code)"))
        assertTrue(backend.contains("public synchronized void retireState"))
        assertTrue(service.contains("activeVpnConfig = null"))
        assertFalse(backend.contains("throw new IllegalStateException(\"TUNNEL_ALREADY_ACTIVE\")"))
        assertTrue(service.contains("class SessionService : Service()"))
    }
}
