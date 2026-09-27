package xyz.terlimo.test;

import org.json.JSONObject;
import org.json.JSONTokener;

import java.nio.ByteBuffer;
import java.nio.charset.CodingErrorAction;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.Arrays;
import java.util.Base64;
import java.util.EnumMap;
import java.util.Map;

/** Pure read-only decision logic shared by the Android gate and local JVM tests. */
public final class ExistingIdentityReadOnlyGate {
    public static final String SIGNING_ALIAS = "terlimo.test.installation.p256.v1";
    public static final int MARKER_LIMIT = 4_096;
    public static final int BASELINE_LIMIT = 4_096;
    public static final int PRIVATE_STATE_LIMIT = 1_048_576;

    public enum ProtectedFile {
        MARKER("installation.marker", MARKER_LIMIT, 1),
        BASELINE("phone-test-baseline.json", BASELINE_LIMIT, 1),
        PRIVATE_STATE("private-state.aes", PRIVATE_STATE_LIMIT, 29);

        public final String fileName;
        public final int limit;
        private final int minimumSize;

        ProtectedFile(String fileName, int limit, int minimumSize) {
            this.fileName = fileName;
            this.limit = limit;
            this.minimumSize = minimumSize;
        }
    }

    public enum ReadState { PRESENT, MISSING, NOT_REGULAR, TOO_LARGE, UNREADABLE }

    public enum Failure {
        NONE,
        MARKER_MISSING,
        MARKER_INVALID,
        BASELINE_MISSING,
        BASELINE_INVALID,
        PRIVATE_STATE_MISSING,
        PRIVATE_STATE_INVALID,
        KEY_MISSING,
        CERTIFICATE_MISSING,
        KEY_MISMATCH,
        PROTECTED_FILES_CHANGED,
        READ_FAILED,
        KEY_CHECK_FAILED
    }

    public interface ProtectedFiles {
        ReadResult read(ProtectedFile file, int maximumBytes);
    }

    public interface KeyAccessFactory {
        KeyAccess open() throws Exception;
    }

    public interface KeyAccess {
        boolean containsSigningAlias() throws Exception;

        byte[] certificatePublicSpki() throws Exception;
    }

    public static final class ReadResult {
        public final ReadState state;
        public final byte[] bytes;

        private ReadResult(ReadState state, byte[] bytes) {
            this.state = state;
            this.bytes = bytes == null ? null : bytes.clone();
        }

        public static ReadResult present(byte[] bytes) {
            return new ReadResult(ReadState.PRESENT, bytes);
        }

        public static ReadResult state(ReadState state) {
            if (state == ReadState.PRESENT) throw new IllegalArgumentException("READ_STATE_INVALID");
            return new ReadResult(state, null);
        }
    }

    public static final class Result {
        public final boolean keyPresent;
        public final boolean baselineValid;
        public final boolean sameKey;
        public final boolean filesUnchanged;
        public final boolean checked;
        public final Failure failure;

        private Result(
                boolean keyPresent,
                boolean baselineValid,
                boolean sameKey,
                boolean filesUnchanged,
                boolean checked,
                Failure failure
        ) {
            this.keyPresent = keyPresent;
            this.baselineValid = baselineValid;
            this.sameKey = sameKey;
            this.filesUnchanged = filesUnchanged;
            this.checked = checked;
            this.failure = failure;
        }

        public boolean passed() {
            return keyPresent && baselineValid && sameKey && filesUnchanged && checked && failure == Failure.NONE;
        }
    }

    private static final class Fingerprint {
        private final ReadState state;
        private final int size;
        private final byte[] digest;

        Fingerprint(ReadResult read, ProtectedFile file) {
            if (read == null || read.state == null) {
                state = ReadState.UNREADABLE;
                size = -1;
                digest = null;
            } else if (read.state == ReadState.PRESENT && read.bytes != null
                    && read.bytes.length >= file.minimumSize && read.bytes.length <= file.limit) {
                state = ReadState.PRESENT;
                size = read.bytes.length;
                digest = sha256(read.bytes);
            } else if (read.state == ReadState.PRESENT) {
                state = read.bytes != null && read.bytes.length > file.limit
                        ? ReadState.TOO_LARGE : ReadState.UNREADABLE;
                size = read.bytes == null ? -1 : read.bytes.length;
                digest = null;
            } else {
                state = read.state;
                size = -1;
                digest = null;
            }
        }

        boolean sameBytes(Fingerprint other) {
            if (other == null || state != other.state) return false;
            if (state == ReadState.MISSING) return true;
            return state == ReadState.PRESENT && size == other.size
                    && MessageDigest.isEqual(digest, other.digest);
        }
    }

    private static final class Snapshot {
        private final Map<ProtectedFile, ReadResult> reads = new EnumMap<>(ProtectedFile.class);
        private final Map<ProtectedFile, Fingerprint> fingerprints = new EnumMap<>(ProtectedFile.class);

        static Snapshot capture(ProtectedFiles files) {
            Snapshot snapshot = new Snapshot();
            for (ProtectedFile file : ProtectedFile.values()) {
                ReadResult read;
                try {
                    read = files.read(file, file.limit);
                } catch (Throwable ignored) {
                    read = ReadResult.state(ReadState.UNREADABLE);
                }
                snapshot.reads.put(file, read);
                snapshot.fingerprints.put(file, new Fingerprint(read, file));
            }
            return snapshot;
        }

        boolean sameProtectedBytes(Snapshot other) {
            for (ProtectedFile file : ProtectedFile.values()) {
                if (!fingerprints.get(file).sameBytes(other.fingerprints.get(file))) return false;
            }
            return true;
        }
    }

    private ExistingIdentityReadOnlyGate() {}

    public static Result evaluate(ProtectedFiles files, KeyAccessFactory keyFactory) {
        Snapshot before = Snapshot.capture(files);
        boolean keyPresent = false;
        boolean baselineValid = false;
        boolean sameKey = false;
        boolean checked = false;
        Failure failure = protectedFilesFailure(before);

        byte[] baselineDigest = null;
        byte[] spki = null;
        Snapshot after;
        try {
            if (failure == Failure.NONE) {
                baselineDigest = parseBaselineDigest(before.reads.get(ProtectedFile.BASELINE).bytes);
                if (baselineDigest == null) {
                    failure = Failure.BASELINE_INVALID;
                } else {
                    baselineValid = true;
                    try {
                        KeyAccess keys = keyFactory.open();
                        keyPresent = keys != null && keys.containsSigningAlias();
                        if (!keyPresent) {
                            failure = Failure.KEY_MISSING;
                        } else {
                            spki = keys.certificatePublicSpki();
                            if (spki == null || spki.length == 0) {
                                failure = Failure.CERTIFICATE_MISSING;
                            } else {
                                byte[] actualDigest = sha256(spki);
                                try {
                                    checked = true;
                                    sameKey = MessageDigest.isEqual(baselineDigest, actualDigest);
                                    failure = sameKey ? Failure.NONE : Failure.KEY_MISMATCH;
                                } finally {
                                    Arrays.fill(actualDigest, (byte) 0);
                                }
                            }
                        }
                    } catch (Throwable ignored) {
                        failure = Failure.KEY_CHECK_FAILED;
                    }
                }
            }
        } finally {
            if (baselineDigest != null) Arrays.fill(baselineDigest, (byte) 0);
            if (spki != null) Arrays.fill(spki, (byte) 0);
            after = Snapshot.capture(files);
        }

        boolean filesUnchanged = before.sameProtectedBytes(after);
        if (!filesUnchanged) failure = Failure.PROTECTED_FILES_CHANGED;
        return new Result(keyPresent, baselineValid, sameKey, filesUnchanged, checked, failure);
    }

    private static Failure protectedFilesFailure(Snapshot snapshot) {
        ReadState marker = snapshot.fingerprints.get(ProtectedFile.MARKER).state;
        if (marker == ReadState.MISSING) return Failure.MARKER_MISSING;
        if (marker == ReadState.UNREADABLE) return Failure.READ_FAILED;
        if (marker != ReadState.PRESENT) return Failure.MARKER_INVALID;

        ReadState baseline = snapshot.fingerprints.get(ProtectedFile.BASELINE).state;
        if (baseline == ReadState.MISSING) return Failure.BASELINE_MISSING;
        if (baseline == ReadState.UNREADABLE) return Failure.READ_FAILED;
        if (baseline != ReadState.PRESENT) return Failure.BASELINE_INVALID;

        ReadState state = snapshot.fingerprints.get(ProtectedFile.PRIVATE_STATE).state;
        if (state == ReadState.MISSING) return Failure.PRIVATE_STATE_MISSING;
        if (state == ReadState.UNREADABLE) return Failure.READ_FAILED;
        if (state != ReadState.PRESENT) return Failure.PRIVATE_STATE_INVALID;
        return Failure.NONE;
    }

    private static byte[] parseBaselineDigest(byte[] bytes) {
        try {
            String json = StandardCharsets.UTF_8.newDecoder()
                    .onMalformedInput(CodingErrorAction.REPORT)
                    .onUnmappableCharacter(CodingErrorAction.REPORT)
                    .decode(ByteBuffer.wrap(bytes)).toString();
            JSONTokener tokens = new JSONTokener(json);
            Object root = tokens.nextValue();
            if (!(root instanceof JSONObject) || tokens.nextClean() != 0) return null;
            Object key = ((JSONObject) root).opt("key");
            if (!(key instanceof String)) return null;
            String encoded = (String) key;
            byte[] decoded = Base64.getDecoder().decode(encoded);
            if (decoded.length != 32 || !Base64.getEncoder().encodeToString(decoded).equals(encoded)) {
                Arrays.fill(decoded, (byte) 0);
                return null;
            }
            return decoded;
        } catch (Exception ignored) {
            return null;
        }
    }

    private static byte[] sha256(byte[] bytes) {
        try {
            return MessageDigest.getInstance("SHA-256").digest(bytes);
        } catch (NoSuchAlgorithmException impossible) {
            throw new IllegalStateException("SHA256_UNAVAILABLE");
        }
    }

}
