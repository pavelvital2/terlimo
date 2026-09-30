package xyz.terlimo.test

/**
 * §26.2 adapter between the pure controller and the single catalog/attempt writer.
 *
 * Every effect is executed with the generation/account/target captured at planning time and is
 * revalidated against the live ports immediately before any native command:
 * attempt == gate.active, armed token/stage, preference, !stopping, account scope and the
 * current data right. A stale or foreign effect is dropped without touching native.
 *
 * The choose/select sequence shares ONE [ConnectDeadline] budget: the deadline starts at the
 * choose send and the ack timeout is the remaining part of the same window, never a second
 * full budget. Timeouts and lost rights are visible completions, never a fallback node.
 */
internal interface AutoConnectPorts {
    /** The service-side generation currently active (validated before assignment). */
    fun activeGeneration(): Long

    fun prefEnabled(generation: Long): Boolean

    fun gateActive(): String?

    fun stopping(): Boolean

    fun phase(): String

    fun nodes(): List<NodeLabel>

    fun selectedNodeId(): String

    fun pendingNodeId(): String?

    fun accountRef(): String?

    fun entitlementUsable(): Boolean

    /** Android VPN consent already granted (VpnService.prepare(this) == null). */
    fun vpnConsentGranted(): Boolean

    /** Real data intent: applied tunnel or an already started explicit connect deadline. */
    fun dataIntentActive(): Boolean

    fun lastNodeId(accountRef: String): String?

    /** Queue once; all command ports below execute synchronously on this writer. */
    fun dispatch(action: () -> Unit)

    fun clearDeadline(attempt: String)

    fun beginAttempt()

    fun sendChoose(nodeId: String)

    fun sendSelect(nodeId: String, attempt: String)

    /** Starts the single ConnectDeadline for this attempt; false when another attempt owns it. */
    fun startDeadline(attempt: String, now: Long): Boolean

    /** Existing deadline end for the attempt, or the sentinel when none is active. */
    fun deadlineEnd(attempt: String): Long

    /** Schedules the remaining budget callback for the attempt (idempotent on the caller side). */
    fun armDeadlineTimeout(attempt: String, delayMillis: Long, onTimeout: () -> Unit)

    fun publishError(code: String)

    /** Only an attempt started by this auto-connect may be terminated on timeout. */
    fun terminateAutoAttempt(attempt: String)

    fun now(): Long
}

internal class AutoConnectAdapter(
    private val controller: AutoConnectController,
    private val ports: AutoConnectPorts,
) {
    /** True only while this adapter started the attempt that is still running. */
    private var startedAttempt: String? = null
    private var startedByAuto = false
    private var choosingAttempt: String? = null

    fun stage(): AutoConnectController.Stage = controller.stage

    fun isArmedFor(generation: Long): Boolean = controller.isArmedFor(generation)

    fun armTimeoutFallback() {
        val attempt = startedAttempt ?: return
        ports.terminateAutoAttempt(attempt)
    }

    /** User launch. [generation] is already the validated, active service generation. */
    @Synchronized fun onLaunch(generation: Long) {
        if (generation != ports.activeGeneration() || !ports.prefEnabled(generation)) return
        if (!controller.isArmedFor(generation) && choosingAttempt != null) cancel()
        apply(controller.onLaunch(runtime(generation)), generation, ports.gateActive())
    }

    /** Fresh account projection of the current attempt. */
    @Synchronized fun onAccount(attempt: String, generation: Long) {
        apply(controller.onAccount(runtime(generation)), generation, attempt)
    }

    /** Verified credential catalog of the current attempt. */
    @Synchronized fun onCatalog(attempt: String) {
        if (attempt != ports.gateActive()) return
        // A queued choose has not yet established the selection we are waiting for.
        if (controller.stage == AutoConnectController.Stage.CHOOSE_SENT && choosingAttempt != attempt) return
        val generation = ports.activeGeneration()
        if (!controller.isArmedFor(generation)) return
        apply(controller.onCatalog(runtime(generation), ports.selectedNodeId(), ports.pendingNodeId() != null), generation, attempt)
        if (controller.stage == AutoConnectController.Stage.SELECT_READY) {
            apply(controller.onSelectOpportunity(runtime(generation), consentGranted()), generation, attempt)
        }
    }

    @Synchronized fun onConnected(nodeId: String) {
        controller.onConnected(nodeId)
        startedAttempt = null
        startedByAuto = false
        choosingAttempt = null
    }

    /** The single deadline window expired without a confirmed selection. */
    @Synchronized fun onDeadlineTimeout(attempt: String, generation: Long) {
        if (generation != ports.activeGeneration() || !controller.isArmedFor(generation)) return
        if (controller.stage != AutoConnectController.Stage.CHOOSE_SENT) return
        if (attempt != ports.gateActive()) return
        choosingAttempt = null
        ports.clearDeadline(attempt)
        apply(controller.onAckTimeout(), generation, attempt)
        if (startedByAuto && attempt == ports.gateActive()) ports.terminateAutoAttempt(attempt)
    }

    /** Off / Disconnect / cancel / import / teardown. */
    @Synchronized fun cancel() {
        controller.disarm()
        choosingAttempt?.let { ports.clearDeadline(it) }
        choosingAttempt = null
        startedAttempt = null
        startedByAuto = false
    }

    private fun consentGranted(): Boolean = ports.vpnConsentGranted()

    private fun runtime(generation: Long): AutoConnectRuntime {
        val accountRef = ports.accountRef()
        return AutoConnectRuntime(
            enabled = ports.prefEnabled(generation) && generation == ports.activeGeneration(),
            generation = generation,
            accountKnown = accountRef != null,
            accountRef = accountRef,
            lastNodeId = accountRef?.let { ports.lastNodeId(it) },
            nodePresent = controller.target?.let { want -> ports.nodes().any { it.id == want } } ?: true,
            entitlementUsable = ports.entitlementUsable(),
            dataConnected = ports.dataIntentActive() && !(choosingAttempt != null && choosingAttempt == ports.gateActive()),
        )
    }

    private fun apply(effects: List<AutoConnectEffect>, generation: Long, capturedAttempt: String?) {
        for (effect in effects) when (effect) {
            AutoConnectEffect.None -> Unit
            AutoConnectEffect.Consume -> {
                choosingAttempt?.let { ports.clearDeadline(it) }
                choosingAttempt = null
            }
            AutoConnectEffect.StartAttempt -> ports.dispatch { synchronized(this) {
                if (generation == ports.activeGeneration() && ports.prefEnabled(generation) &&
                    controller.isArmedFor(generation) && ports.gateActive() == null && !ports.stopping()) {
                    startedByAuto = true
                    ports.beginAttempt()
                    startedAttempt = ports.gateActive()
                }
            } }
            is AutoConnectEffect.Message -> ports.publishError(effect.code)
            is AutoConnectEffect.Choose -> ports.dispatch { synchronized(this) { executeChoose(effect, capturedAttempt) } }
            is AutoConnectEffect.Select -> ports.dispatch { synchronized(this) { executeSelect(effect, capturedAttempt) } }
        }
    }

    private fun executeChoose(effect: AutoConnectEffect.Choose, capturedAttempt: String?) {
        if (!validEffect(effect.generation, effect.accountRef, capturedAttempt)) return
        if (controller.stage != AutoConnectController.Stage.CHOOSE_SENT || controller.target != effect.target) return
        val attempt = ports.gateActive() ?: return
        if (ports.dataIntentActive()) return
        if (ports.phase() != "CatalogReady" || ports.pendingNodeId() != null) return
        if (ports.nodes().none { it.id == effect.target }) { ports.publishError(AutoConnectCode.REMOVED); cancel(); return }
        if (startedAttempt != null && startedAttempt != attempt) return
        if (ports.deadlineEnd(attempt) == Long.MAX_VALUE) {
            if (!ports.startDeadline(attempt, ports.now())) return
        }
        choosingAttempt = attempt
        ports.sendChoose(effect.target)
        val remaining = (ports.deadlineEnd(attempt) - ports.now()).coerceAtLeast(0L)
        ports.armDeadlineTimeout(attempt, remaining) { onDeadlineTimeout(attempt, effect.generation) }
    }

    private fun executeSelect(effect: AutoConnectEffect.Select, capturedAttempt: String?) {
        if (!validEffect(effect.generation, effect.accountRef, capturedAttempt)) return
        val attempt = ports.gateActive() ?: return
        if (controller.stage != AutoConnectController.Stage.SELECT_SENT || controller.target != effect.target) return
        if (ports.dataIntentActive() && choosingAttempt != attempt) return
        if (!consentGranted()) { ports.publishError(AutoConnectCode.CONSENT); cancel(); return }
        if (ports.phase() != "CatalogReady" || ports.pendingNodeId() != null) return
        if (effect.target != ports.selectedNodeId()) return
        if (ports.deadlineEnd(attempt) == Long.MAX_VALUE) {
            if (!ports.startDeadline(attempt, ports.now())) return
        }
        if (ports.now() >= ports.deadlineEnd(attempt)) { ports.publishError(AutoConnectCode.TIMEOUT); cancel(); return }
        choosingAttempt = null // deadline now belongs to actual data setup, not catalog choice
        ports.sendSelect(effect.target, attempt)
        startedAttempt = attempt
    }

    /** Test seam: executes captured effects exactly like a queued production callback would. */
    internal fun applyForTest(effects: List<AutoConnectEffect>, generation: Long, capturedAttempt: String?) =
        apply(effects, generation, capturedAttempt)

    /** The captured planning context must still describe the live world. */
    private fun validEffect(generation: Long, accountRef: String?, capturedAttempt: String?): Boolean {
        if (generation != ports.activeGeneration()) return false
        if (!controller.isArmedFor(generation)) return false
        if (!ports.prefEnabled(generation)) { cancel(); return false }
        if (ports.stopping()) return false
        if (accountRef != null && accountRef != ports.accountRef()) return false
        if (!ports.entitlementUsable()) { ports.publishError(AutoConnectCode.RIGHTS); cancel(); return false }
        if (capturedAttempt != null && capturedAttempt != ports.gateActive()) return false
        return true
    }
}
