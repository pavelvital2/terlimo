package xyz.terlimo.test

import android.app.AlarmManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/** Alarms can notify an existing owner only. They never start a stopped service. */
class RetentionAlarmReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val token = intent.getStringExtra("owner") ?: return
        val kind = intent.getIntExtra("kind", -1)
        val deadline = intent.getLongExtra("deadline", -1)
        val listener = synchronized(callbacks) { callbacks[token] } ?: return
        listener(kind, deadline)
    }
    companion object {
        internal val callbacks = mutableMapOf<String, (Int, Long) -> Unit>()
    }
}

internal class RetentionAlarmScheduler(private val context: Context, private val owner: String,
    listener: (Int, Long) -> Unit) : AutoCloseable {
    private val alarms = context.getSystemService(AlarmManager::class.java)
    private val pending = mutableMapOf<Int, PendingIntent>()
    private var closed = false
    init { synchronized(RetentionAlarmReceiver.callbacks) { RetentionAlarmReceiver.callbacks[owner] = listener } }

    @Synchronized fun schedule(kind: Int, deadline: Long) {
        check(!closed) { "RETENTION_TIMER_CLOSED" }
        cancel(kind)
        val intent = Intent(context, RetentionAlarmReceiver::class.java)
            .setAction("xyz.terlimo.test.RETENTION_$owner")
            .putExtra("owner", owner).putExtra("kind", kind).putExtra("deadline", deadline)
        val token = PendingIntent.getBroadcast(context, kind, intent,
            PendingIntent.FLAG_CANCEL_CURRENT or PendingIntent.FLAG_IMMUTABLE)
        pending[kind] = token
        // Same v18 fallback when no special exact-alarm access is granted.
        alarms.setAndAllowWhileIdle(AlarmManager.ELAPSED_REALTIME_WAKEUP, deadline, token)
    }
    @Synchronized fun cancel(kind: Int) { pending.remove(kind)?.let { alarms.cancel(it); it.cancel() } }
    @Synchronized override fun close() {
        closed = true
        pending.keys.toList().forEach(::cancel)
        synchronized(RetentionAlarmReceiver.callbacks) { RetentionAlarmReceiver.callbacks.remove(owner) }
    }
}
