package ootle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/tari-project/ootle-go/transport"
)

// TestSubmitSealed_DrivesSubmitWaitParse asserts SubmitSealed hex→base64-encodes the
// envelope, submits it, and parses the finalized result the transport returns. The encoded
// transaction is a synthetic hex blob (SubmitSealed only re-encodes it — it does not decode or
// seal), and the expected result comes from a committed parse vector, so the parse runs through
// the real core parser. The hex→base64 byte-compare is exact here (it tests the transport
// encoding, not the random-nonce seal).
func TestSubmitSealed_DrivesSubmitWaitParse(t *testing.T) {
	const encodedTxHex = "0102030405060708090a0b0c0d0e0f10"
	wantEnvelopeB64 := base64.StdEncoding.EncodeToString(mustHex(t, encodedTxHex))
	rawResult, wantParsed := loadParseRaw(t, "accept.json")

	var gotEnvelope string
	mock := &mockTransport{
		fetch: func(context.Context, []string) ([]transport.FetchedSubstate, error) {
			t.Error("SubmitSealed must not fetch substates")
			return nil, nil
		},
		submit: func(_ context.Context, envelopeB64 string) (string, error) {
			gotEnvelope = envelopeB64
			return fakeTxID, nil
		},
		result: func(_ context.Context, _ string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}

	c := NewClient(mock)
	result, err := c.SubmitSealed(context.Background(), EncodedPublicTransfer{
		EncodedTransaction: encodedTxHex,
		TransactionID:      fakeTxID,
	})
	if err != nil {
		t.Fatalf("SubmitSealed: %v", err)
	}
	if gotEnvelope != wantEnvelopeB64 {
		t.Errorf("submitted envelope mismatch:\n got:  %s\n want: %s", gotEnvelope, wantEnvelopeB64)
	}
	if !reflect.DeepEqual(result, wantParsed) {
		gj, _ := json.MarshalIndent(result, "", "  ")
		wj, _ := json.MarshalIndent(wantParsed, "", "  ")
		t.Errorf("parsed FinalizedResult mismatch:\n got:  %s\n want: %s", gj, wj)
	}
}

// TestSubmitSealed_BadHexErrorsBeforeSubmit asserts an invalid (non-hex) EncodedTransaction
// yields an ENCODING *Error and never reaches the transport.
func TestSubmitSealed_BadHexErrorsBeforeSubmit(t *testing.T) {
	mock := &mockTransport{
		fetch: func(context.Context, []string) ([]transport.FetchedSubstate, error) {
			t.Error("SubmitSealed must not fetch substates")
			return nil, nil
		},
		submit: func(context.Context, string) (string, error) {
			t.Error("Submit must not be called when the envelope hex is invalid")
			return "", nil
		},
	}
	c := NewClient(mock)
	_, err := c.SubmitSealed(context.Background(), EncodedPublicTransfer{EncodedTransaction: "zz"})
	var oe *Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *ootle.Error, got %T: %v", err, err)
	}
	if oe.Code != "ENCODING" {
		t.Errorf("error code = %q, want ENCODING", oe.Code)
	}
}
