package xyz.terlimo.test

/** Loss callbacks outrank cached capabilities until a subsequent availability event. */
internal class PhysicalAvailability<T : Any> {
    private val lost = java.util.concurrent.ConcurrentHashMap.newKeySet<T>()
    fun lost(network: T) { lost.add(network) }
    fun available(network: T) { lost.remove(network) }
    fun accepts(network: T): Boolean = network !in lost
}
