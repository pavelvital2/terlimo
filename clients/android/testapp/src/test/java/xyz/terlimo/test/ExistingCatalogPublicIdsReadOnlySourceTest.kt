package xyz.terlimo.test

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

class ExistingCatalogPublicIdsReadOnlySourceTest {
    @Test fun extractorHasNoMutationServiceNetworkOrSecretProjection() {
        val source = listOf(
            File("src/androidTest/java/xyz/terlimo/test/ExistingCatalogPublicIdsReadOnlyTest.kt"),
            File("testapp/src/androidTest/java/xyz/terlimo/test/ExistingCatalogPublicIdsReadOnlyTest.kt")
        ).first { it.isFile }.readText()
        assertTrue(source.contains("InstallationStore(inst.targetContext).read()"))
        assertTrue(source.contains("saved_selected_node_id"))
        assertTrue(source.contains("saved_catalog_nodes"))
        assertTrue(source.contains("saved_catalog_revision"))
        assertTrue(source.contains("pending_present"))
        for (forbidden in listOf(".write(", "ensureIdentity(", "startService", "startActivity", "ConnectivityManager", "VpnService", "publicSpki", "sign(", "credential", "grant_id", "password", "pin", "config")) {
            assertFalse("forbidden extractor operation/field: $forbidden", source.contains(forbidden))
        }
    }
}
