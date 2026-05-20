package controller

import (
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/pod-nsg-controller/internal/azure"
)

// StatusWriteOutcome classifies the result of a status write operation.
type StatusWriteOutcome string

const (
	// StatusWriteOutcomeWritten indicates the status was successfully written.
	StatusWriteOutcomeWritten StatusWriteOutcome = "written"
	// StatusWriteOutcomeNoop indicates the status was semantically unchanged (no write needed).
	StatusWriteOutcomeNoop StatusWriteOutcome = "noop"
	// StatusWriteOutcomeError indicates the status write failed after retries.
	StatusWriteOutcomeError StatusWriteOutcome = "error"
)

// ConvergenceCommitter is a callback invoked by the status updater after final
// status resolution to commit convergence metrics. The outcome and statusErr
// parameters allow the implementation to decide whether to commit based on
// write success, semantic no-op, or error conditions.
//
// Idempotency requirement: implementations MUST be safe to call multiple times
// with the same (key, observedGeneration, results) arguments. The current
// retry loop calls the committer exactly once per UpdateAfterReconcile
// invocation (conflict retries skip notification), but this contract ensures
// correctness if the call topology changes. The default implementation
// (ConvergenceTracker.CommitConvergence) satisfies this by deleting the
// pending token on the first call, making subsequent calls no-ops.
type ConvergenceCommitter func(
	key types.NamespacedName,
	observedGeneration int64,
	results []azure.ActionResult,
	outcome StatusWriteOutcome,
	statusErr error,
)

// SetConvergenceCommitter sets the convergence committer callback on the status updater.
// The committer is invoked after UpdateAfterReconcile completes: on successful status
// write (outcome=written), semantic no-op (outcome=noop), or write error (outcome=error).
// Not-found and stale-generation guards skip notification entirely.
func (u *MappingStatusUpdater) SetConvergenceCommitter(committer ConvergenceCommitter) {
	u.convergenceCommitter = committer
}
