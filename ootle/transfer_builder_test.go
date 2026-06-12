package ootle

import (
	"encoding/json"
	"testing"
)

// sameTransferJSON asserts two intents marshal to byte-identical core JSON.
func sameTransferJSON(t *testing.T, got, want PublicTransferIntent) {
	t.Helper()
	gj, err := got.marshalJSON()
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wj, err := want.marshalJSON()
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gj) != string(wj) {
		t.Fatalf("intent mismatch:\n got: %s\nwant: %s", gj, wj)
	}
}

func TestTransferBuilder_ToPublicKey(t *testing.T) {
	got := NewTransfer("component_from").
		ToPublicKey("aa11").
		Resource("resource_tari").
		Amount(1000).
		Fee(50).
		Intent()

	want := PublicTransferIntent{
		FromAccount:     "component_from",
		Recipient:       PublicKeyRecipient("aa11"),
		ResourceAddress: "resource_tari",
		Amount:          1000,
		Fee:             50,
	}
	sameTransferJSON(t, got, want)

	if len(got.Inputs) != 0 {
		t.Fatalf("builder must leave Inputs empty, got %d", len(got.Inputs))
	}
}

func TestTransferBuilder_ToAccount(t *testing.T) {
	got := NewTransfer("component_from").
		ToAccount("component_to").
		Resource("resource_tari").
		Amount(7).
		Fee(2).
		Intent()

	want := PublicTransferIntent{
		FromAccount:     "component_from",
		Recipient:       AccountRecipient("component_to"),
		ResourceAddress: "resource_tari",
		Amount:          7,
		Fee:             2,
	}
	sameTransferJSON(t, got, want)
}

func TestTransferBuilder_DryRunAndEpochs(t *testing.T) {
	got := NewTransfer("component_from").
		ToAccount("component_to").
		Resource("resource_tari").
		Amount(7).
		Fee(2).
		MinEpoch(3).
		MaxEpoch(9).
		DryRun().
		Intent()

	if !got.DryRun {
		t.Fatal("DryRun() did not set the flag")
	}
	if got.MinEpoch == nil || *got.MinEpoch != 3 {
		t.Fatalf("MinEpoch = %v, want 3", got.MinEpoch)
	}
	if got.MaxEpoch == nil || *got.MaxEpoch != 9 {
		t.Fatalf("MaxEpoch = %v, want 9", got.MaxEpoch)
	}
}

func TestTransferRecipient_MarshalRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    TransferRecipient
		want string
	}{
		{"public key", PublicKeyRecipient("dead"), `{"PublicKey":"dead"}`},
		{"account", AccountRecipient("component_x"), `{"Account":"component_x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.r)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tc.want {
				t.Fatalf("marshal = %s, want %s", b, tc.want)
			}
			var back TransferRecipient
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			rt, err := json.Marshal(back)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if string(rt) != tc.want {
				t.Fatalf("round-trip = %s, want %s", rt, tc.want)
			}
		})
	}
}

func TestTransferRecipient_ZeroValueMarshalErrors(t *testing.T) {
	var r TransferRecipient
	if _, err := json.Marshal(r); err == nil {
		t.Fatal("zero-value TransferRecipient should fail to marshal")
	}
}
