package controller

import (
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/pod-nsg-controller/internal/azure"
)

// ConvergenceCommitter is a callback invoked by the status updater after a
// successful status write (or semantic no-op) to commit convergence metrics.
type ConvergenceCommitter func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult)

// SetConvergenceCommitter sets the convergence committer callback on the status updater.
// The committer is invoked after UpdateAfterReconcile completes successfully or when
// the computed status is semantically equal to the existing status (no-op).
func (u *MappingStatusUpdater) SetConvergenceCommitter(committer ConvergenceCommitter) {
	u.convergenceCommitter = committer
}
