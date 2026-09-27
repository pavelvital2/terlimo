package xyz.terlimo.test

import android.content.pm.ApplicationInfo
import java.nio.file.Files
import java.nio.file.Paths
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class RoutingSystemFilterTest {
    private val src = String(
        Files.readAllBytes(Paths.get("src/main/java/xyz/terlimo/test/RoutingSettingsActivity.kt")),
        Charsets.UTF_8,
    )

    @Test fun systemRowsHiddenOnlyWhenFilterOff() {
        assertTrue(RoutingSystemFilter.isVisible(isSystem = true, showSystem = true))
        assertFalse(RoutingSystemFilter.isVisible(isSystem = true, showSystem = false))
        assertTrue(RoutingSystemFilter.isVisible(isSystem = false, showSystem = false))
    }

    @Test fun systemClassificationCoversPreinstalledAndUpdatedSystemApps() {
        assertTrue(RoutingSystemFilter.isSystem(ApplicationInfo.FLAG_SYSTEM))
        assertTrue(RoutingSystemFilter.isSystem(ApplicationInfo.FLAG_UPDATED_SYSTEM_APP))
        assertTrue(RoutingSystemFilter.isSystem(
            ApplicationInfo.FLAG_SYSTEM or ApplicationInfo.FLAG_UPDATED_SYSTEM_APP))
        assertFalse(RoutingSystemFilter.isSystem(0))
    }

    @Test fun quickSelectionUsesInstalledMatchesRegardlessOfVisibility() {
        val installed = setOf("com.example.a", "com.example.b", "com.example.system")
        val quick = setOf("com.example.b", "com.example.system", "com.example.missing")
        assertEquals(
            setOf("com.example.b", "com.example.system"),
            RoutingSystemFilter.quickAdditions(installed, quick, emptySet()),
        )
        // Already-selected packages are not re-added.
        assertEquals(
            setOf("com.example.system"),
            RoutingSystemFilter.quickAdditions(installed, quick, setOf("com.example.b")),
        )
        // Nothing installed -> nothing added (not a visibility question).
        assertEquals(emptySet<String>(), RoutingSystemFilter.quickAdditions(emptySet(), quick, emptySet()))
    }

    @Test fun quickSelectionSourceIsStateBasedAndDoesNotForceVisibility() {
        val quick = src.substringAfter("private fun applyQuickExclusions()").substringBefore("private fun save()")
        assertTrue(quick.contains("RoutingSystemFilter.quickAdditions(installed.keys, QUICK_PACKAGES, checked)"))
        assertTrue(quick.contains("checked += added"))
        assertFalse(quick.contains("showSystem = true"))
        assertFalse(quick.contains("appList.childCount"))
        // The display filter path never clears the persisted selection.
        assertTrue(src.contains("RoutingSystemFilter.isVisible(system, showSystem)"))
        assertFalse(src.contains("checked.clear()"))
    }

    @Test fun visibleFilterSwitchAndSaveOutsideScroll() {
        assertTrue(src.contains("\"Показать системные приложения\""))
        val bottom = src.substringAfter("val bottomBar").substringBefore("root.addView(bottomBar)")
        assertTrue(bottom.contains("\"Сохранить\""))
        val scroll = src.substringAfter("root.addView(ScrollView(this)").substringBefore("val bottomBar")
        assertFalse(scroll.contains("\"Сохранить\""))
    }

    @Test fun systemInsetsApplied() {
        assertTrue(src.contains("setOnApplyWindowInsetsListener"))
        assertTrue(src.contains("WindowInsets.Type.systemBars()"))
        assertTrue(src.contains("systemWindowInsetTop"))
    }

    @Test fun touchTargetsAtLeast48dp() {
        val save = src.substringAfter("\"Сохранить\"").substringBefore("setOnClickListener")
        assertTrue(save.contains("minimumHeight = dp(48)"))
        val switch = src.substringAfter("\"Показать системные приложения\"")
            .substringBefore("setOnCheckedChangeListener")
        assertTrue(switch.contains("minHeight = dp(48)"))
    }

    @Test fun reopeningKeepsPersistedRevisionAndEmptyIncludeRejected() {
        assertTrue(src.contains("compareAndSetRoutingSettings(loaded?.revision"))
        assertTrue(src.contains("loaded = it"))
        assertFalse(RoutingSettingsApply.shouldPersist(AppRoutingMode.INCLUDE_ONLY, emptySet()))
        assertTrue(RoutingSettingsApply.shouldPersist(AppRoutingMode.INCLUDE_ONLY, setOf("com.example.app")))
    }

    @Test fun homeAffordanceIsFixedOutsideScrollAndReturnsHome() {
        assertTrue(src.contains("configureHomeButton(R.drawable.ic_nav_home)"))
        val home = src.substringAfter("val homeBar").substringBefore("root.addView(homeBar)")
        assertTrue(home.contains("\"Главная\""))
        assertTrue(home.contains("MainActivity::class.java"))
        assertTrue(home.contains("Intent.FLAG_ACTIVITY_CLEAR_TOP"))
        assertTrue(home.contains("finish()"))
        // Home and Save live outside the scrollable content container.
        val contentBlock = src.substringAfter("val content = LinearLayout")
            .substringBefore("root.addView(ScrollView(this)")
        assertFalse(contentBlock.contains("\"Главная\""))
        assertFalse(contentBlock.contains("\"Сохранить\""))
        assertTrue(src.contains("root.addView(homeBar)"))
    }

    @Test fun searchMatchesLabelOrPackageAndOnlyFiltersRendering() {
        assertTrue(RoutingSystemFilter.matchesDisplay("Сбербанк", "ru.sberbankmobile", ""))
        assertTrue(RoutingSystemFilter.matchesDisplay("Сбербанк", "ru.sberbankmobile", "сбер"))
        assertTrue(RoutingSystemFilter.matchesDisplay("Сбербанк", "ru.sberbankmobile", "SBERBANK"))
        assertFalse(RoutingSystemFilter.matchesDisplay("Сбербанк", "ru.sberbankmobile", "tinkoff"))
        val render = src.substringAfter("private fun renderAppList()").substringBefore("private fun applyQuickExclusions()")
        assertTrue(render.contains("RoutingSystemFilter.matchesDisplay(label, packageName, query)"))
        assertFalse(render.contains("checked.clear()"))
    }

    private val hintSrc = String(
        Files.readAllBytes(Paths.get("src/main/java/xyz/terlimo/test/RoutingModeHint.kt")), Charsets.UTF_8)

    @Test fun requiredModeLabelsSearchAndDefaultHiddenSystem() {
        listOf(
            "Все через VPN", "Выбранные через VPN", "Выбранные мимо VPN",
            "Выбрать приложения из белого списка", "Показать системные приложения", "Поиск приложений",
        ).forEach { assertTrue("missing UI text: $it", src.contains(it)) }
        // System rows are hidden by default until the explicit switch is turned on.
        assertTrue(src.contains("private var showSystem = false"))
    }

    @Test fun singleDynamicHintFollowsEachModeIncludingInitialLoad() {
        // The single hint text for every mode.
        assertEquals(RoutingModeHint.ALL, RoutingModeHint.text(AppRoutingMode.DISABLED))
        assertEquals(RoutingModeHint.INCLUDE, RoutingModeHint.text(AppRoutingMode.INCLUDE_ONLY))
        assertEquals(RoutingModeHint.EXCLUDE, RoutingModeHint.text(AppRoutingMode.EXCLUDE))
        for (text in listOf(RoutingModeHint.ALL, RoutingModeHint.INCLUDE, RoutingModeHint.EXCLUDE)) {
            assertTrue("missing hint text: $text", hintSrc.contains(text))
        }
        // Exactly one hint view, wired to the mode on both the initial render and every change.
        assertEquals(1, Regex(Regex.escape("content.addView(modeHint)")).findAll(src).count())
        assertFalse(src.contains("content.addView(hint(\"Весь трафик"))
        assertTrue(src.contains("setOnCheckedChangeListener { _, id -> modeHint.text = RoutingModeHint.text(modeForChecked(id)) }"))
        assertTrue(src.contains("modeHint.text = RoutingModeHint.text(mode)"))
        // Initial render uses the saved mode (not a hard-coded hint).
        val render = src.substringAfter("private fun renderCurrent()").substringBefore("private fun renderAppList()")
        assertTrue(render.contains("current?.routing?.apps?.mode ?: AppRoutingMode.DISABLED"))
        assertTrue(render.contains("modeHint.text = RoutingModeHint.text(mode)"))
    }
}
