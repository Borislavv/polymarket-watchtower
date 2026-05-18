package alertsender

import (
	"errors"
	"math/rand"
	"testing"
	"time"
)

func TestBackoffGrowsExponentiallyAndCaps(t *testing.T) {
	p := RetryPolicy{
		Enabled:        true,
		MaxAttempts:    5,
		InitialBackoff: time.Second,
		MaxBackoff:     time.Minute,
		JitterFraction: 0,
	}
	rng := rand.New(rand.NewSource(1))
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{7, time.Minute},  // capped
		{10, time.Minute}, // capped
	}
	for _, c := range cases {
		got := backoff(p, c.attempt, rng)
		if got != c.want {
			t.Errorf("attempt %d: got %s want %s", c.attempt, got, c.want)
		}
	}
}

func TestBackoffJitterIsBounded(t *testing.T) {
	p := RetryPolicy{
		Enabled:        true,
		MaxAttempts:    5,
		InitialBackoff: time.Second,
		MaxBackoff:     time.Minute,
		JitterFraction: 0.2,
	}
	rng := rand.New(rand.NewSource(42))
	// Run many samples and assert all fall within [0.8s, 1.2s] for
	// attempt=1.
	for i := 0; i < 1000; i++ {
		got := backoff(p, 1, rng)
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("jittered delay out of bounds: %s", got)
			break
		}
	}
}

func TestIsPermanentError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("render: invalid character"), true},
		{errors.New("telegram: chat 1 -> 400: {\"description\":\"Bad Request: can't parse entities: …\"}"), true},
		{errors.New("Bad Request: chat not found"), true},
		{errors.New("bot was kicked from chat"), true},
		{errors.New("bot is not a member of the channel"), true},
		{errors.New("have no rights to send a message"), true},
		{errors.New("message is too long"), true},

		// Transient signatures should NOT be classified permanent.
		{errors.New("telegram: chat 1 -> 500: internal server error"), false},
		{errors.New("net/http: timeout awaiting response headers"), false},
		{errors.New("connection refused"), false},
		{nil, false},
	}
	for _, c := range cases {
		got := isPermanentError(c.err)
		if got != c.want {
			t.Errorf("isPermanentError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
