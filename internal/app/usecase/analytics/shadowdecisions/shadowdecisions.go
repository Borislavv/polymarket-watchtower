// Package shadowdecisions is the common audit + value-tracking
// trail every v11.5 detector writes to before / instead of emitting
// a live Telegram alert.
//
// The package exposes:
//   - Decision (the typed row a detector hands the writer);
//   - DecisionKind / DecisionLevel enums;
//   - Writer interface satisfied by the production repository;
//   - NopWriter for tests / disabled detectors.
//
// Per v11.5 spec rule: "ни один новый детектор не должен писать
// прямо в Telegram". Detectors call Writer.Record from the
// orchestration layer (detect.Loop or per-worker tick), never from
// inside their pure Decide() function.
package shadowdecisions

import (
	"context"
	"encoding/json"
	"time"
)

// DecisionKind enumerates the operational effect a detector's
// verdict has on downstream pipeline state.
type DecisionKind string

const (
	// KindStandalone — the detector wants to fire its own alert
	// (only honored once the strategy has passed promotion
	// criteria; otherwise shadow_only=true).
	KindStandalone DecisionKind = "standalone"
	// KindBoost — booster bumps an existing alert's score /
	// severity.
	KindBoost DecisionKind = "boost"
	// KindSuppress — suppress a competing alert.
	KindSuppress DecisionKind = "suppress"
	// KindDegrade — downgrade severity of an alert.
	KindDegrade DecisionKind = "degrade"
	// KindTag — attach a reason / context label without changing
	// severity (e.g. ambiguity score).
	KindTag DecisionKind = "tag"
)

// DecisionLevel is the severity ladder reused from the v11.x
// alert taxonomy. "none" is the default for shadow rows that
// haven't picked a severity yet (boosters, tags).
type DecisionLevel string

const (
	LevelNone     DecisionLevel = "none"
	LevelInfo     DecisionLevel = "info"
	LevelWarning  DecisionLevel = "warning"
	LevelCritical DecisionLevel = "critical"
	LevelHard     DecisionLevel = "hard"
)

// Decision is the canonical row every detector writes. The two
// JSON fields (Reasons + Features) are free-form per detector but
// keyed to the strategy_name so a Grafana query can pivot on
// "show me thesisaccum decisions whose features.breadth >= 3".
type Decision struct {
	StrategyName        string
	StrategyVersion     string
	ConditionID         string
	EventSlug           string
	Wallet              string
	CohortID            string // empty if not cohort-aware
	Side                string
	Kind                DecisionKind
	Level               DecisionLevel
	Score               float64
	Confidence          float64
	Reasons             []string
	Features            map[string]any
	ShadowOnly          bool
	LinkedAlertDedupKey string // empty for pure shadow rows
	ControlBucketKey    string
	FiredAt             time.Time
}

// Writer is the production sink for shadow decisions. Implemented
// by repository.ShadowDecisionsRepository.
type Writer interface {
	Record(ctx context.Context, d Decision) (int64, error)
}

// NopWriter is the default for tests + disabled detectors. Record
// returns (0, nil) without side effects.
type NopWriter struct{}

func (NopWriter) Record(_ context.Context, _ Decision) (int64, error) { return 0, nil }

// MarshalReasons + MarshalFeatures are small helpers detectors use
// when the orchestration layer builds the Decision row. nil maps
// to NULL JSONB at the repository layer.
func MarshalReasons(reasons []string) ([]byte, error) {
	if len(reasons) == 0 {
		return nil, nil
	}
	return json.Marshal(reasons)
}

func MarshalFeatures(features map[string]any) ([]byte, error) {
	if len(features) == 0 {
		return nil, nil
	}
	return json.Marshal(features)
}

// ControlBucketKey is the deterministic matched-control identifier
// used by Grafana / promotion analysis to compare a detector's
// signed-move uplift against a "no-detector" baseline. The format
// is intentionally compact and stable: category|lifecycle_bucket|
// odds_bucket|notional_bucket|event_kind. Callers pass the
// already-bucketed values.
//
// Example: "Politics|75-100|0.45-0.55|10k-100k|election_day".
func ControlBucketKey(category, lifecycleBucket, oddsBucket, notionalBucket, eventKind string) string {
	parts := []string{category, lifecycleBucket, oddsBucket, notionalBucket, eventKind}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "|"
		}
		if p == "" {
			out += "_"
			continue
		}
		out += p
	}
	return out
}
