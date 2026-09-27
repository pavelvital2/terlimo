package xyz.terlimo.test

import java.net.SocketTimeoutException

internal class ProbeBudget(private val deadline: Long, private val now: () -> Long) {
    val expired: Boolean get() = now() >= deadline
    fun timeoutMs(cap: Int = 4000): Int {
        val remaining = deadline - now()
        if (remaining <= 0) throw SocketTimeoutException()
        return remaining.coerceAtMost(cap.toLong()).toInt()
    }
}
