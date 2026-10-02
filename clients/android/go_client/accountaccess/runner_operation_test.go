package accountaccess

import (
	"context"
	"testing"
	"time"
)

// A replacement queued at operation completion must survive the previous
// manualQueued fence. The real Runner loop is used; no parallel worker is added.
func TestRunnerOperationFinishCanScheduleNextManual(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var sequence []bool
	runner.config.BeginAttempt = func(parent context.Context, manual bool) (context.Context, func()) {
		sequence = append(sequence, manual)
		runCtx, stop := context.WithCancel(parent)
		stop() // cancel operation, not persistent runner
		return runCtx, func() {
			if len(sequence) < 3 {
				runner.TriggerManual()
			} else {
				cancel()
			}
		}
	}
	_ = runner.Run(ctx)
	if len(sequence) != 3 || sequence[0] || !sequence[1] || !sequence[2] {
		t.Fatalf("replacement lost behind manual fence: %v", sequence)
	}
}
