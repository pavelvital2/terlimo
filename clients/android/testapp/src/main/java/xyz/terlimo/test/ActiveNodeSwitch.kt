package xyz.terlimo.test

/** Why an explicit active-tunnel replacement may or may not start; never chooses a node. */
internal enum class SwitchAdmission { ALLOWED, NOT_CONNECTED, PENDING, NO_REVISION, SAME_TARGET, UNKNOWN_TARGET }

/** Pure gates for an explicit active-tunnel replacement; never chooses a node. */
internal object ActiveNodeSwitch {
    fun admission(state: ViewState, targetId: String): SwitchAdmission = when {
        state.phase != "Connected" -> SwitchAdmission.NOT_CONNECTED
        state.pendingNodeId != null || state.pendingSwitchId != null -> SwitchAdmission.PENDING
        state.catalogRevision.isEmpty() -> SwitchAdmission.NO_REVISION
        targetId == state.selectedNodeId -> SwitchAdmission.SAME_TARGET
        state.nodes.none { it.id == targetId } -> SwitchAdmission.UNKNOWN_TARGET
        else -> SwitchAdmission.ALLOWED
    }

    fun canStart(state: ViewState, targetId: String): Boolean =
        admission(state, targetId) == SwitchAdmission.ALLOWED

    fun awaitingConfig(state: ViewState, activeNodeId: String, targetId: String,
        switchId: String, revision: String): Boolean =
        state.phase == "Connected" && state.pendingNodeId == targetId &&
            state.pendingSwitchId == switchId && state.pendingSwitchRevision == revision &&
            targetId != state.selectedNodeId && activeNodeId == state.selectedNodeId

    fun success(state: ViewState, activeNodeId: String, targetId: String,
        switchId: String, revision: String): ViewState {
        require(state.phase == "SwitchingServer" && state.pendingNodeId == targetId && activeNodeId == targetId &&
            state.pendingSwitchId == switchId && state.pendingSwitchRevision == revision) {
            "STALE_SWITCH_COMPLETION"
        }
        return state.copy(phase = "Connected", selectedNodeId = targetId, pendingNodeId = null,
            pendingSwitchId = null, pendingSwitchRevision = null, error = null)
    }

    fun failure(state: ViewState, lastGoodNodeId: String, targetId: String,
        switchId: String, revision: String, code: String): ViewState {
        require(state.phase == "SwitchingServer" && state.pendingNodeId == targetId && lastGoodNodeId.isNotEmpty() &&
            state.pendingSwitchId == switchId && state.pendingSwitchRevision == revision) {
            "STALE_SWITCH_COMPLETION"
        }
        return state.copy(phase = "Connected", selectedNodeId = lastGoodNodeId, pendingNodeId = null,
            pendingSwitchId = null, pendingSwitchRevision = null, error = code)
    }
}
