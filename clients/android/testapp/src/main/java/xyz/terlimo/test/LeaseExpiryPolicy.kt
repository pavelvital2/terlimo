package xyz.terlimo.test

internal object LeaseExpiryPolicy {
    fun merge(previous: Long, sameLease: Boolean, candidate: Long): Long =
        if (sameLease && previous > 0) minOf(previous, candidate) else candidate
    fun due(deadline: Long, nowElapsed: Long): Boolean =
        deadline in 1 until Long.MAX_VALUE && nowElapsed >= deadline
}
