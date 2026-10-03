package xyz.terlimo.test

internal data class ReferralClientState(
    val durable: ReferralState? = null,
    val info: ReferralInfo? = null,
    val loading: Boolean = false,
    val error: String? = null,
    val storageUnavailable: Boolean = false,
    // Display-only subject from the full receipt's correlated fresh /me, never a data grant.
    val verifiedRegistrationAccount: String? = null,
)

internal object ReferralAccount {
    /** Verified account identity, independent of purchase binding, slots or expired access. */
    fun registered(state: ViewState): Boolean = state.accountAccess?.projection?.account?.let {
        it.telegramLinked && !it.accountRef.isNullOrBlank()
    } == true
}

/** No inferred grants, discount arithmetic or terminal receipt from a GET projection. */
internal object ReferralClientPresentation {
    fun model(state: ViewState): ReferralRenderModel {
        val client = state.referral
        val durable = client.durable
        val registered = ReferralAccount.registered(state)
        val account = state.accountAccess?.projection?.account?.accountRef
        val info = client.info?.takeIf { registered && it.accountRef == account }
        val op = durable?.operation
        val receipt = durable?.receipt?.takeIf {
            (registered && it.accountRef == account) ||
                (it.accountRef == client.verifiedRegistrationAccount && (account == null || account == it.accountRef))
        }
        val status = when {
            client.storageUnavailable -> "Сохранённую попытку не удалось прочитать. Она не заменена новой."
            durable?.registration?.expired == true -> "Срок исходной регистрации истёк. Код сохранён; новая регистрация начнётся только по вашему действию."
            client.error != null -> ReferralContract.errorText(client.error)
            durable?.rejection != null -> ReferralContract.errorText(durable.rejection.code)
            durable?.receipt != null && receipt == null -> "Сохранён результат приглашения другого аккаунта. Для текущего аккаунта он не применяется."
            durable?.locked == true -> "Ждём подтверждения исходной регистрации. Открытие Telegram не подтверждает приглашение."
            op != null -> "Исходная операция сохранена. Повтор использует тот же код и ключ; результат ещё не подтверждён."
            durable?.candidate != null -> "Код сохранён на сервере для этой установки. Привязка к аккаунту ещё не подтверждена."
            else -> null
        }
        return ReferralRenderModel(
            telegramRegistered = registered,
            draft = durable?.draftCode.orEmpty(), draftRevision = durable?.revision ?: 0L,
            candidateCode = op?.code ?: durable?.candidate?.code,
            candidateState = when {
                client.storageUnavailable -> ReferralCandidateUiState.UNAVAILABLE
                durable?.locked == true -> ReferralCandidateUiState.LOCKED
                op != null && client.loading -> if (op.kind == ReferralOperationKind.DELETE) ReferralCandidateUiState.CLEARING else ReferralCandidateUiState.SUBMITTING
                op != null -> ReferralCandidateUiState.UNKNOWN
                durable?.rejection != null -> ReferralCandidateUiState.REJECTED
                receipt?.state == "attached" -> ReferralCandidateUiState.CONFIRMED
                receipt?.state == "rejected" -> ReferralCandidateUiState.REJECTED
                durable?.receipt != null && receipt == null -> ReferralCandidateUiState.UNAVAILABLE
                durable?.candidate != null -> ReferralCandidateUiState.PENDING
                !durable?.draftCode.isNullOrEmpty() -> ReferralCandidateUiState.DRAFT
                else -> ReferralCandidateUiState.NONE
            },
            statusMessage = status,
            canEdit = !client.storageUnavailable && durable?.unresolved != true && !client.loading,
            canSubmit = !client.storageUnavailable && durable?.unresolved != true && !client.loading &&
                ReferralContract.validCode(durable?.draftCode.orEmpty()),
            canClear = !client.storageUnavailable && durable?.unresolved != true && !client.loading &&
                (durable?.candidate != null || !durable?.draftCode.isNullOrEmpty()),
            canRetry = !client.storageUnavailable && !client.loading &&
                (op != null || (durable?.locked == true && !durable.legacyLocked) || durable?.registration?.expired == true),
            registrationExpired = durable?.registration?.expired == true,
            infoState = when {
                !registered -> ReferralInfoUiState.UNREGISTERED
                info != null -> ReferralInfoUiState.READY
                client.loading -> ReferralInfoUiState.LOADING
                client.error == "REFERRAL_HISTORY_PENDING" -> ReferralInfoUiState.HISTORY_PENDING
                else -> ReferralInfoUiState.UNAVAILABLE
            },
            ownCode = info?.code, telegramLink = info?.telegramLink, webLink = info?.webLink,
            appliedDays = info?.appliedDays, waitingDays = info?.waitingDays,
            attributionMessage = info?.attribution?.let {
                when (it.state) {
                    "attached" -> "Сервер подтверждает приглашение для текущего аккаунта."
                    "rejected" -> "Приглашение не применено: " + when (it.reason) {
                        "self" -> "нельзя пригласить себя."
                        "already_attributed" -> "ранее уже применено другое приглашение."
                        "ineligible" -> "условия приглашения не выполнены."
                        else -> "код не принят."
                    }
                    else -> "У аккаунта пока нет подтверждённого приглашения."
                }
            },
            benefitsMessage = info?.discount?.let {
                when (it.state) {
                    "eligible" -> "Сервер подтверждает право на скидку; итоговую цену покажет предложение оплаты."
                    "reserved" -> "Скидка закреплена за текущим счётом."
                    "consumed" -> "Скидка уже использована."
                    "history_pending" -> "История скидки ещё проверяется."
                    else -> "Скидка сейчас недоступна."
                }
            }, termsVersion = info?.termsVersion,
        )
    }
}
