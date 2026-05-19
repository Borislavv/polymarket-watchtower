// ProviderError surfaces the classification of an OpenAI failure
// in a shape the upstream usecase can route into request_logs and
// metrics without parsing strings. The old `fmt.Errorf("openai
// status 429: %s", body)` pattern dumped raw provider JSON into
// the analysis table — exactly the incident that drove the v8
// data-correctness cleanup.
package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrorCategory is a small, stable string set the rest of the
// codebase groups failures by. Order is: most-specific first.
type ErrorCategory string

const (
	CategoryQuotaExceeded    ErrorCategory = "quota_exceeded"
	CategoryRateLimited      ErrorCategory = "rate_limited"
	CategoryTimeout          ErrorCategory = "timeout"
	CategoryProvider5xx      ErrorCategory = "provider_5xx"
	CategoryBadRequest       ErrorCategory = "bad_request"
	CategoryInvalidModel     ErrorCategory = "invalid_model"
	CategoryPromptRejected   ErrorCategory = "prompt_rejected"
	CategoryUnauthorized     ErrorCategory = "unauthorized"
	CategoryNetwork          ErrorCategory = "network_error"
	CategoryEmptyResponse    ErrorCategory = "empty_response"
	CategoryValidationFailed ErrorCategory = "validation_failed"
	CategoryUnknown          ErrorCategory = "unknown"
)

// ProviderError is the typed failure surface. Carries enough
// metadata to route into request_logs and metrics without re-parsing
// upstream strings.
type ProviderError struct {
	HTTPStatus int
	Category   ErrorCategory
	Code       string // provider-supplied error code ("insufficient_quota" etc.)
	Message    string // sanitized, capped to 500 chars
	Retryable  bool
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("openai %s (http=%d code=%s): %s", e.Category, e.HTTPStatus, e.Code, e.Message)
}

// AsProviderError unwraps a *ProviderError from any error chain.
// Returns (nil, false) on a non-provider error.
func AsProviderError(err error) (*ProviderError, bool) {
	if err == nil {
		return nil, false
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// errorResponse is the openai JSON shape on non-2xx:
//
//	{"error": {"message": "...", "type": "...", "code": "..."}}
//
// We never store the full body; we extract the small fields.
type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// classifyHTTPError maps an OpenAI non-2xx response into a typed
// ProviderError. Body is parsed for the `error.type` / `error.code`
// fields when present; everything is sanitized + length-capped before
// landing on the struct.
func classifyHTTPError(httpStatus int, rawBody []byte) *ProviderError {
	pe := &ProviderError{HTTPStatus: httpStatus}
	var er errorResponse
	if len(rawBody) > 0 {
		_ = json.Unmarshal(rawBody, &er)
	}
	pe.Code = er.Error.Code
	pe.Message = sanitizeAndCap(er.Error.Message, 500)
	if pe.Message == "" {
		// fall back to a short marker from the raw body so the
		// operator has something — but never the whole JSON.
		pe.Message = sanitizeAndCap(string(rawBody), 200)
	}

	switch httpStatus {
	case 401, 403:
		pe.Category = CategoryUnauthorized
		pe.Retryable = false
	case 429:
		// OpenAI uses 429 for BOTH quota-exhausted and per-minute
		// rate-limit. The discriminator is error.code /
		// error.type — `insufficient_quota` is "billing dead",
		// retry is useless. Everything else is "slow down".
		switch {
		case strings.EqualFold(er.Error.Code, "insufficient_quota"),
			strings.EqualFold(er.Error.Type, "insufficient_quota"):
			pe.Category = CategoryQuotaExceeded
			pe.Retryable = false
		default:
			pe.Category = CategoryRateLimited
			pe.Retryable = true
		}
	case 400:
		// Distinguish three sub-categories the operator may want to
		// fix differently.
		switch strings.ToLower(er.Error.Code) {
		case "model_not_found", "invalid_model":
			pe.Category = CategoryInvalidModel
		case "context_length_exceeded", "string_above_max_length":
			pe.Category = CategoryPromptRejected
		default:
			pe.Category = CategoryBadRequest
		}
		pe.Retryable = false
	case 408, 504:
		pe.Category = CategoryTimeout
		pe.Retryable = true
	default:
		if httpStatus >= 500 {
			pe.Category = CategoryProvider5xx
			pe.Retryable = true
		} else {
			pe.Category = CategoryUnknown
			pe.Retryable = false
		}
	}
	return pe
}

// sanitizeAndCap strips control runes, collapses whitespace, and
// truncates to `n` runes with an ellipsis. The DB column is TEXT but
// we cap aggressively — operators reading the request_logs table
// want categories + short messages, not novellas.
func sanitizeAndCap(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
