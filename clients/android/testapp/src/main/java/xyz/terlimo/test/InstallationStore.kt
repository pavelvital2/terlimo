package xyz.terlimo.test

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.AtomicFile
import org.json.JSONObject
import java.io.File
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.MessageDigest
import java.security.PrivateKey
import java.security.Signature
import java.security.spec.ECGenParameterSpec
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/** One key per installation, outside subscription state. Never replaced after loss. */
internal class InstallationStore(context: Context) {
    init { withUnlockedStorage(context.getSystemService(android.os.UserManager::class.java).isUserUnlocked) { Unit } }

    private val store = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
    private val marker = AtomicFile(File(context.noBackupFilesDir, "installation.marker"))
    private val stateFile = AtomicFile(File(context.noBackupFilesDir, "private-state.aes"))
    private val signingAlias = "terlimo.test.installation.p256.v1"
    private val encryptionAlias = "terlimo.test.storage.aes.v1"

    fun ensureIdentity() = synchronized(LOCK) {
        if (marker.baseFile.exists() || stateFile.baseFile.exists()) {
            check(store.containsAlias(signingAlias) && store.containsAlias(encryptionAlias)) { "KEY_UNAVAILABLE" }
            return@synchronized
        }
        if (!store.containsAlias(signingAlias)) {
            KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore").apply {
                initialize(KeyGenParameterSpec.Builder(signingAlias, KeyProperties.PURPOSE_SIGN or KeyProperties.PURPOSE_VERIFY)
                    .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
                    .setDigests(KeyProperties.DIGEST_SHA256).setUserAuthenticationRequired(false).build())
            }.generateKeyPair()
        }
        if (!store.containsAlias(encryptionAlias)) {
            KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore").apply {
                init(KeyGenParameterSpec.Builder(encryptionAlias, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                    .setKeySize(256).setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                    .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE).build())
            }.generateKey()
        }
        atomicWrite(marker, byteArrayOf(1))
    }
    fun publicSpki(): ByteArray {
        ensureIdentity()
        return store.getCertificate(signingAlias)?.publicKey?.encoded ?: error("KEY_UNAVAILABLE")
    }
    fun installationId(): String = MessageDigest.getInstance("SHA-256").digest(publicSpki()).joinToString("") { "%02x".format(it) }
    fun sign(kind: String, transcript: ByteArray): ByteArray {
        SigningPolicy.validate(kind, transcript)
        val key = store.getKey(signingAlias, null) as? PrivateKey ?: error("KEY_UNAVAILABLE")
        check(key.encoded == null) { "KEY_EXPORTABLE" }
        return Signature.getInstance("SHA256withECDSA").run {
            initSign(key); update(transcript); sign()
        }
    }
    fun read(): JSONObject = synchronized(LOCK) { readLocked() }
    fun write(state: JSONObject) = synchronized(LOCK) { writeLocked(state) }

    /** Keeps the installation identity and user routing settings, but removes all subscription-bound state. */
    fun replaceSubscription() = synchronized(LOCK) {
        val current = readLocked()
        check(current.optString("link").isNotEmpty()) { "IMPORT_REQUIRED" }
        write(SubscriptionStateReplacement.withoutSubscription(current))
    }
    fun readRoutingSettings(
        protectedPackages: Set<String>,
        presets: QuickExclusionCatalog,
    ): RoutingSettingsDocument? = synchronized(LOCK) {
        val raw = readLocked().opt("routing_settings") ?: return@synchronized null
        require(raw is String) { "SETTINGS_INVALID" }
        return@synchronized RoutingSettingsCodec.decode(raw, protectedPackages, presets)
    }

    /** Validates first; AtomicFile preserves the previous complete private state on failure. */
    fun compareAndSetRoutingSettings(
        expectedRevision: Long?,
        next: RoutingSettingsDocument,
        protectedPackages: Set<String>,
        presets: QuickExclusionCatalog,
    ): Boolean = synchronized(LOCK) {
        val encoded = RoutingSettingsCodec.encode(next)
        val state = readLocked()
        val existing = state.opt("routing_settings")?.let {
            require(it is String) { "SETTINGS_INVALID" }
            RoutingSettingsCodec.decode(it, protectedPackages, presets)
        }
        if (existing?.revision != expectedRevision) return@synchronized false
        writeLocked(JSONObject(state.toString()).put("routing_settings", encoded))
        return@synchronized true
    }

    /**
     * Native subscription persist. Replaces only the subscription-bound fields and
     * keeps the user routing settings, so a native/refresh persist cannot erase the
     * policy that a concurrent host edit or an earlier session wrote.
     */
    fun writeSubscriptionState(link: String, stateB64: String) = synchronized(LOCK) {
        writeLocked(SubscriptionStateReplacement.withSubscriptionState(readLocked(), link, stateB64))
    }

    /**
     * Native import acknowledgement: retain every field, refresh only the active link atomically.
     * A linkless mobile bootstrap has no legacy link to persist: an empty write is a no-op so it
     * can never erase a saved link or synthesize a pseudo-link.
     */
    fun writeActiveLink(link: String) = synchronized(LOCK) {
        if (link.isEmpty()) return@synchronized
        val current = readLocked()
        if (current.optString("link") != link) writeLocked(SubscriptionStateReplacement.withLink(current, link))
    }

    /**
     * Durable native state blob per versioned namespace; never wlbs state/link/catalog.
     * accountaccess_receipt_v1 keeps the receipt field, service_seed_v1 keeps the
     * service seed store (last-good cached seed + user hash override), and
     * onboarding_flow_v1 keeps the onboarding attempt identity (request_key/intent
     * metadata only; a bootstrap secret is never stored here).
     */
    fun readAccountAccessState(namespace: String): String? = synchronized(LOCK) {
        when (namespace) {
            "accountaccess_receipt_v1" -> readLocked().opt("accountaccess_state") as? String
            "service_seed_v1" -> readLocked().opt("service_seed_state") as? String
            "onboarding_flow_v1" -> readLocked().opt("onboarding_flow_state") as? String
            else -> null
        }
    }

    fun writeAccountAccessState(namespace: String, stateB64: String) = synchronized(LOCK) {
        require(namespace in NATIVE_STATE_NAMESPACES) { "NAMESPACE_INVALID" }
        require(SigningPolicy.decode(stateB64).size <= 64_000) { "STORAGE_TOO_LARGE" }
        val field = when (namespace) {
            "accountaccess_receipt_v1" -> "accountaccess_state"
            "service_seed_v1" -> "service_seed_state"
            else -> "onboarding_flow_state"
        }
        writeLocked(JSONObject(readLocked().toString()).put(field, stateB64))
    }

    /**
     * Host-owned purchase attempt memory (S5 idempotency keys). It lives in the same
     * encrypted AtomicFile state as every other host secret and is written before the
     * first send of each payment operation, so a retry/restart reuses the same keys.
     * It is never an entitlement, a grant or a payment truth.
     */
    fun readPurchaseAttempt(): String? = synchronized(LOCK) {
        readLocked().opt("purchase_attempt_state") as? String
    }

    fun writePurchaseAttempt(encoded: String) = synchronized(LOCK) {
        require(encoded.toByteArray(Charsets.UTF_8).size in 2..900_000) { "PURCHASE_STATE_INVALID" }
        writeLocked(JSONObject(readLocked().toString()).put("purchase_attempt_state", encoded))
    }

    fun resolvePurchaseNoOrder(expected: String, resolved: String, installation: String) = synchronized(LOCK) {
        check(installationId() == installation) { "PURCHASE_INSTALLATION_MISMATCH" }
        val next = PurchaseNoOrderResolution.prepare(readLocked(), expected, resolved, installation)
        writeLocked(next)
    }

    /** Host-only referral namespace; keeps identity, recovery and payment records intact. */
    fun readReferralState(): ReferralState? = synchronized(LOCK) {
        val raw = readLocked().opt("referral_client_v1") ?: return@synchronized null
        check(raw is String) { "REFERRAL_STATE_INVALID" }
        ReferralStateCodec.decode(raw, installationId())
    }

    fun compareAndSetReferral(expectedRevision: Long?, next: ReferralState): Boolean = synchronized(LOCK) {
        check(next.installationId == installationId()) { "REFERRAL_STATE_INVALID" }
        val current = readLocked()
        val raw = current.opt("referral_client_v1")
        val previous = if (raw == null) null else {
            check(raw is String) { "REFERRAL_STATE_INVALID" }
            ReferralStateCodec.decode(raw, installationId())
        }
        if (previous?.revision != expectedRevision) return@synchronized false
        check(next.revision == (previous?.revision ?: 0L) + 1L) { "REFERRAL_STATE_INVALID" }
        writeLocked(JSONObject(current.toString()).put("referral_client_v1", ReferralStateCodec.encode(next)))
        true
    }

    /** Display-only memory of the last verified catalog. Never an access grant. */
    fun readCatalogCache(): String? = synchronized(LOCK) { readLocked().opt("catalog_cache") as? String }

    /**
     * §26.2 last server that actually reached a confirmed Connected, scoped to the account it
     * was confirmed under. A different/absent account makes it unusable (no cross-identity move).
     */
    fun readLastConnectedNode(accountRef: String?): String? = synchronized(LOCK) {
        val block = readLocked().optJSONObject("last_connect") ?: return@synchronized null
        val stored = block.optString("account_ref").takeIf { it.isNotEmpty() } ?: return@synchronized null
        if (accountRef == null || stored != accountRef) return@synchronized null
        block.optString("node_id").takeIf { it.isNotEmpty() }
    }

    /** Import/deeplink identity change: the previous identity's last server must not survive. */
    fun clearLastConnectedNode() = synchronized(LOCK) {
        val current = readLocked()
        if (current.has("last_connect")) {
            val next = JSONObject(current.toString())
            next.remove("last_connect")
            writeLocked(next)
        }
    }

    /** Written only on a confirmed Connected, together with the account it belongs to. */
    fun writeLastConnectedNode(nodeId: String, accountRef: String?) {
        require(nodeId.isNotEmpty() && nodeId.length <= 128)
        synchronized(LOCK) {
            val block = JSONObject()
                .put("node_id", nodeId)
                .put("account_ref", accountRef ?: "")
            writeLocked(JSONObject(readLocked().toString()).put("last_connect", block))
        }
    }

    fun writeCatalogCache(encoded: String, mobileSelection: JSONObject? = null) = synchronized(LOCK) {
        require(encoded.toByteArray(Charsets.UTF_8).size in 1..65_536) { "CACHE_OVERSIZED" }
        writeLocked(MobileSelectionPreference.merge(readLocked(), mobileSelection).put("catalog_cache", encoded))
    }

    fun reconcileMobileSelection(state: ViewState, source: MobileBootstrapSeed) = synchronized(LOCK) {
        writeLocked(MobileSelectionPreference.reconcile(readLocked(), state, installationId(), source))
    }

    private fun encryptionKey() = store.getKey(encryptionAlias, null) as? SecretKey ?: error("KEY_UNAVAILABLE")
    private fun readLocked(): JSONObject {
        ensureIdentity()
        if (!stateFile.baseFile.exists()) return JSONObject()
        val bytes = stateFile.readFully()
        check(bytes.size in 29..1_048_576 && bytes[0] == 1.toByte()) { "STORAGE_INVALID" }
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(Cipher.DECRYPT_MODE, encryptionKey(), GCMParameterSpec(128, bytes.copyOfRange(1, 13)))
        cipher.updateAAD("TERLIMO-TEST-STATE-1".toByteArray())
        val plaintext = cipher.doFinal(bytes, 13, bytes.size - 13)
        return try { JSONObject(plaintext.toString(Charsets.UTF_8)) } finally { plaintext.fill(0) }
    }
    private fun writeLocked(state: JSONObject) {
        ensureIdentity()
        val plaintext = state.toString().toByteArray()
        require(plaintext.size <= 900_000) { "STORAGE_TOO_LARGE" }
        try {
            val cipher = Cipher.getInstance("AES/GCM/NoPadding")
            cipher.init(Cipher.ENCRYPT_MODE, encryptionKey())
            cipher.updateAAD("TERLIMO-TEST-STATE-1".toByteArray())
            atomicWrite(stateFile, byteArrayOf(1) + cipher.iv + cipher.doFinal(plaintext))
        } finally { plaintext.fill(0) }
    }
    private fun atomicWrite(file: AtomicFile, bytes: ByteArray) {
        val stream = file.startWrite()
        try { stream.write(bytes); file.finishWrite(stream) }
        catch (error: Exception) { file.failWrite(stream); throw error }
    }

    private companion object {
        private val NATIVE_STATE_NAMESPACES = setOf("accountaccess_receipt_v1", "service_seed_v1", "onboarding_flow_v1")
        // One process-wide owner for the encrypted private state. Every InstallationStore
        // instance (SessionService persist, RoutingSettingsActivity CAS, MainActivity import)
        // serializes on the same monitor, so concurrent native/host writes cannot interleave
        // a read-modify-write and drop routing_settings.
        private val LOCK = Any()
    }
}

/** Encrypted-store adapter for the pure S5 purchase idempotency policy. */
internal class InstallationPurchaseAttemptStore(private val storage: InstallationStore) : PurchaseAttemptStore {
    override fun read(): String? = storage.readPurchaseAttempt()
    override fun write(encoded: String) = storage.writePurchaseAttempt(encoded)
    override fun resolveNoOrder(expected: String, resolved: String, installationId: String) =
        storage.resolvePurchaseNoOrder(expected, resolved, installationId)
}

internal object SubscriptionStateReplacement {
    fun withoutSubscription(current: JSONObject): JSONObject = JSONObject().also { retained ->
        if (current.has("routing_settings")) retained.put("routing_settings", current.get("routing_settings"))
        // §26.2: last_connect is identity/account bound; a subscription replacement removes it.
    }

    /** Field-preserving merge for native subscription persist; never drops routing_settings. */
    fun withSubscriptionState(current: JSONObject, link: String, stateB64: String): JSONObject =
        JSONObject(current.toString()).put("link", link).put("state_b64", stateB64)

    fun withLink(current: JSONObject, link: String): JSONObject =
        JSONObject(current.toString()).put("link", link)
}

/** Guard runs before storage construction/read; callers cannot interpret locked CE as empty. */
internal inline fun <T> withUnlockedStorage(unlocked: Boolean, access: () -> T): T {
    check(unlocked) { "USER_UNLOCK_REQUIRED" }
    return access()
}

internal class InstallationReferralStateStore(private val storage: InstallationStore) : ReferralStateStorage {
    override fun read(): ReferralState? = storage.readReferralState()
    override fun compareAndSet(expectedRevision: Long?, next: ReferralState): Boolean =
        storage.compareAndSetReferral(expectedRevision, next)
}
