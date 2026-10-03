package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class OrbitHomeSourceTest {
    @Test fun preAdmissionPowerUsesTheExistingConsentFunnelBeforeCancel() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        val orbit = source("src/main/java/xyz/terlimo/test/OrbitHomeHeader.kt")
        val powerHandler = activity.substringAfter("onPower = {").substringBefore("onRefresh = {")
        assertTrue(powerHandler.contains("PreAdmissionConnect.connectable(state, pendingChoiceId != null)"))
        assertTrue(powerHandler.contains("requestPreAdmissionConsent(state)"))
        assertTrue(powerHandler.indexOf("requestPreAdmissionConsent(state)") < powerHandler.indexOf("setAction(\"cancel\")"))
        assertTrue(activity.contains("orbitHeader.render(state, pending != null)"))
        assertTrue(orbit.contains("PreAdmissionConnect.connectable(state, pendingChoice)"))
        assertTrue(orbit.contains("power.isEnabled = preAdmissionConnect ||"))
    }

    @Test fun actualCatalogStageIsVisibleOnOrbitAndCatalogLoadingSurface() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        val orbit = source("src/main/java/xyz/terlimo/test/OrbitHomeHeader.kt")
        val catalog = source("src/main/java/xyz/terlimo/test/ServerCatalogView.kt")
        assertTrue(activity.contains("orbitHeader.render(state, pending != null)"))
        assertTrue(activity.contains("catalogView.render(state, state.pings)"))
        val subtitle = orbit.substringAfter("subtitle.text = when {").substringBefore("power.contentDescription")
        assertTrue(subtitle.contains("state.catalogStage != null -> state.catalogStage.label"))
        assertTrue(subtitle.indexOf("state.catalogStage") < subtitle.indexOf("connected ->"))
        assertTrue(catalog.contains("addLoading(state.catalogStage)"))
        val loading = catalog.substringAfter("private fun addLoading(stage: CatalogStage?)").substringBefore("private fun addIdle()")
        assertTrue(loading.contains("addView(text(stage?.label ?:"))
        assertFalse(loading.contains("postDelayed"))
    }

    private fun source(path: String): String = listOf(File(path), File("testapp/$path"))
        .first { it.isFile }.readText()

    @Test fun approvedOrbitIsTheMainProductSurface() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        val orbit = source("src/main/java/xyz/terlimo/test/OrbitHomeHeader.kt")
        assertTrue(activity.contains("OrbitHomeHeader(this"))
        assertTrue(activity.contains("showChrome = false"))
        assertTrue(orbit.contains("VPN выключен"))
        assertTrue(orbit.contains("КАЧЕСТВО"))
        assertTrue(orbit.contains("СКОРОСТЬ ↓ / ↑"))
        assertTrue(orbit.contains("ТРАФИК ↓ / ↑"))
        assertFalse(activity.contains("TERLIMO · TEST"))
        assertFalse(activity.contains("TEST-уз"))
        assertFalse(activity.contains("FLAG_SECURE"))
    }

    @Test fun productSurfacesAndMetricsAreTruthful() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        val catalog = source("src/main/java/xyz/terlimo/test/ServerCatalogView.kt")
        val orbit = source("src/main/java/xyz/terlimo/test/OrbitHomeHeader.kt")
        assertTrue(activity.contains("val helpPanel"))
        assertTrue(activity.contains("Диагностика проверяет VPN, интернет, DNS и доступность соединения"))
        assertTrue(activity.contains("val bottomNavigation"))
        assertTrue(activity.contains("addView(content, LinearLayout.LayoutParams"))
        assertTrue(activity.contains("subscriptionSurface.visibility = View.VISIBLE"))
        assertFalse(catalog.contains("private val onConnect"))
        assertFalse(catalog.contains("text = \"Подключить\""))
        assertTrue(orbit.contains("Подключение…"))
        assertTrue(orbit.contains("traffic.setMetricValue(\"Недоступно\")"))
        assertFalse(orbit.contains("0 / 0 МБ"))
        assertTrue(activity.contains("text = \"Маршрутизация приложений\""))
        assertFalse(activity.contains("Маршрутизация приложений, адресов и DNS"))
    }

    @Test fun killSwitchHasItsOwnBlockedHeaderAndExplicitDisconnectPower() {
        val orbit = source("src/main/java/xyz/terlimo/test/OrbitHomeHeader.kt")
        // The protected hold is not a connect attempt and must not share the "Подключение…" title.
        val connecting = orbit.substringAfter("val connecting = state.phase in setOf(").substringBefore(")")
        assertFalse(connecting.contains("KillSwitch"))
        assertTrue(orbit.contains("val blocked = state.phase == \"KillSwitch\""))
        assertTrue(orbit.contains("blocked -> \"Сеть заблокирована\""))
        assertTrue(orbit.contains("blocked -> \"Трафик заблокирован. Выберите другой сервер или отключите VPN.\""))
        assertTrue(orbit.contains("if (connected || connecting || blocked) \"Отключить VPN\""))
    }

    @Test fun powerUsesExistingManualSelectionAndSingleServiceOwner() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        assertTrue(activity.contains("requestVpnConsentForCurrentSelection()"))
        assertTrue(activity.contains("setAction(\"cancel\")"))
        assertFalse(source("src/main/java/xyz/terlimo/test/OrbitHomeHeader.kt").contains("selectedNodeId ="))
    }

    @Test fun orbitUsesDrawableStateIconsAndCompactRefresh() {
        val orbit = source("src/main/java/xyz/terlimo/test/OrbitHomeHeader.kt")
        assertTrue(orbit.contains("private val power: ImageButton"))
        assertTrue(orbit.contains("R.drawable.ic_vpn_power_off"))
        assertTrue(orbit.contains("R.drawable.ic_vpn_connecting"))
        assertTrue(orbit.contains("R.drawable.ic_vpn_connected"))
        assertTrue(orbit.contains("R.drawable.ic_refresh"))
        assertTrue(orbit.contains("LayoutParams(dp(48), dp(48))"))
        assertFalse(orbit.contains("text = \"⏻\""))
        assertFalse(orbit.contains("text = \"↻\""))
        assertTrue(orbit.contains("LayoutParams(dp(160), dp(160))"))
        assertFalse(orbit.contains("LayoutParams(dp(184), dp(184))"))
        assertTrue(orbit.contains("LayerDrawable(arrayOf(glow, outer, inner))"))
        assertTrue(orbit.contains("setLayerInset(2, dp(5), dp(5), dp(5), dp(5))"))
        assertTrue(orbit.contains("0x3300FE7A"))
        listOf("ic_vpn_power_off.xml", "ic_vpn_connecting.xml", "ic_vpn_connected.xml").forEach { drawable ->
            val vector = source("src/main/res/drawable/$drawable")
            assertTrue(vector.contains("M12,3.5L12,11.5"))
            assertTrue(vector.contains("android:strokeLineCap=\"round\""))
            assertFalse(vector.contains("M13,3h-2v10"))
            assertFalse(vector.contains("M12,2C6.48"))
        }
    }

    @Test fun bottomNavigationRemainsSingleLineAtOrbitViewport() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        assertTrue(activity.contains("configureBottomNavigationButton(icon)"))
        val helper = activity.substringAfter("private fun Button.configureBottomNavigationButton(icon: Int)")
            .substringBefore("private fun requestNodeSelection")
        assertTrue(helper.contains("minWidth = 0"))
        assertTrue(helper.contains("minHeight = dp(48)"))
        assertTrue(helper.contains("isAllCaps = false"))
        assertTrue(helper.contains("background = null"))
        assertTrue(helper.contains("setCompoundDrawablesRelativeWithIntrinsicBounds"))
        assertTrue(helper.contains("compoundDrawableTintList"))
        assertTrue(helper.contains("TerlimoCatalogBrandTokens.ACCENT"))
        assertTrue(helper.contains("maxLines = 1"))
        assertFalse(helper.contains("HorizontalScrollView"))
        for (label in listOf("Главная", "Подписка", "Настройки", "Помощь"))
            assertTrue(activity.contains("destination(\"$label\""))
    }

    @Test fun idleCatalogActionUsesOrbitPrimaryStyle() {
        val catalog = source("src/main/java/xyz/terlimo/test/ServerCatalogView.kt")
        val idle = catalog.substringAfter("private fun addIdle()").substringBefore("private fun addError")
        assertTrue(idle.contains("Загрузить каталог"))
        assertTrue(idle.contains("TerlimoCatalogBrandTokens.ACCENT"))
        assertTrue(idle.contains("TerlimoCatalogBrandTokens.BACKGROUND"))
        assertTrue(idle.contains("LayoutParams.MATCH_PARENT, dp(48)"))
    }
}
