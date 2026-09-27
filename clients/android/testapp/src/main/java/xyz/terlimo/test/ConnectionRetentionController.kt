package xyz.terlimo.test

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.os.SystemClock
import java.util.UUID

/** Main-thread policy/timers only. SessionService owns native join and blocked TUN. */
internal class ConnectionRetentionController(private val context: Context,
    private val pause: () -> Unit, private val resume: () -> Unit) : AutoCloseable {
    private val main = Handler(Looper.getMainLooper())
    private val power = context.getSystemService(PowerManager::class.java)
    private val connectivity = context.getSystemService(ConnectivityManager::class.java)
    private val locks = ConnectionRetentionLocks(context)
    private val alarm = RetentionAlarmScheduler(context, UUID.randomUUID().toString()) { _, deadline ->
        if (!closed && cycle?.resumeAt == deadline) maybeResume()
    }
    private var mode = RetentionMode.BALANCED
    private var pauseMinutes = SleepRetentionPolicy.DEFAULT_PAUSE_MINUTES
    private var resumeMinutes: Int? = null
    private var cycle: SleepRetentionCycle? = null
    private var sleepCycleFinished = false
    private var epoch = 0L
    private var closed = false
    private var running = false
    private var paused = false
    private var physical: Network? = null
    private var interactive = power.isInteractive
    private var shortGuard: PowerManager.WakeLock? = null
    private var transition: PowerManager.WakeLock? = null

    fun loadForConnection() {
        val preferences = context.getSharedPreferences("transport_retention", Context.MODE_PRIVATE)
        mode = RetentionMode.fromStored(preferences.getString("mode", null))
        pauseMinutes = preferences.getInt("pause_minutes", SleepRetentionPolicy.DEFAULT_PAUSE_MINUTES)
        resumeMinutes = if (preferences.getBoolean("timed_resume", false)) preferences.getInt("resume_minutes", 5) else null
        sleepCycleFinished = false
        invalidateCycle()
    }

    fun transitionFor(milliseconds: Long) {
        if (closed) return
        transition?.let { if (it.isHeld) it.release() }
        transition = power.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "terlimo:transition").apply {
            setReferenceCounted(false); acquire(milliseconds)
        }
    }

    fun update(connected: Boolean, policyPaused: Boolean, network: Network?) {
        if (closed) return
        running = connected
        paused = policyPaused
        physical = network
        if (!connected && !policyPaused && cycle != null) invalidateCycle()
        if (connected) transition?.let { if (it.isHeld) it.release() }
        reconcile()
        if (running && !interactive && !paused && mode == RetentionMode.SAVE_BATTERY && cycle == null) startCycle()
    }

    fun screenChanged() {
        if (closed) return
        val nowInteractive = power.isInteractive
        if (interactive == nowInteractive) return
        interactive = nowInteractive
        if (interactive) {
            val shouldResume = paused
            invalidateCycle()
            if (shouldResume) resume()
        } else {
            sleepCycleFinished = false
            if (running && !paused && mode == RetentionMode.SAVE_BATTERY) startCycle()
        }
        reconcile()
    }

    private fun startCycle() {
        if (sleepCycleFinished) return
        invalidateCycle()
        val current = SleepRetentionPolicy.begin(epoch, SystemClock.elapsedRealtime(), pauseMinutes, resumeMinutes)
            ?: run { sleepCycleFinished = true; return }
        cycle = current
        guard(current.pauseAt)
        main.postDelayed({
            if (cycle != current || closed) return@postDelayed
            if (SleepRetentionPolicy.pauseDue(current, epoch, SystemClock.elapsedRealtime(), power.isInteractive, closed, running)) {
                paused = true
                reconcile()
                pause()
                releaseGuard()
                current.resumeAt?.let { deadline ->
                    if (runCatching { alarm.schedule(1, deadline) }.isFailure) {
                        sleepCycleFinished = true
                        invalidateCycle()
                        resume()
                        return@let
                    }
                    guard(deadline)
                    main.postDelayed({ if (cycle == current) maybeResume() }, (deadline - SystemClock.elapsedRealtime()).coerceAtLeast(0))
                }
            }
        }, (current.pauseAt - SystemClock.elapsedRealtime()).coerceAtLeast(0))
    }

    private fun maybeResume() {
        val current = cycle ?: return
        if (!SleepRetentionPolicy.resumeDue(current, epoch, SystemClock.elapsedRealtime(), power.isInteractive, closed, paused)) return
        sleepCycleFinished = true
        invalidateCycle()
        // Keep paused until SessionService establishes a new admitted runtime.
        resume()
    }

    private fun reconcile() {
        val capabilities = physical?.let { connectivity.getNetworkCapabilities(it) }
        locks.reconcile(RetentionPolicy.locks(mode, running, paused, closed,
            capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN) == true,
            interactive, capabilities?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true))
    }
    private fun guard(deadline: Long) {
        releaseGuard()
        val duration = SleepRetentionPolicy.shortGuardMillis(deadline, SystemClock.elapsedRealtime()) ?: return
        shortGuard = power.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "terlimo:sleep_timer").apply {
            setReferenceCounted(false); acquire(duration)
        }
    }
    private fun releaseGuard() { shortGuard?.let { if (it.isHeld) it.release() }; shortGuard = null }
    private fun invalidateCycle() { epoch++; cycle = null; alarm.cancel(1); releaseGuard() }
    override fun close() {
        if (closed) return
        closed = true
        invalidateCycle()
        main.removeCallbacksAndMessages(null)
        alarm.close()
        transition?.let { if (it.isHeld) it.release() }
        locks.close()
    }
}
