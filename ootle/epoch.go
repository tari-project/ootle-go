// Mandatory transaction validity window.
//
// Since core 0.39.0 every transaction carries a `max_epoch`: the last epoch it may be
// sequenced in. It is no longer optional, so an intent that leaves MaxEpoch at 0 has to be
// settled before it reaches the core. settleMaxEpoch does that from the indexer's current
// epoch, keeping the caller free of an epoch round-trip they did not ask for. A caller-pinned
// MaxEpoch is taken as given — the network's validity-window cap is measured from the epoch the
// transaction is sequenced in, which the host cannot know, so it is not second-guessed here.

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
// consensus constant (~30 days): a transaction whose `max_epoch` is further than this past the
// epoch it is *sequenced* in aborts as VALIDITY_WINDOW_TOO_LONG.
//
// It is exported as a reference for callers choosing a MaxEpoch pin, and is deliberately NOT
// enforced here. Consensus measures the window from the pinned execution epoch
// (`window_abort_reason(pinned_epoch, max_epoch)`), which only the network knows at sequencing
// time — and the SDK cannot read the live constant. Enforcing a duplicated copy locally would
// wrongly refuse a valid transaction against a network with a raised ceiling; walletd records
// the same reasoning for its own copy of this value.
const MaxTransactionValidityEpochs uint64 = 2160

// epochProvider is the optional transport capability to read the indexer's current epoch
// (GET /epoch-manager/stats). Mirroring finalizationStreamer, the driver type-asserts the
// stored Transport to it rather than widening Transport (which would break custom
// transports and the mocks). The concrete *transport.Client satisfies it.
type epochProvider interface {
	CurrentEpoch(ctx context.Context) (uint64, error)
}

// settleMaxEpoch resolves an intent's mandatory max_epoch. A non-zero maxEpoch is the caller's
// own pin and is returned unchanged; a zero one is settled from the transport to
// max(current, minEpoch) + DefaultValidityEpochs. Without an epoch-capable transport a zero
// maxEpoch is a VALIDATION error rather than a silent 0 the network would reject as expired.
func (c *Client) settleMaxEpoch(ctx context.Context, maxEpoch, minEpoch uint64) (uint64, error) {
	if maxEpoch != 0 {
		if err := checkWindowIsNonEmpty(maxEpoch, minEpoch); err != nil {
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
	// Settle from whichever bound is later. A MinEpoch beyond the current epoch is a legal pin
	// (the transaction is not sequenceable before it), so measuring only from `current` would
	// sign a window that is empty by construction — max_epoch below the caller's own min_epoch.
	base := current
	if minEpoch > base {
		base = minEpoch
	}
	// Saturate rather than wrap. Unreachable at real epoch rates, but a wrapped sum would land
	// BELOW minEpoch — reintroducing, silently, the empty window the floor above exists to prevent.
	const maxUint64 = ^uint64(0)
	if base > maxUint64-DefaultValidityEpochs {
		return maxUint64, nil
	}
	return base + DefaultValidityEpochs, nil
}

// checkWindowIsNonEmpty rejects a pinned window that can never contain a sequenceable epoch. This
// is the only window rule the host can decide on its own: it needs no knowledge of the current
// epoch and no duplicated consensus constant. The network's own cap is measured from the
// execution epoch and is left to the network (see MaxTransactionValidityEpochs).
func checkWindowIsNonEmpty(maxEpoch, minEpoch uint64) error {
	if minEpoch == 0 || maxEpoch >= minEpoch {
		return nil
	}
	return &Error{
		Code: "VALIDATION",
		Message: fmt.Sprintf(
			"empty validity window: MaxEpoch %d is before MinEpoch %d, so the transaction can never be sequenced",
			maxEpoch, minEpoch,
		),
	}
}

// derefEpoch reads an optional epoch pin, mapping "unset" to 0 (min_epoch stays optional in
// the core; only max_epoch became mandatory).
func derefEpoch(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}
