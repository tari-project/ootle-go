// Mandatory transaction validity window.
//
// Since core 0.39.0 every transaction carries a `max_epoch`: the last epoch it may be
// sequenced in. It is no longer optional, so an intent that leaves MaxEpoch at 0 has to be
// settled before it reaches the core. settleMaxEpoch does that from the indexer's current
// epoch, keeping the caller free of an epoch round-trip they did not ask for.

package ootle

import (
	"context"
	"fmt"
)

// DefaultValidityEpochs is how far past the current epoch an unpinned MaxEpoch is settled
// (current + DefaultValidityEpochs). It is deliberately short: a transfer that has not been
// sequenced within this window is stale, and a tight window bounds how long a leaked
// sealed envelope stays replayable. Pin MaxEpoch explicitly for a longer-lived transaction.
const DefaultValidityEpochs uint64 = 10

// MaxTransactionValidityEpochs mirrors the network's `max_transaction_validity_epochs`
// consensus constant (~30 days). A transaction whose window exceeds it is aborted with
// VALIDITY_WINDOW_TOO_LONG; the SDK rejects such an intent locally instead of paying a fee
// to learn it.
const MaxTransactionValidityEpochs uint64 = 2160

// epochProvider is the optional transport capability to read the indexer's current epoch
// (GET /epoch-manager/stats). Mirroring finalizationStreamer, the driver type-asserts the
// stored Transport to it rather than widening Transport (which would break custom
// transports and the mocks). The concrete *transport.Client satisfies it.
type epochProvider interface {
	CurrentEpoch(ctx context.Context) (uint64, error)
}

// settleMaxEpoch resolves an intent's mandatory max_epoch. A non-zero maxEpoch is the
// caller's own pin and is returned unchanged (bounds-checked only); a zero one is settled
// to current + DefaultValidityEpochs from the transport. Without an epoch-capable
// transport a zero maxEpoch is a VALIDATION error rather than a silent 0 the network would
// reject as expired.
func (c *Client) settleMaxEpoch(ctx context.Context, maxEpoch, minEpoch uint64) (uint64, error) {
	if maxEpoch != 0 {
		if err := checkValidityWindow(maxEpoch, minEpoch); err != nil {
			return 0, err
		}
		return maxEpoch, nil
	}
	ep, ok := c.transport.(epochProvider)
	if !ok {
		return 0, &Error{
			Code:    "VALIDATION",
			Message: "intent has no MaxEpoch and the transport cannot read the current epoch: set MaxEpoch explicitly (it is mandatory since core 0.39.0)",
		}
	}
	current, err := ep.CurrentEpoch(ctx)
	if err != nil {
		return 0, err
	}
	return current + DefaultValidityEpochs, nil
}

// checkValidityWindow rejects a caller-pinned window wider than the network allows, so the
// abort surfaces here instead of as a VALIDITY_WINDOW_TOO_LONG reject that costs a fee. A
// zero minEpoch means "unpinned", which the network measures from the current epoch — that
// window cannot be checked locally and is left to the engine.
func checkValidityWindow(maxEpoch, minEpoch uint64) error {
	if minEpoch == 0 || maxEpoch < minEpoch {
		return nil
	}
	if maxEpoch-minEpoch > MaxTransactionValidityEpochs {
		return &Error{
			Code: "VALIDATION",
			Message: fmt.Sprintf(
				"validity window %d epochs (MinEpoch %d → MaxEpoch %d) exceeds the network cap of %d",
				maxEpoch-minEpoch, minEpoch, maxEpoch, MaxTransactionValidityEpochs,
			),
		}
	}
	return nil
}

// derefEpoch reads an optional epoch pin, mapping "unset" to 0 (min_epoch stays optional in
// the core; only max_epoch became mandatory).
func derefEpoch(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}
