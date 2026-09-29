package xyz.terlimo.test

import android.graphics.Bitmap
import android.graphics.Canvas
import android.view.View
import android.widget.ImageButton
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class OrbitHoldRenderTest {
    @Test fun powerRendersConnectedOffAndBlockedWithoutChangingDisconnectAction() {
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        instrumentation.runOnMainSync {
            var powerTaps = 0
            val header = OrbitHomeHeader(instrumentation.targetContext, { powerTaps++ }, {})
            val power = header.getChildAt(1) as ImageButton
            val size = (160 * header.resources.displayMetrics.density + 0.5f).toInt()
            power.measure(View.MeasureSpec.makeMeasureSpec(size, View.MeasureSpec.EXACTLY),
                View.MeasureSpec.makeMeasureSpec(size, View.MeasureSpec.EXACTLY))
            power.layout(0, 0, size, size)

            fun redPixels(): Int {
                val bitmap = Bitmap.createBitmap(size, size, Bitmap.Config.ARGB_8888)
                power.draw(Canvas(bitmap))
                var count = 0
                for (y in 0 until size) for (x in 0 until size) {
                    val color = bitmap.getPixel(x, y)
                    val red = color shr 16 and 255
                    val green = color shr 8 and 255
                    val blue = color and 255
                    if (red >= 235 && green in 80..150 && blue in 80..150) count++
                }
                bitmap.recycle()
                return count
            }

            header.render(ViewState(phase = "Idle"))
            assertEquals("VPN выключен", (header.getChildAt(2) as android.widget.TextView).text.toString())
            assertEquals("Подключить VPN", power.contentDescription.toString())
            assertFalse(power.isEnabled)
            assertEquals(0, redPixels())

            header.render(ViewState(phase = "Connected"))
            assertEquals("VPN подключён", (header.getChildAt(2) as android.widget.TextView).text.toString())
            assertEquals("Отключить VPN", power.contentDescription.toString())
            assertTrue(power.isEnabled)
            assertEquals(0, redPixels())

            header.render(ViewState(phase = "KillSwitch", error = "LEASE_EXPIRED"))
            assertEquals("Сеть заблокирована", (header.getChildAt(2) as android.widget.TextView).text.toString())
            assertEquals("Отключить VPN", power.contentDescription.toString())
            assertTrue(power.isEnabled)
            assertTrue("HOLD power must be visibly red", redPixels() > 100)
            power.performClick()
            assertEquals(1, powerTaps)
        }
    }
}
