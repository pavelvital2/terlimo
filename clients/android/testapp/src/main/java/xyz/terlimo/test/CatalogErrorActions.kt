package xyz.terlimo.test

/**
 * §29.3: which real catalogue-error surfaces get the explicit retry + support actions.
 *
 * The displayed error is what matters:
 * - Browse offline: [BrowseErrorPolicy] mirrors TRANSPORT / SERVICE_UNAVAILABLE / RATE_LIMITED
 *   while the catalogue is still being fetched; the browse card shows the action card.
 * - Credential catalogue Error: only a genuine catalogue-update failure. A Connected server
 *   switch failure (or a success/null state) must not be labelled as an update error.
 */
internal object CatalogErrorActions {
    private val CATALOG_UPDATE_CODES = setOf(
        "CATALOG_CONNECTING_TIMEOUT", "CATALOG_DEVICE_TIMEOUT", "CATALOG_SUBSCRIPTION_TIMEOUT", "CATALOG_LIST_TIMEOUT",
        "CATALOG_TIMEOUT", "CATALOG_EXPIRED", "CATALOG_INVALID", "BAD_CATALOG",
        "TRANSPORT", "TRANSPORT_TIMEOUT", "TRANSPORT_FAILED", "EOF",
        "NETWORK_UNAVAILABLE", "PHYSICAL_NETWORK_UNAVAILABLE",
        "SERVICE_UNAVAILABLE", "RATE_LIMITED", "BACKEND_UNAVAILABLE",
    )

    /** Credential/retained catalogue Error card: genuine update failure only. */
    fun credentialRetry(phase: String, error: String?): Boolean =
        phase == "Error" && error in CATALOG_UPDATE_CODES

    /** Browse card: its own displayed error (offline mirror or onboarding conflict). */
    fun browseRetry(browseError: String?): Boolean = browseError != null
}
