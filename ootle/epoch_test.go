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

// A caller-pinned window wider than the network cap is rejected locally, so the abort never
// costs a fee on-chain (VALIDITY_WINDOW_TOO_LONG).
func TestSettleMaxEpoch_WindowTooLong(t *testing.T) {
	c := NewClient(&epochMockTransport{epoch: 1}, WithNetwork(NetworkLocalNet))
	minEpoch := uint64(100)
	_, err := c.settleMaxEpoch(context.Background(), minEpoch+MaxTransactionValidityEpochs+1, minEpoch)
	var oerr *Error
	if !errors.As(err, &oerr) || oerr.Code != "VALIDATION" {
		t.Fatalf("err = %v, want a VALIDATION *Error", err)
	}

	// Exactly at the cap is accepted.
	if _, err := c.settleMaxEpoch(context.Background(), minEpoch+MaxTransactionValidityEpochs, minEpoch); err != nil {
		t.Fatalf("window at the cap must be accepted, got %v", err)
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
