package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class MobileBootstrapSeedTest {
    // Exact shape of the packaged testapp/src/main/assets/test-mobile.json.
    private val packagedSeed =
        """{"base_url":"https://terlimo.193-5-251-217.sslip.io","environment":"test"}"""

    @Test
    fun packagedTestSeedIsValid() {
        val seed = MobileBootstrapSeed.parse(packagedSeed)
        assertNotNull(seed)
        assertEquals("https://terlimo.193-5-251-217.sslip.io", seed!!.baseUrl)
        assertEquals("test", seed.environment)
    }

    @Test
    fun environmentMustBeAContractValue() {
        assertEquals("production",
            MobileBootstrapSeed.parse("""{"base_url":"https://host","environment":"production"}""")?.environment)
        // Native resolves an absent/empty value to the same "test" default the host sends.
        assertEquals("test", MobileBootstrapSeed.parse("""{"base_url":"https://host"}""")?.environment)
        assertEquals("test", MobileBootstrapSeed.parse("""{"base_url":"https://host","environment":""}""")?.environment)
        assertNull(MobileBootstrapSeed.parse("""{"base_url":"https://host","environment":"staging"}"""))
    }

    @Test
    fun baseUrlMustBeHttpsWithNonEmptyHostAndNoUserinfo() {
        assertNull(MobileBootstrapSeed.parse("""{"base_url":"http://host","environment":"test"}"""))
        assertNull(MobileBootstrapSeed.parse("""{"base_url":"https://user:pass@host","environment":"test"}"""))
        assertNull(MobileBootstrapSeed.parse("""{"base_url":"https://user@host","environment":"test"}"""))
        assertNull(MobileBootstrapSeed.parse("""{"base_url":"https://","environment":"test"}"""))
        assertNull(MobileBootstrapSeed.parse("""{"base_url":"https:///path","environment":"test"}"""))
        assertNull(MobileBootstrapSeed.parse("""{"base_url":"not a url","environment":"test"}"""))
        assertNull(MobileBootstrapSeed.parse("""{"base_url":"","environment":"test"}"""))
        assertNotNull(MobileBootstrapSeed.parse("""{"base_url":"https://host:8443/path","environment":"test"}"""))
    }

    @Test
    fun absentUnreadableOrMalformedAssetNeverYieldsASeed() {
        assertNull(MobileBootstrapSeed.parse(null))
        assertNull(MobileBootstrapSeed.parse("not json"))
        assertNull(MobileBootstrapSeed.parse("[]"))
    }

    @Test
    fun gateAdmitsRealLinksAndOnlySeedAuthorizedEmptyLinks() {
        val seed = MobileBootstrapSeed.parse(packagedSeed)
        // Legacy/import path: a real link always passes, with or without a seed.
        assertTrue(MobileBootstrapGate.admits("saved-link", null))
        assertTrue(MobileBootstrapGate.admits("saved-link", seed))
        // Clean install resume/connect: only the valid packaged seed authorizes the empty link.
        assertTrue(MobileBootstrapGate.admits("", seed))
        assertFalse(MobileBootstrapGate.admits("", null))
        assertFalse(MobileBootstrapGate.admits("", MobileBootstrapSeed.parse("""{"base_url":"http://host"}""")))
        assertFalse(MobileBootstrapGate.admits("", MobileBootstrapSeed.parse("""{"base_url":"https://user@host"}""")))
        assertFalse(MobileBootstrapGate.admits("",
            MobileBootstrapSeed.parse("""{"base_url":"https://host","environment":"staging"}""")))
    }

    @Test
    fun realSubscriptionUsesLegacyPathEvenWhenMobileSeedIsPackaged() {
        val seed = MobileBootstrapSeed.parse(packagedSeed)
        assertNull(MobileBootstrapGate.forLink("saved-link", seed))
        assertNull(MobileBootstrapGate.forLink("imported-link", seed))
        assertNotNull(MobileBootstrapGate.forLink("", seed))
        assertNull(MobileBootstrapGate.forLink("", null))
    }
}
