package xyz.terlimo.test

import android.view.View
import android.view.ViewGroup
import android.widget.Button
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

/**
 * §29.3 isolated render check of the production ServerCatalogView with a fixture ViewState.
 * It proves which error surfaces show exactly one retry + one support action, separated from
 * any live network failure. No release hook, no native, no permission or server change.
 */
@RunWith(AndroidJUnit4::class)
class CatalogErrorCardFixtureTest {
    private fun texts(root: View): List<String> {
        val out = mutableListOf<String>()
        fun walk(v: View) {
            if (v is Button) v.text?.toString()?.let(out::add)
            if (v is ViewGroup) for (i in 0 until v.childCount) walk(v.getChildAt(i))
        }
        walk(root)
        return out
    }

    private fun render(state: ViewState, refreshes: IntArray): ServerCatalogView {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val view = ServerCatalogView(context,
            onRefresh = { refreshes[0]++ }, onSelect = {}, onProbe = {}, onProbeAll = {},
            onProbeAllCancel = {}, showChrome = false)
        InstrumentationRegistry.getInstrumentation().runOnMainSync { view.render(state) }
        return view
    }

    @Test fun browseOfflineShowsExactlyOneRetryAndOneSupport() {
        val refreshes = intArrayOf(0)
        val view = render(ViewState(phase = "BootstrapConnecting", displayMode = CatalogDisplayMode.BROWSE,
            browseError = "TRANSPORT"), refreshes)
        val texts = texts(view)
        assertEquals(1, texts.count { it == "Попробовать ещё раз" })
        assertEquals(1, texts.count { it == HelpContent.SUPPORT_LABEL })
    }

    @Test fun credentialErrorShowsExactlyOneRetryAndOneSupport() {
        val view = render(ViewState(phase = "Error", error = "CATALOG_TIMEOUT"), intArrayOf(0))
        val texts = texts(view)
        assertEquals(1, texts.count { it == "Попробовать ещё раз" })
        assertEquals(1, texts.count { it == HelpContent.SUPPORT_LABEL })
    }

    @Test fun connectedSwitchFailureHasNoUpdateActions() {
        val view = render(ViewState(phase = "Connected", error = "TRANSPORT"), intArrayOf(0))
        val texts = texts(view)
        assertTrue(texts.none { it == "Попробовать ещё раз" })
        assertTrue(texts.none { it == HelpContent.SUPPORT_LABEL })
    }
}
