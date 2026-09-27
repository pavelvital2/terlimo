package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class MobileBootstrapGateSourceTest {
    private fun source(path: String): String =
        listOf(File(path), File("testapp/$path"), File("../$path")).first { it.isFile }.readText()

    private fun service() = source("src/main/java/xyz/terlimo/test/SessionService.kt")

    @Test
    fun beginAdmitsAnEmptyLinkOnlyThroughTheParsedSeedGate() {
        val service = service()
        assertTrue(service.contains("MobileBootstrapGate.forLink(activeLink, MobileBootstrapSeed.parse(mobileSeed))"))
        assertTrue(service.contains(
            "check(MobileBootstrapGate.admits(activeLink, mobileBootstrap)) { \"IMPORT_REQUIRED\" }"))
        assertTrue(service.contains("val mobileBaseUrl = mobileBootstrap?.baseUrl"))
        assertFalse("seed fields must not be read bypassing MobileBootstrapSeed.parse",
            service.contains("optString(\"base_url\")"))
        assertFalse("no pseudo-link may be synthesized", service.contains("\"mobile:\""))
        assertFalse("no pseudo-link may be synthesized", service.contains("activeLink = \"mobile"))
    }

    @Test
    fun nativeStartGetsTheValidatedSeedFieldsOnly() {
        val service = service()
        assertTrue(service.contains("start.put(\"mobile_base_url\", mobileBootstrap.baseUrl)"))
        assertTrue(service.contains("start.put(\"mobile_environment\", mobileBootstrap.environment)"))
        assertFalse(service.contains("optString(\"environment\""))
    }

    @Test
    fun linklessMobileAttemptNeverPersistsAnEmptyOrPseudoLink() {
        val store = source("src/main/java/xyz/terlimo/test/InstallationStore.kt")
        val writeActiveLink = store.substringAfter("fun writeActiveLink(link: String)")
            .substringBefore("fun readAccountAccessState")
        assertTrue(writeActiveLink.contains("if (link.isEmpty()) return@synchronized"))
        assertTrue(writeActiveLink.contains("writeLocked(SubscriptionStateReplacement.withLink(current, link))"))
        // The native mobile import acknowledgement still routes through writeActiveLink(activeLink),
        // which is exactly the no-op path for a linkless seed-authorized attempt.
        assertTrue(service().contains("storage.writeActiveLink(activeLink)"))
    }

    @Test
    fun legacyGuardsAreUntouchedByTheMobileGate() {
        val service = service()
        assertTrue(service.contains("check(issuers.length() > 0) { \"TRUST_NOT_PROVISIONED\" }"))
        assertTrue(service.contains("check(retiringActors.get() == 0) { \"CLEANUP_PENDING\" }"))
        assertTrue(service.contains("check(gate.active == null) { \"ATTEMPT_ACTIVE\" }"))
        assertTrue(service.contains("PHYSICAL_NETWORK_UNAVAILABLE"))
    }
}
