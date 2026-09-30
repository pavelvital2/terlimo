package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Test

class HelpContentTest {
    @Test fun versionIsRuntimeFormattedNotHardcoded() {
        assertEquals("Версия приложения: 0.14-routing (14)", HelpContent.formatVersion("0.14-routing", 14L))
        assertEquals("Версия приложения: 1.2-d1 (14)", HelpContent.formatVersion("1.2-d1", 14L))
        assertEquals("Версия приложения: —", HelpContent.formatVersion(null, 0L))
    }
}
