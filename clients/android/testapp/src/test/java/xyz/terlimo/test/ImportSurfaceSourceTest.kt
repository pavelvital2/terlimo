package xyz.terlimo.test

import java.io.File
import javax.xml.parsers.DocumentBuilderFactory
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** Source acceptance of the deliberately removed legacy UI and intent ingress. */
class ImportSurfaceSourceTest {
    private fun source(path: String): String = listOf(File(path), File("testapp/$path"))
        .first { it.isFile }.readText()

    @Test fun legacyControlsCannotReappearThroughCreationOrRender() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        for (removed in listOf("whitelists://", "Сканировать QR", "QR из файла",
            "Открыть файл подписки", "Импортировать и получить каталог", "Открыть сохранённую подписку",
            "Заменить подписку", "Удалить и заменить", "Обновить сохранённую подписку",
            "STATE_LINK_TEXT", "importButton", "resumeButton", "refreshButton")) {
            assertFalse("legacy control retained: $removed", activity.contains(removed))
        }
        val catalog = source("src/main/java/xyz/terlimo/test/ServerCatalogView.kt")
        assertFalse(catalog.contains("Откройте сохранённую подписку"))
        assertFalse(catalog.contains("импортируйте"))
        assertTrue(catalog.contains("Загрузить каталог"))
    }

    @Test fun externalDataAndOldActivityResultsHaveNoImportHandler() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        for (removed in listOf("handleIncomingIntent", "acceptImportInput", "decodeQrImage",
            "decodeSubscriptionFile", "IntentIntegrator", "SubscriptionImportInput",
            "SubscriptionTextFileReader", "Intent.EXTRA_TEXT", "Intent.ACTION_SEND",
            "REQUEST_QR_IMAGE", "REQUEST_SUBSCRIPTION_FILE", ".setAction(\"import\")")) {
            assertFalse("legacy callback retained: $removed", activity.contains(removed))
        }
        val warm = activity.substringAfter("override fun onNewIntent(")
            .substringBefore("override fun onSaveInstanceState(")
        assertTrue(warm.contains("setIntent(intent)"))
        assertTrue(warm.indexOf("setIntent(intent)") < warm.indexOf("maybeOpenAnnouncements(intent)"))
        assertFalse(warm.contains("dataString"))
        assertFalse(warm.contains("getStringExtra"))
        assertTrue(activity.contains("REQUEST_AUTOCONNECT_CONSENT"))
        assertTrue(activity.contains("if (requestCode != 100) return"))
    }

    @Test fun manifestKeepsLauncherAndPrivateServiceWithoutImportIngress() {
        val text = source("src/main/AndroidManifest.xml")
        val manifest = DocumentBuilderFactory.newInstance().newDocumentBuilder()
            .parse(text.byteInputStream())
        val activities = manifest.getElementsByTagName("activity")
        val main = (0 until activities.length).map { activities.item(it) }
            .single { it.attributes.getNamedItem("android:name").nodeValue == ".MainActivity" }
        val actions = main.childNodes.let { children ->
            (0 until children.length).map { children.item(it) }.filter { it.nodeName == "intent-filter" }
        }.flatMap { filter ->
            (0 until filter.childNodes.length).map { filter.childNodes.item(it) }
                .filter { it.nodeName == "action" }
                .map { it.attributes.getNamedItem("android:name").nodeValue }
        }
        assertEquals(listOf("android.intent.action.MAIN"), actions)
        assertEquals("singleTop", main.attributes.getNamedItem("android:launchMode").nodeValue)
        assertFalse(text.contains("whitelists"))
        assertFalse(text.contains("android.intent.action.SEND"))
        assertFalse(text.contains(".QrCaptureActivity"))
        assertTrue(text.contains(".SessionService\" android:exported=\"false\""))
    }

    @Test fun modernResumeAndCatalogRefreshRemainSeparateFromRecovery() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        val resume = activity.substringAfter("private fun autoLoadSavedSubscription()")
            .substringBefore("\n    }")
        assertTrue(resume.contains("ProcessAutoLoad.claim()"))
        assertTrue(resume.contains(".setAction(\"resume\")"))
        assertTrue(activity.contains("onRefresh = { startForegroundService(Intent(this, SessionService::class.java).setAction(\"refresh\")) }"))
        assertTrue(activity.contains(".setAction(\"recovery_apply\").putExtra(\"recovery_code\", code)"))
        assertEquals(1, Regex("RecoveryCodeUi\\.showEditor").findAll(activity).count())
        assertTrue(activity.contains("recoveryErrorButton"))
        assertTrue(activity.contains("maybeOpenAnnouncements(intent)"))
    }

    @Test fun subscriptionCommerceDevicesTelegramAndFiveDestinationsRemain() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        for (action in listOf("purchase_plans", "purchase_pay", "purchase_check", "devices_refresh",
            "telegram_login", "telegram_refresh")) {
            assertTrue("missing modern action: $action", activity.contains(".setAction(\"$action\")"))
        }
        assertTrue(activity.contains("BottomNavigation.destinations.forEach"))
        val navigation = source("src/main/java/xyz/terlimo/test/BottomNavigation.kt")
        for (label in listOf("Главная", "Подписка", "Маршрутизация", "Настройки", "Помощь")) {
            assertTrue("missing destination: $label", navigation.contains("NavDestination(\"$label\""))
        }
        assertFalse(activity.contains(".replaceSubscription()"))
        assertFalse(activity.contains("Log."))
        assertFalse(activity.contains("println("))
    }
}
