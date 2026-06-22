package ootle

import (
	"encoding/json"
	"errors"
	"strings"
)

// FinalizedResult is the full, typed boundary view of a finalized transaction result.
// It mirrors the core's FinalizedResult (crates/ootle_sdk_core/src/types/result.rs):
// the Submit ack carries the id + accept/reject outcome; the remaining fields are
// present when the engine returned an execution result. A still-Pending result yields
// a FinalizedResult whose Submit.Outcome is nil and whose nested fields are empty.
//
// This type is produced ONLY by the core's parse_finalized_result (via cffi); the Go
// side never derives any of it — it just unmarshals the core's JSON.
type FinalizedResult struct {
	// Submit is the submit acknowledgement (id + outcome).
	Submit SubmitResult `json:"submit"`
	// FeeReceipt is the fee receipt, when an execution result was present.
	FeeReceipt *FeeReceipt `json:"fee_receipt,omitempty"`
	// DiffSummary is the accepted substate diff summary, when accepted.
	DiffSummary *DiffSummary `json:"diff_summary,omitempty"`
	// Events are the emitted events.
	Events []EventSummary `json:"events,omitempty"`
	// Logs are the emitted logs.
	Logs []LogSummary `json:"logs,omitempty"`
	// Epoch is the epoch the transaction executed in, when known.
	Epoch *uint64 `json:"epoch,omitempty"`
	// EstimatedFee is the estimated minimum fee (in µTari), surfaced ONLY for a dry-run
	// result — the core's required_fees (total_fees_charged + 1), the minimum max_fee to
	// use for the real submission. A committed result leaves this nil and exposes the
	// realized fee in FeeReceipt instead. The core computes it; the Go side only
	// unmarshals the u64 (never coerced through a float).
	EstimatedFee *uint64 `json:"estimated_fee,omitempty"`
}

// IsCommit reports whether the transaction was fully committed. It is nil-safe: a
// still-pending result (nil Outcome) reports false.
func (r FinalizedResult) IsCommit() bool { return r.Submit.Outcome.IsCommit() }

// RejectReason returns the reject reason when the transaction was rejected or only its
// fee committed, and nil otherwise (including while still pending).
func (r FinalizedResult) RejectReason() *RejectReason { return r.Submit.Outcome.RejectReason() }

// Pending reports whether the result has no outcome yet (still awaiting finality).
func (r FinalizedResult) Pending() bool { return r.Submit.Outcome == nil }

// EstimatedFeeOr returns the estimated minimum fee (µTari) from a dry-run result, or def
// when no estimate is present (e.g. a committed, non-dry-run result).
func (r FinalizedResult) EstimatedFeeOr(def uint64) uint64 {
	if r.EstimatedFee == nil {
		return def
	}
	return *r.EstimatedFee
}

// SubmitResult is a submit acknowledgement: the transaction id plus its (optional,
// known-later) outcome.
type SubmitResult struct {
	// TransactionID is the submitted transaction's id (lowercase hex).
	TransactionID string `json:"transaction_id"`
	// Outcome is the outcome once finalized; nil while still pending.
	Outcome *TransactionOutcome `json:"outcome"`
}

// TransactionOutcome mirrors the core's externally-tagged TransactionOutcome:
//
//	"Commit"                       — fully committed
//	{"OnlyFeeCommit": RejectReason} — only the fee intent committed
//	{"Reject": RejectReason}        — rejected
//
// Exactly one of the booleans / pointers is set after unmarshalling.
type TransactionOutcome struct {
	// Commit is true when the transaction was fully committed.
	Commit bool
	// OnlyFeeCommit carries the reject reason when only the fee intent committed.
	OnlyFeeCommit *RejectReason
	// Reject carries the reject reason when the transaction was rejected.
	Reject *RejectReason
}

// IsCommit reports whether the transaction was fully committed.
func (o *TransactionOutcome) IsCommit() bool { return o != nil && o.Commit }

// RejectReason is the reject reason, if any (set for both OnlyFeeCommit and Reject).
func (o *TransactionOutcome) RejectReason() *RejectReason {
	if o == nil {
		return nil
	}
	if o.OnlyFeeCommit != nil {
		return o.OnlyFeeCommit
	}
	return o.Reject
}

// UnmarshalJSON decodes the externally-tagged TransactionOutcome enum.
func (o *TransactionOutcome) UnmarshalJSON(data []byte) error {
	// Unit variant: the bare string "Commit".
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s != "Commit" {
			return errors.New("ootle: unknown TransactionOutcome unit variant " + s)
		}
		o.Commit = true
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	if raw, ok := obj["OnlyFeeCommit"]; ok {
		var r RejectReason
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		o.OnlyFeeCommit = &r
		return nil
	}
	if raw, ok := obj["Reject"]; ok {
		var r RejectReason
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		o.Reject = &r
		return nil
	}
	return errors.New("ootle: unrecognised TransactionOutcome JSON")
}

// MarshalJSON re-emits the externally-tagged form (round-trips the core shape).
func (o TransactionOutcome) MarshalJSON() ([]byte, error) {
	switch {
	case o.OnlyFeeCommit != nil:
		return json.Marshal(map[string]*RejectReason{"OnlyFeeCommit": o.OnlyFeeCommit})
	case o.Reject != nil:
		return json.Marshal(map[string]*RejectReason{"Reject": o.Reject})
	case o.Commit:
		return json.Marshal("Commit")
	default:
		return nil, errors.New("ootle: empty TransactionOutcome")
	}
}

// RejectReason is a boundary reject reason: a stable Code plus the rendered Message,
// with an optional canonical AbortCode (e.g. "EPOCH_EXPIRED") when this is an abort.
type RejectReason struct {
	// Code is the stable variant code, e.g. "EXECUTION_FAILURE".
	Code string `json:"code"`
	// AbortCode is the canonical AbortReason sub-code when this is an abort; else empty.
	AbortCode string `json:"abort_code,omitempty"`
	// Message is the human-readable detail.
	Message string `json:"message"`
}

// FeeReceipt is a boundary fee receipt — all amounts are u64-safe (µTari).
type FeeReceipt struct {
	// TotalFeePayment is the total fee payment(s) before refunds.
	TotalFeePayment uint64 `json:"total_fee_payment"`
	// TotalFeesPaid is the total fees paid after refunds.
	TotalFeesPaid uint64 `json:"total_fees_paid"`
	// TotalFeeOvercharge is the non-refundable overpaid fees.
	TotalFeeOvercharge uint64 `json:"total_fee_overcharge"`
	// CostBreakdown is the per-source breakdown as (source, amount) pairs.
	CostBreakdown []FeeCost `json:"cost_breakdown"`
}

// FeeCost is one (FeeSource, amount) entry of a FeeReceipt's cost breakdown. The core
// emits it as a 2-tuple JSON array [source, amount]; FeeCost (un)marshals that form.
type FeeCost struct {
	// Source is the FeeSource name (e.g. "Initial", "Storage").
	Source string
	// Amount is the cost charged from that source, in µTari.
	Amount uint64
}

// UnmarshalJSON decodes the core's [source, amount] tuple form.
func (c *FeeCost) UnmarshalJSON(data []byte) error {
	var tuple []json.RawMessage
	if err := json.Unmarshal(data, &tuple); err != nil {
		return err
	}
	if len(tuple) != 2 {
		return errors.New("ootle: FeeCost must be a 2-element [source, amount] array")
	}
	if err := json.Unmarshal(tuple[0], &c.Source); err != nil {
		return err
	}
	return json.Unmarshal(tuple[1], &c.Amount)
}

// MarshalJSON re-emits the [source, amount] tuple form.
func (c FeeCost) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{c.Source, c.Amount})
}

// UpSubstate is one created (up) substate in a DiffSummary: its id and version.
type UpSubstate struct {
	// SubstateID is the substate id (canonical string form).
	SubstateID string `json:"substate_id"`
	// Version is the created version.
	Version uint32 `json:"version"`
}

// DownSubstate is one destroyed (down) substate: id + version. The core emits it as a
// 2-tuple JSON array [id, version].
type DownSubstate struct {
	// SubstateID is the destroyed substate id.
	SubstateID string
	// Version is the destroyed version.
	Version uint32
}

// UnmarshalJSON decodes the core's [id, version] tuple form.
func (d *DownSubstate) UnmarshalJSON(data []byte) error {
	var tuple []json.RawMessage
	if err := json.Unmarshal(data, &tuple); err != nil {
		return err
	}
	if len(tuple) != 2 {
		return errors.New("ootle: DownSubstate must be a 2-element [id, version] array")
	}
	if err := json.Unmarshal(tuple[0], &d.SubstateID); err != nil {
		return err
	}
	return json.Unmarshal(tuple[1], &d.Version)
}

// MarshalJSON re-emits the [id, version] tuple form.
func (d DownSubstate) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{d.SubstateID, d.Version})
}

// DiffSummary is a boundary diff summary — ids + versions of created/destroyed
// substates (not their bodies).
type DiffSummary struct {
	// Up are the created (up) substates.
	Up []UpSubstate `json:"up"`
	// Down are the destroyed (down) substates.
	Down []DownSubstate `json:"down"`
}

// Canonical substate id prefixes, used to pick a newly created substate out of a diff.
const (
	PrefixComponent = "component_"
	PrefixTemplate  = "template_"
	PrefixResource  = "resource_"
	PrefixUTXO      = "utxo_"
)

// FirstUp returns the first created (up) substate id with the given prefix that is not in
// exclude, and false if none matches. It is nil-safe: a nil *DiffSummary yields "", false.
func (d *DiffSummary) FirstUp(prefix string, exclude ...string) (string, bool) {
	if d == nil {
		return "", false
	}
	for _, up := range d.Up {
		id := up.SubstateID
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		if containsString(exclude, id) {
			continue
		}
		return id, true
	}
	return "", false
}

// NewComponent returns the first created component_ id not in exclude.
func (d *DiffSummary) NewComponent(exclude ...string) (string, bool) {
	return d.FirstUp(PrefixComponent, exclude...)
}

// NewTemplate returns the first created template_ id not in exclude.
func (d *DiffSummary) NewTemplate(exclude ...string) (string, bool) {
	return d.FirstUp(PrefixTemplate, exclude...)
}

// NewUTXO returns the first created utxo_ id not in exclude.
func (d *DiffSummary) NewUTXO(exclude ...string) (string, bool) {
	return d.FirstUp(PrefixUTXO, exclude...)
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// EventPayload is one (key, value) entry of an event payload. The core emits it as a
// 2-tuple JSON array [key, value].
type EventPayload struct {
	// Key is the payload entry key.
	Key string
	// Value is the payload entry value.
	Value string
}

// UnmarshalJSON decodes the core's [key, value] tuple form.
func (p *EventPayload) UnmarshalJSON(data []byte) error {
	var tuple []string
	if err := json.Unmarshal(data, &tuple); err != nil {
		return err
	}
	if len(tuple) != 2 {
		return errors.New("ootle: EventPayload must be a 2-element [key, value] array")
	}
	p.Key, p.Value = tuple[0], tuple[1]
	return nil
}

// MarshalJSON re-emits the [key, value] tuple form.
func (p EventPayload) MarshalJSON() ([]byte, error) {
	return json.Marshal([]string{p.Key, p.Value})
}

// EventSummary is a boundary event summary — engine Event flattened to strings.
type EventSummary struct {
	// SubstateID is the emitting substate id, if any.
	SubstateID string `json:"substate_id,omitempty"`
	// TemplateAddress is the template that emitted the event (canonical string form).
	TemplateAddress string `json:"template_address"`
	// Topic is the event topic.
	Topic string `json:"topic"`
	// Payload is the event payload as (key, value) pairs.
	Payload []EventPayload `json:"payload"`
}

// LogSummary is a boundary log summary — engine LogEntry flattened to strings.
type LogSummary struct {
	// Message is the log message.
	Message string `json:"message"`
	// Level is the log level (e.g. "INFO", "ERROR").
	Level string `json:"level"`
}
