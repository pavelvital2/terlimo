package xyz.terlimo.test

/** Atomic ownership boundary shared by manual window results and request cleanup. */
internal class ManualCaptchaPendingOwner<T> {
    data class Pending<T>(val owner: String, val value: T)

    private var pending: Pending<T>? = null

    @Synchronized fun current(): Pending<T>? = pending

    @Synchronized fun replace(request: Pending<T>): Pending<T>? {
        val previous = pending
        pending = request
        return previous
    }

    /** Invalidate before delivering a result, so duplicate callbacks cannot deliver twice. */
    @Synchronized fun consume(owner: String): Pending<T>? {
        val current = pending ?: return null
        if (current.owner != owner) return null
        pending = null
        return current
    }
}
