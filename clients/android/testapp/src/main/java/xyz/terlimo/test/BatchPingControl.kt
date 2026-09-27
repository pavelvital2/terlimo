package xyz.terlimo.test

/**
 * Pure visibility decision for the manual "Пинг всех" batch-ping control (S5 §07.2).
 *
 * The control is available for a usable catalog both pre-connect (`CatalogReady`) and
 * while `Connected` (the native branch probes the current node on the live session and
 * every other node on its own fenced direct echo connection, without switching). It is
 * never rendered in read-only/retained, idle, error or switching states, and it never
 * starts a probe on its own.
 */
internal object BatchPingControl {
    fun shouldRender(readOnly: Boolean, phase: String, nodeCount: Int): Boolean =
        !readOnly && nodeCount > 0 && phase in setOf("CatalogReady", "Connected")
}
