package ootle

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// epochMockTransport is a mockTransport that also answers the optional epochProvider
// capability, so the driver can settle an unpinned max_epoch without a live indexer.
type epochMockTransport struct {
	mockTransport
	epoch uint64
	err   error
}

func (m *epochMockTransport) CurrentEpoch(ctx context.Context) (uint64, error) {
	if m.err != nil {
		return 0, m.err
	}
	return m.epoch, nil
}

var _ epochProvider = (*epochMockTransport)(nil)

// A caller-pinned MaxEpoch is returned verbatim and never costs an epoch round-trip — even
// when the transport cannot answer one.
func TestSettleMaxEpoch_PinnedIsKept(t *testing.T) {
	c := NewClient(&mockTransport{}, WithNetwork(NetworkLocalNet))
	got, err := c.settleMaxEpoch(context.Background(), 77, 0)
	if err != nil {
		t.Fatalf("settleMaxEpoch: %v", err)
	}
	if got != 77 {
		t.Fatalf("MaxEpoch = %d, want 77 (the caller's pin)", got)
	}
}

// An unpinned MaxEpoch is settled to the indexer's current epoch + DefaultValidityEpochs.
func TestSettleMaxEpoch_SettledFromCurrentEpoch(t *testing.T) {
	c := NewClient(&epochMockTransport{epoch: 42}, WithNetwork(NetworkLocalNet))
	got, err := c.settleMaxEpoch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("settleMaxEpoch: %v", err)
	}
	if want := 42 + DefaultValidityEpochs; got != want {
		t.Fatalf("MaxEpoch = %d, want %d (current + DefaultValidityEpochs)", got, want)
	}
}

// An epoch read that fails surfaces the transport error rather than a silent zero window.
func TestSettleMaxEpoch_TransportErrorPropagates(t *testing.T) {
	sentinel := errors.New("indexer down")
	c := NewClient(&epochMockTransport{err: sentinel}, WithNetwork(NetworkLocalNet))
	if _, err := c.settleMaxEpoch(context.Background(), 0, 0); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the transport error", err)
	}
}

// Without an epoch-capable transport an unpinned MaxEpoch is an actionable VALIDATION error,
// never a zero the network would reject as expired.
func TestSettleMaxEpoch_NoProviderIsValidationError(t *testing.T) {
	c := NewClient(&mockTransport{}, WithNetwork(NetworkLocalNet))
	_, err := c.settleMaxEpoch(context.Background(), 0, 0)
	var oerr *Error
	if !errors.As(err, &oerr) || oerr.Code != "VALIDATION" {
		t.Fatalf("err = %v, want a VALIDATION *Error", err)
	}
}

// A pinned MaxEpoch is taken as given, however far out: consensus measures its cap from the
// epoch the transaction is sequenced in, which the host cannot know, and the SDK cannot read the
// live constant. A duplicated local limit would refuse valid transactions on a network with a
// raised ceiling.
func TestSettleMaxEpoch_FarPinIsNotSecondGuessed(t *testing.T) {
	c := NewClient(&epochMockTransport{epoch: 5000}, WithNetwork(NetworkLocalNet))
	pin := 5000 + MaxTransactionValidityEpochs*10
	got, err := c.settleMaxEpoch(context.Background(), pin, 0)
	if err != nil {
		t.Fatalf("settleMaxEpoch: %v", err)
	}
	if got != pin {
		t.Fatalf("MaxEpoch = %d, want %d (the caller's pin, unmodified)", got, pin)
	}
}

// The window rule the host CAN decide alone: a pin below MinEpoch can never contain a
// sequenceable epoch, so it is rejected without needing the current epoch.
func TestSettleMaxEpoch_EmptyWindowRejected(t *testing.T) {
	c := NewClient(&epochMockTransport{epoch: 100}, WithNetwork(NetworkLocalNet))
	_, err := c.settleMaxEpoch(context.Background(), 500, 1000)
	var oerr *Error
	if !errors.As(err, &oerr) || oerr.Code != "VALIDATION" {
		t.Fatalf("err = %v, want a VALIDATION *Error", err)
	}
	if !strings.Contains(oerr.Message, "empty validity window") {
		t.Errorf("message = %q, want it to name the empty window", oerr.Message)
	}

	// MaxEpoch == MinEpoch is a single-epoch window, which is legal.
	if _, err := c.settleMaxEpoch(context.Background(), 1000, 1000); err != nil {
		t.Fatalf("a single-epoch window must be accepted, got %v", err)
	}
}

// A MinEpoch beyond the current epoch is a legal pin, so an unpinned MaxEpoch settles from
// MinEpoch — never from `current` alone, which would sign a window that is empty by
// construction (max_epoch below the caller's own min_epoch).
func TestSettleMaxEpoch_SettlesFromMinEpochFloor(t *testing.T) {
	c := NewClient(&epochMockTransport{epoch: 100}, WithNetwork(NetworkLocalNet))
	got, err := c.settleMaxEpoch(context.Background(), 0, 1000)
	if err != nil {
		t.Fatalf("settleMaxEpoch: %v", err)
	}
	if want := 1000 + DefaultValidityEpochs; got != want {
		t.Fatalf("MaxEpoch = %d, want %d (MinEpoch + DefaultValidityEpochs)", got, want)
	}
	if got <= 1000 {
		t.Fatalf("settled MaxEpoch %d is not after MinEpoch 1000 — the window is empty", got)
	}
}

// A MinEpoch already in the past does not drag the window backwards: the current epoch is the
// floor in that direction.
func TestSettleMaxEpoch_PastMinEpochDoesNotLowerTheWindow(t *testing.T) {
	c := NewClient(&epochMockTransport{epoch: 5000}, WithNetwork(NetworkLocalNet))
	got, err := c.settleMaxEpoch(context.Background(), 0, 1)
	if err != nil {
		t.Fatalf("settleMaxEpoch: %v", err)
	}
	if want := 5000 + DefaultValidityEpochs; got != want {
		t.Fatalf("MaxEpoch = %d, want %d (current + DefaultValidityEpochs)", got, want)
	}
}

// The settle arithmetic saturates instead of wrapping: a wrapped sum would land below MinEpoch
// and sign the empty window the floor exists to prevent. Unreachable at real epoch rates.
func TestSettleMaxEpoch_SaturatesInsteadOfWrapping(t *testing.T) {
	const maxUint64 = ^uint64(0)
	minEpoch := maxUint64 - 1
	c := NewClient(&epochMockTransport{epoch: 1}, WithNetwork(NetworkLocalNet))
	got, err := c.settleMaxEpoch(context.Background(), 0, minEpoch)
	if err != nil {
		t.Fatalf("settleMaxEpoch: %v", err)
	}
	if got < minEpoch {
		t.Fatalf("settled MaxEpoch %d wrapped below MinEpoch %d — the window is empty", got, minEpoch)
	}
	if got != maxUint64 {
		t.Fatalf("MaxEpoch = %d, want %d (saturated)", got, uint64(maxUint64))
	}
}

// SendPublicTransfer settles an unpinned MaxEpoch from the transport and drives the full flow
// with it, without mutating the caller's intent.
func TestSendPublicTransfer_SettlesUnpinnedMaxEpoch(t *testing.T) {
	fx := loadResolveFixture(t, "single_key_basic.json")
	intent := fx.Input.Intent
	intent.MaxEpoch = 0 // unpinned: the driver must settle it

	var rounds int
	mock := &epochMockTransport{epoch: 500}
	mock.fetch = fetchFromVector(fx, &rounds)
	c := NewClient(mock, WithNetwork(fx.Input.Network), WithoutFinalizationWait())

	if _, err := c.SendPublicTransfer(context.Background(), intent, fx.Input.Keys); err != nil {
		t.Fatalf("SendPublicTransfer: %v", err)
	}

	// The settle runs on the driver's own copy: the caller's intent is left untouched.
	if intent.MaxEpoch != 0 {
		t.Fatalf("the caller's intent was mutated: MaxEpoch = %d", intent.MaxEpoch)
	}
}

// The transport-free one-shot cannot settle an epoch itself, so an unset MaxEpoch is a
// VALIDATION error rather than a transaction the network sees as already expired.
func TestBuildAndEncodeStealthTransfer_RequiresMaxEpoch(t *testing.T) {
	intent := StealthTransferIntent{FromAccount: "component_00", ResourceAddress: TariResource}
	keys := StealthTransferKeys{Seed: "00"}
	_, err := BuildAndEncodeStealthTransfer(NetworkLocalNet, intent, nil, keys)
	var oerr *Error
	if !errors.As(err, &oerr) || oerr.Code != "VALIDATION" {
		t.Fatalf("err = %v, want a VALIDATION *Error", err)
	}
	if !strings.Contains(oerr.Message, "MaxEpoch") {
		t.Errorf("message = %q, want it to name MaxEpoch", oerr.Message)
	}
}
