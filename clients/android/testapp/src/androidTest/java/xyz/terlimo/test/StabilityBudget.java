package xyz.terlimo.test;

/** Test-only elapsed-realtime budget. Never extends a window or shortens the hold. */
public final class StabilityBudget {
    public static final long HOLD_MS = 1_200_000L;
    public static final long CLEANUP_MS = 15_000L;
    public static final long MAX_OVERALL_MS = 1_380_000L;
    public static final long MAX_SETUP_MS = 120_000L;

    private final long start;
    private final long setupDeadline;
    private final long operationDeadline;
    private long holdEnd = -1L;

    public StabilityBudget(long startElapsed, long absoluteDeadlineElapsed) {
        if (startElapsed < 0 || absoluteDeadlineElapsed <= startElapsed
                || absoluteDeadlineElapsed - startElapsed > MAX_OVERALL_MS
                || absoluteDeadlineElapsed - startElapsed < HOLD_MS + CLEANUP_MS) {
            throw new IllegalArgumentException("Window must fit full hold and cleanup, at most 23 minutes");
        }
        start = startElapsed;
        operationDeadline = absoluteDeadlineElapsed - CLEANUP_MS;
        // All additions are bounded by the validated absolute deadline.
        setupDeadline = startElapsed + Math.min(MAX_SETUP_MS,
                operationDeadline - HOLD_MS - startElapsed);
    }

    /** Records the first actual Connected only. A second call cannot restart the timer. */
    public boolean connected(long now) {
        if (holdEnd >= 0 || now < start || now > setupDeadline) return false;
        holdEnd = now + HOLD_MS;
        return true;
    }

    public long setupDeadline() { return setupDeadline; }
    public long operationDeadline() { return operationDeadline; }
    public long holdEnd() { return holdEnd; }

    public boolean holdingDone(long now) {
        return holdEnd >= 0 && now >= holdEnd && now <= operationDeadline;
    }

    /** active=false means cancellation/terminal state, including at the completion boundary. */
    public String decision(long now, boolean active) {
        if (!active) return "CANCELED";
        if (now < start || now > operationDeadline) return "DEADLINE";
        if (holdingDone(now)) return "COMPLETE";
        if (holdEnd < 0 && now > setupDeadline) return "DEADLINE";
        return "WAIT";
    }
}
