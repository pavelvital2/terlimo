package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ImportSurfaceSourceTest {
    private fun source(path: String): String = listOf(File(path), File("testapp/$path"))
        .first { it.isFile }.readText()

    @Test fun externalIntentIsConsumedOnceAcrossRecreation() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        assertTrue(activity.contains("STATE_EXTERNAL_INTENT_CONSUMED"))
        assertTrue(activity.contains("if (externalIntentConsumed) return"))
        assertTrue(activity.contains("externalIntentConsumed = true"))
        assertTrue(activity.contains("outState.putBoolean(STATE_EXTERNAL_INTENT_CONSUMED"))
        assertFalse(activity.contains("Log."))
        assertFalse(activity.contains("println("))
    }

    @Test fun allImportSurfacesConvergeOnSingleNormalizer() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        val method = activity.substringAfter("private fun acceptImportInput(")
            .substringBefore("private fun decodeQrImage(")
        assertTrue(method.contains("SubscriptionImportInput.normalize(raw)"))
        assertTrue(activity.contains("Intent.ACTION_VIEW"))
        assertTrue(activity.contains("Intent.ACTION_SEND"))
        assertTrue(activity.contains("QrImageDecoder.decode"))
        assertTrue(activity.contains("SubscriptionTextFileReader.read"))
    }

    @Test fun manifestExposesOnlyExpectedIngressAndKeepsServicePrivate() {
        val manifest = source("src/main/AndroidManifest.xml")
        assertTrue(manifest.contains("android:scheme=\"whitelists\" android:host=\"subscription\""))
        assertTrue(manifest.contains("android.intent.action.SEND"))
        assertTrue(manifest.contains("android:mimeType=\"text/plain\""))
        assertTrue(manifest.contains(".SessionService\" android:exported=\"false\""))
    }

    @Test fun subscriptionPickerAcceptsCanonicalLinkMimeWithoutWeakeningContentGate() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        val picker = activity.substringAfter("text = \"Открыть файл подписки\"")
            .substringBefore("}, REQUEST_SUBSCRIPTION_FILE)")
        assertTrue(picker.contains("Intent.ACTION_OPEN_DOCUMENT"))
        assertTrue(picker.contains("Intent.CATEGORY_OPENABLE"))
        assertTrue(picker.contains("type = \"*/*\""))
        assertTrue(picker.contains("Intent.EXTRA_MIME_TYPES"))
        assertTrue(picker.contains("arrayOf(\"text/plain\", \"application/octet-stream\")"))
        assertFalse(picker.contains("type = \"text/*\""))

        val handoff = activity.substringAfter("if (requestCode == REQUEST_SUBSCRIPTION_FILE)")
            .substringBefore("if (requestCode == REQUEST_QR_IMAGE)")
        assertTrue(handoff.contains("SubscriptionTextFileReader.read"))
        assertTrue(handoff.contains("acceptImportInput"))
        assertTrue(activity.substringAfter("private fun acceptImportInput(")
            .substringBefore("private fun decodeQrImage(")
            .contains("SubscriptionImportInput.normalize(raw)"))
    }
}
