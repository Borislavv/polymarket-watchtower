package eventpage

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Polymarket's event-page payload is INTERNAL and frequently drifts
// between equivalent JSON encodings for the same logical field:
//
//   - market.volume: float (16592143.35) OR string ("4701578.76").
//   - market.outcomes: JSON array of strings (["Yes","No"]) OR a
//     JSON-encoded array baked as a string ("[\"Yes\",\"No\"]").
//   - market.lastTradePrice: float OR null OR sometimes string.
//
// The previous parser declared everything as float64/string and
// surrendered the entire `markets` array on the first drift it hit.
// Production evidence: 85 event-page snapshots persisted, ZERO rows
// in polymarket_event_page_markets, which transitively killed every
// downstream consumer that needs CurrentPrice (repricing, prediction
// evolution context, blocked-alert AI).
//
// The flex* types below accept the union of encodings Polymarket has
// been observed to ship. Each one records a ParseWarning when it had
// to fall back to a non-canonical decoding, so the operator gets
// visibility instead of silent normalisation.

// flexFloat64 is a number that may arrive as a JSON number OR a
// JSON string holding a number. Empty string and `null` decode to
// zero. Non-numeric strings are an error.
type flexFloat64 struct {
	val    float64
	set    bool
	source string // "number" | "string" | "null" | "missing"
}

func (f *flexFloat64) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		f.source = "null"
		return nil
	}
	// Plain number — fast path.
	if b[0] != '"' {
		v, err := strconv.ParseFloat(string(b), 64)
		if err != nil {
			return err
		}
		f.val = v
		f.set = true
		f.source = "number"
		return nil
	}
	// String — Polymarket sometimes ships "123.45" instead of 123.45.
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		f.source = "string"
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// Surface as an error so the caller can record a parse
		// warning and skip the field without aborting the whole
		// market parse.
		return err
	}
	f.val = v
	f.set = true
	f.source = "string"
	return nil
}

func (f flexFloat64) Float64() float64 { return f.val }
func (f flexFloat64) Drifted() bool    { return f.source == "string" }

// flexFloat64Ptr is the same as flexFloat64 but propagates `null` /
// missing as a nil pointer (matches the existing *float64 contract on
// EventPageMarket fields like LastTradePrice / BestBid / BestAsk).
type flexFloat64Ptr struct {
	v       *float64
	drifted bool
}

func (f *flexFloat64Ptr) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	if b[0] != '"' {
		var v float64
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		f.v = &v
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	f.v = &v
	f.drifted = true
	return nil
}

func (f flexFloat64Ptr) Ptr() *float64 { return f.v }
func (f flexFloat64Ptr) Drifted() bool { return f.drifted }

// flexStringList is a list of strings that may arrive as a real
// JSON array ["Yes","No"] OR as a JSON-encoded-string-of-an-array
// "[\"Yes\",\"No\"]". Empty / null / "[]" decode to a nil slice.
type flexStringList struct {
	vals    []string
	drifted bool
}

func (f *flexStringList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	// Real JSON array.
	if b[0] == '[' {
		return json.Unmarshal(b, &f.vals)
	}
	// JSON-encoded string holding an array.
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" || s == "[]" {
			f.drifted = true
			return nil
		}
		if err := json.Unmarshal([]byte(s), &f.vals); err != nil {
			return err
		}
		f.drifted = true
		return nil
	}
	return &json.UnmarshalTypeError{Value: "non-array", Type: nil}
}

func (f flexStringList) Vals() []string { return f.vals }
func (f flexStringList) Drifted() bool  { return f.drifted }

// flexBool accepts true/false (canonical) AND "true"/"false" string
// variants Polymarket has been observed to emit on edge fields.
type flexBool struct {
	val     bool
	drifted bool
}

func (f *flexBool) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	if bytes.Equal(b, []byte("true")) {
		f.val = true
		return nil
	}
	if bytes.Equal(b, []byte("false")) {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(strings.ToLower(s))
		switch s {
		case "true", "1", "yes":
			f.val = true
			f.drifted = true
			return nil
		case "false", "0", "no", "":
			f.drifted = true
			return nil
		}
	}
	return &json.UnmarshalTypeError{Value: "non-bool", Type: nil}
}

func (f flexBool) Bool() bool { return f.val }
