package xyz.terlimo.test

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

class OwnerHarnessUiSynchronizationSourceTest {
    @Test fun catalogWaitsForExactSelectedSpinnerAndEnabledConnect() {
        val source = listOf(
            File("src/androidTest/java/xyz/terlimo/test/OwnerPhoneTest.kt"),
            File("testapp/src/androidTest/java/xyz/terlimo/test/OwnerPhoneTest.kt")
        ).first { it.isFile }.readText()
        val helper = source.substringAfter("fun clickConnectWhenReady(").substringBefore("val monitor =")
        assertTrue(helper.contains("while (!clicked"))
        assertTrue(helper.contains("state.phase == \"CatalogReady\""))
        assertTrue(helper.contains("selected != null"))
        assertTrue(helper.contains("spinner.setSelection(position)"))
        assertTrue(helper.contains("state.selectedNodeId != requestedNodeId"))
        assertTrue(helper.contains("selected == requestedNodeId"))
        assertTrue(helper.contains("displayed == selected"))
        assertTrue(helper.contains("button.isEnabled"))
        assertTrue(helper.indexOf("button.isEnabled") < helper.indexOf("button.performClick()"))
        assertFalse(source.substringAfter("if (catalog && !initiallyVpn)").substringBefore("val vpnDeadline")
            .contains("click(\"Подключить выбранный сервер\")"))
        assertTrue(source.contains("TEST_REQUESTED_NODE_REQUIRED"))
        assertTrue(source.contains("TEST_REQUESTED_NODE_MISSING"))
    }
}
