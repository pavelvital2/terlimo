package xyz.terlimo.test

/** Service-owned state. Rendering never starts or completes a referral operation. */
internal data class ReferralRenderModel(
    val telegramRegistered: Boolean = false,
    val draft: String = "",
    /** Increment only for explicit replacement, e.g. confirmed clear or restored draft. */
    val draftRevision: Long = 0,
    val candidateState: ReferralCandidateUiState = ReferralCandidateUiState.NONE,
    /** Original operation's code, distinct from the edited draft. */
    val candidateCode: String? = null,
    val statusMessage: String? = null,
    val canEdit: Boolean = true,
    val canSubmit: Boolean = false,
    val canClear: Boolean = false,
    val canRetry: Boolean = false,
    val registrationExpired: Boolean = false,
    val infoState: ReferralInfoUiState = ReferralInfoUiState.UNREGISTERED,
    /** Pass only values checked against the current verified account. */
    val ownCode: String? = null,
    val telegramLink: String? = null,
    val webLink: String? = null,
    val appliedDays: Long? = null,
    val waitingDays: Long? = null,
    val attributionMessage: String? = null,
    val benefitsMessage: String? = null,
    val termsVersion: String? = null,
)

internal enum class ReferralCandidateUiState {
    NONE, DRAFT, SUBMITTING, UNKNOWN, PENDING, CLEARING, LOCKED,
    CONFIRMED, REJECTED, HISTORY_PENDING, UNAVAILABLE,
}

internal enum class ReferralInfoUiState {
    UNREGISTERED, LOADING, READY, HISTORY_PENDING, UNAVAILABLE,
}
