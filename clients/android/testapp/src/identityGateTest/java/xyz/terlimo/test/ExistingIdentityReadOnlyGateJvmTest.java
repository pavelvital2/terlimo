package xyz.terlimo.test;

import org.junit.Test;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.Base64;
import java.util.EnumMap;
import java.util.Map;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

public final class ExistingIdentityReadOnlyGateJvmTest {
    private static final byte[] SPKI = "fixed-public-spki-for-local-test".getBytes(StandardCharsets.UTF_8);

    @Test public void matchingCertificateDigestPasses() throws Exception {
        Fixture fixture = validFixture(SPKI);
        ExistingIdentityReadOnlyGate.Result result = fixture.evaluate();

        assertTrue(result.passed());
        assertEquals(2, fixture.keyReads);
        fixture.assertBoundedPreAndPostReads();
    }

    @Test public void mismatchedDigestRefusesWithoutRepair() throws Exception {
        Fixture fixture = validFixture("different-spki".getBytes(StandardCharsets.UTF_8));
        fixture.spki = SPKI.clone();

        ExistingIdentityReadOnlyGate.Result result = fixture.evaluate();

        assertEquals(ExistingIdentityReadOnlyGate.Failure.KEY_MISMATCH, result.failure);
        assertTrue(result.checked);
        assertFalse(result.sameKey);
        assertTrue(result.filesUnchanged);
        fixture.assertBoundedPreAndPostReads();
    }

    @Test public void missingAliasAndCertificateRefuse() throws Exception {
        Fixture missingAlias = validFixture(SPKI);
        missingAlias.aliasPresent = false;
        ExistingIdentityReadOnlyGate.Result aliasResult = missingAlias.evaluate();
        assertEquals(ExistingIdentityReadOnlyGate.Failure.KEY_MISSING, aliasResult.failure);
        assertEquals(1, missingAlias.keyReads);

        Fixture missingCertificate = validFixture(SPKI);
        missingCertificate.spki = null;
        ExistingIdentityReadOnlyGate.Result certificateResult = missingCertificate.evaluate();
        assertEquals(ExistingIdentityReadOnlyGate.Failure.CERTIFICATE_MISSING, certificateResult.failure);
        assertEquals(2, missingCertificate.keyReads);
    }

    @Test public void missingBaselineRefusesBeforeOpeningKeyStore() {
        Fixture fixture = validFixtureUnchecked(SPKI);
        fixture.files.put(ExistingIdentityReadOnlyGate.ProtectedFile.BASELINE,
                ExistingIdentityReadOnlyGate.ReadResult.state(ExistingIdentityReadOnlyGate.ReadState.MISSING));

        ExistingIdentityReadOnlyGate.Result result = fixture.evaluate();

        assertEquals(ExistingIdentityReadOnlyGate.Failure.BASELINE_MISSING, result.failure);
        assertEquals(0, fixture.factoryOpens);
        assertEquals(0, fixture.keyReads);
        fixture.assertBoundedPreAndPostReads();
    }

    @Test public void malformedAndNonCanonicalBaselinesRefuseBeforeKeyStore() throws Exception {
        String digest = Base64.getEncoder().encodeToString(MessageDigest.getInstance("SHA-256").digest(SPKI));
        String[] invalid = {
                "{\"key\":\"" + digest + "\"",
                "{\"key\":\"" + digest.replace("=", "") + "\"}",
                "{\"key\":\"AAAA\"}",
                "{\"key\":3}",
                "{\"key\":\"" + digest + "\"}{}"
        };
        for (String json : invalid) {
            Fixture fixture = validFixture(SPKI);
            fixture.baseline(json);
            ExistingIdentityReadOnlyGate.Result result = fixture.evaluate();
            assertEquals(ExistingIdentityReadOnlyGate.Failure.BASELINE_INVALID, result.failure);
            assertEquals(0, fixture.factoryOpens);
            assertFalse(result.checked);
            fixture.assertBoundedPreAndPostReads();
        }
    }

    @Test public void protectedMutationIsDetectedAndNeverRestored() throws Exception {
        Fixture fixture = validFixture(SPKI);
        fixture.mutateBeforePostRead = true;

        ExistingIdentityReadOnlyGate.Result result = fixture.evaluate();

        assertEquals(ExistingIdentityReadOnlyGate.Failure.PROTECTED_FILES_CHANGED, result.failure);
        assertFalse(result.filesUnchanged);
        assertEquals(9, fixture.current(ExistingIdentityReadOnlyGate.ProtectedFile.MARKER)[0]);
        fixture.assertBoundedPreAndPostReads();
    }

    @Test public void everyProtectedFileMissingOrOversizedRefusesBeforeKeyStore() {
        for (ExistingIdentityReadOnlyGate.ProtectedFile file : ExistingIdentityReadOnlyGate.ProtectedFile.values()) {
            Fixture missing = validFixtureUnchecked(SPKI);
            missing.files.put(file, ExistingIdentityReadOnlyGate.ReadResult.state(
                    ExistingIdentityReadOnlyGate.ReadState.MISSING));
            assertFalse(missing.evaluate().passed());
            assertEquals(0, missing.factoryOpens);
            missing.assertBoundedPreAndPostReads();

            Fixture oversized = validFixtureUnchecked(SPKI);
            oversized.files.put(file, ExistingIdentityReadOnlyGate.ReadResult.present(new byte[file.limit + 1]));
            assertFalse(oversized.evaluate().passed());
            assertEquals(0, oversized.factoryOpens);
            oversized.assertBoundedPreAndPostReads();
        }
    }

    @Test public void unreadableOrNonRegularFileCannotPass() {
        for (ExistingIdentityReadOnlyGate.ReadState state : new ExistingIdentityReadOnlyGate.ReadState[] {
                ExistingIdentityReadOnlyGate.ReadState.UNREADABLE,
                ExistingIdentityReadOnlyGate.ReadState.NOT_REGULAR}) {
            Fixture fixture = validFixtureUnchecked(SPKI);
            fixture.files.put(ExistingIdentityReadOnlyGate.ProtectedFile.BASELINE,
                    ExistingIdentityReadOnlyGate.ReadResult.state(state));
            assertFalse(fixture.evaluate().passed());
            assertEquals(0, fixture.factoryOpens);
            fixture.assertBoundedPreAndPostReads();
        }
    }

    @Test public void keyFailureStillPerformsBoundedPostChecks() throws Exception {
        Fixture fixture = validFixture(SPKI);
        fixture.throwOnContains = true;

        ExistingIdentityReadOnlyGate.Result result = fixture.evaluate();

        assertEquals(ExistingIdentityReadOnlyGate.Failure.KEY_CHECK_FAILED, result.failure);
        assertTrue(result.filesUnchanged);
        fixture.assertBoundedPreAndPostReads();
    }

    private static Fixture validFixture(byte[] baselineSpki) throws Exception {
        return validFixtureUnchecked(baselineSpki);
    }

    private static Fixture validFixtureUnchecked(byte[] baselineSpki) {
        try {
            byte[] digest = MessageDigest.getInstance("SHA-256").digest(baselineSpki);
            String encoded = Base64.getEncoder().encodeToString(digest);
            Fixture fixture = new Fixture();
            fixture.files.put(ExistingIdentityReadOnlyGate.ProtectedFile.MARKER,
                    ExistingIdentityReadOnlyGate.ReadResult.present(new byte[] {1}));
            fixture.baseline("{\"key\":\"" + encoded + "\",\"request\":\"x\",\"payload\":\"y\",\"state\":\"z\"}");
            fixture.files.put(ExistingIdentityReadOnlyGate.ProtectedFile.PRIVATE_STATE,
                    ExistingIdentityReadOnlyGate.ReadResult.present(new byte[29]));
            fixture.spki = baselineSpki.clone();
            return fixture;
        } catch (Exception impossible) {
            throw new AssertionError(impossible);
        }
    }

    private static final class Fixture implements ExistingIdentityReadOnlyGate.ProtectedFiles,
            ExistingIdentityReadOnlyGate.KeyAccessFactory, ExistingIdentityReadOnlyGate.KeyAccess {
        final Map<ExistingIdentityReadOnlyGate.ProtectedFile, ExistingIdentityReadOnlyGate.ReadResult> files =
                new EnumMap<>(ExistingIdentityReadOnlyGate.ProtectedFile.class);
        final Map<ExistingIdentityReadOnlyGate.ProtectedFile, Integer> reads =
                new EnumMap<>(ExistingIdentityReadOnlyGate.ProtectedFile.class);
        final Map<ExistingIdentityReadOnlyGate.ProtectedFile, Integer> observedLimits =
                new EnumMap<>(ExistingIdentityReadOnlyGate.ProtectedFile.class);
        boolean aliasPresent = true;
        byte[] spki;
        int factoryOpens;
        int keyReads;
        int totalReads;
        boolean mutateBeforePostRead;
        boolean throwOnContains;

        void baseline(String json) {
            files.put(ExistingIdentityReadOnlyGate.ProtectedFile.BASELINE,
                    ExistingIdentityReadOnlyGate.ReadResult.present(json.getBytes(StandardCharsets.UTF_8)));
        }

        byte[] current(ExistingIdentityReadOnlyGate.ProtectedFile file) {
            return files.get(file).bytes;
        }

        ExistingIdentityReadOnlyGate.Result evaluate() {
            return ExistingIdentityReadOnlyGate.evaluate(this, this);
        }

        @Override public ExistingIdentityReadOnlyGate.ReadResult read(
                ExistingIdentityReadOnlyGate.ProtectedFile file, int maximumBytes) {
            totalReads++;
            reads.put(file, reads.getOrDefault(file, 0) + 1);
            observedLimits.put(file, maximumBytes);
            if (mutateBeforePostRead && totalReads == 4) {
                files.put(ExistingIdentityReadOnlyGate.ProtectedFile.MARKER,
                        ExistingIdentityReadOnlyGate.ReadResult.present(new byte[] {9}));
            }
            return files.get(file);
        }

        @Override public ExistingIdentityReadOnlyGate.KeyAccess open() {
            factoryOpens++;
            return this;
        }

        @Override public boolean containsSigningAlias() {
            keyReads++;
            if (throwOnContains) throw new IllegalStateException("must not escape");
            return aliasPresent;
        }

        @Override public byte[] certificatePublicSpki() {
            keyReads++;
            return spki == null ? null : spki.clone();
        }

        void assertBoundedPreAndPostReads() {
            for (ExistingIdentityReadOnlyGate.ProtectedFile file : ExistingIdentityReadOnlyGate.ProtectedFile.values()) {
                assertEquals(Integer.valueOf(2), reads.get(file));
                assertEquals(Integer.valueOf(file.limit), observedLimits.get(file));
            }
        }
    }
}
