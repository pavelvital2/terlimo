package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class FirstReleaseLinksTest {
    private val testBot = "https://t.me/terlimo_test_bot?start=recovery"
    private val testSite = "https://step036.193-5-251-217.sslip.io/api/public/recovery"
    private val prodBot = "https://t.me/terlimo_vpn_wdtt_bot?start=recovery"
    private val prodSite = "https://terlimo.xyz/api/public/recovery"

    @Test fun `default calls use the configured deployment pair`() {
        val expected = when (BuildConfig.RECOVERY_ENVIRONMENT) {
            "test" -> testBot to testSite
            "prod" -> prodBot to prodSite
            else -> throw AssertionError("Unknown recovery deployment")
        }
        assertEquals(expected.first, FirstReleaseLinks.recoveryBotUrl())
        assertEquals(expected.second, FirstReleaseLinks.recoverySiteUrl())
    }

    @Test fun `test deployment uses its own approved recovery entries`() {
        assertEquals(testBot, FirstReleaseLinks.recoveryBotUrl(testBot, "test"))
        assertEquals(testSite, FirstReleaseLinks.recoverySiteUrl(testSite, "test"))
        assertNull(FirstReleaseLinks.recoveryBotUrl(prodBot, "test"))
        assertNull(FirstReleaseLinks.recoverySiteUrl(prodSite, "test"))
    }

    @Test fun `production deployment uses its own approved recovery entries`() {
        assertEquals(prodBot, FirstReleaseLinks.recoveryBotUrl(prodBot, "prod"))
        assertEquals(prodSite, FirstReleaseLinks.recoverySiteUrl(prodSite, "prod"))
        assertNull(FirstReleaseLinks.recoveryBotUrl(testBot, "prod"))
        assertNull(FirstReleaseLinks.recoverySiteUrl(testSite, "prod"))
    }

    @Test fun `ordinary links are unchanged and are not recovery entries`() {
        assertEquals("https://t.me/terlimo_vpn_wdtt_bot", FirstReleaseLinks.BOT_URL)
        assertEquals("https://terlimo.xyz/", FirstReleaseLinks.SITE_URL)
        assertNull(FirstReleaseLinks.recoveryBotUrl(FirstReleaseLinks.BOT_URL, "prod"))
        assertNull(FirstReleaseLinks.recoverySiteUrl(FirstReleaseLinks.SITE_URL, "prod"))
    }

    @Test fun `missing unknown or modified routes never become recovery links`() {
        assertNull(FirstReleaseLinks.recoveryBotUrl(null, "test"))
        assertNull(FirstReleaseLinks.recoverySiteUrl("", "test"))
        assertNull(FirstReleaseLinks.recoveryBotUrl(testBot, "unknown"))
        assertNull(FirstReleaseLinks.recoverySiteUrl(testSite, "unknown"))
        for (value in listOf(
            "$testBot&other=1", "$testBot#fragment", testBot.replace("https://", "http://"),
            testBot.replace("t.me/", "t.me.evil/"), testBot.replace("t.me/", "user@t.me/"),
        )) assertNull(value, FirstReleaseLinks.recoveryBotUrl(value, "test"))
        for (value in listOf(
            "$testSite?other=1", "$testSite#fragment", "$testSite/",
            testSite.replace("https://", "http://"), testSite.replace("/api/public/recovery", "/"),
        )) assertNull(value, FirstReleaseLinks.recoverySiteUrl(value, "test"))
    }
}
