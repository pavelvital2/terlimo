package xyz.terlimo.test

/** Authoritative WireGuard tunnel state for routing-edit decisions. */
internal enum class TunnelApplicationState { NONE, APPLYING, APPLIED }

/** Consistent, read-only snapshot of the routing-relevant session state. */
internal data class RoutingEditSnapshot(val phase: String, val tunnel: TunnelApplicationState)

/** Pure derivation of [TunnelApplicationState] from the authoritative session markers. */
internal object RoutingEditState {
    fun tunnelOf(
        activeVpnConfig: Boolean,
        sleepPaused: Boolean,
        networkRecovery: Boolean,
        nativeStopped: Boolean,
        applying: Boolean,
    ): TunnelApplicationState = when {
        activeVpnConfig || sleepPaused || networkRecovery || !nativeStopped -> TunnelApplicationState.APPLIED
        applying -> TunnelApplicationState.APPLYING
        else -> TunnelApplicationState.NONE
    }
}

/**
 * Single source of truth for when the routing editor may persist a policy.
 *
 * Routing is applied only on the next Connect, so editing is allowed whenever there is no
 * actually applied/applying tunnel. A phase alone is NOT sufficient: during recovery or
 * sleep-resume the previous WireGuard tunnel is still UP while the phase may pass through
 * `CatalogReady`, so the decision combines the phase with the authoritative tunnel state.
 *
 * Pure Kotlin (no Android API) so the gate is directly unit-testable.
 */
internal object RoutingEditGate {
    private val EDITABLE = setOf("Idle", "Error", "CatalogReady")

    fun allowsSave(snapshot: RoutingEditSnapshot): Boolean =
        snapshot.tunnel == TunnelApplicationState.NONE && snapshot.phase in EDITABLE
}
