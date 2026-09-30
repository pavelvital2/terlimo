package xyz.terlimo.test

import com.journeyapps.barcodescanner.CaptureActivity

class QrCaptureActivity : CaptureActivity() {
    override fun attachBaseContext(newBase: android.content.Context) {
        // §26.1: helper Activity resolves the stored per-user theme too.
        super.attachBaseContext(AppTheme.wrap(newBase))
    }
}
