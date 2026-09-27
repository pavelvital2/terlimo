package xyz.terlimo.test

import android.content.Context
import android.net.wifi.WifiManager
import android.os.PowerManager

/** Owned by SessionService; never controls native workers or lease expiry. */
internal class ConnectionRetentionLocks(context: Context) : AutoCloseable {
    private val power = context.getSystemService(PowerManager::class.java)
    private val wifiManager = context.applicationContext.getSystemService(WifiManager::class.java)
    private var cpu: PowerManager.WakeLock? = null
    private var wifi: WifiManager.WifiLock? = null

    @Synchronized fun reconcile(wanted: RetentionLocks): RetentionLocks {
        if (!wanted.wifi) releaseWifi()
        if (!wanted.cpu) {
            releaseCpu()
        } else if (cpu?.isHeld != true) {
            val lock = power.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "terlimo:connection_cpu")
            lock.setReferenceCounted(false)
            cpu = lock
            runCatching { lock.acquire() }
        }
        if (wanted.wifi && cpu?.isHeld == true && wifi?.isHeld != true) {
            // Same as v18: LOW_LATENCY is ineffective with the screen off.
            // Android may still limit the radio; isHeld is not Internet evidence.
            @Suppress("DEPRECATION")
            val lock = wifiManager.createWifiLock(WifiManager.WIFI_MODE_FULL_HIGH_PERF, "terlimo:connection_wifi")
            lock.setReferenceCounted(false)
            wifi = lock
            runCatching { lock.acquire() }
        }
        return RetentionLocks(cpu?.isHeld == true, wifi?.isHeld == true)
    }

    private fun releaseWifi() { wifi?.let { if (it.isHeld) it.release() }; wifi = null }
    private fun releaseCpu() { cpu?.let { if (it.isHeld) it.release() }; cpu = null }
    @Synchronized override fun close() { try { releaseWifi() } finally { releaseCpu() } }
}
