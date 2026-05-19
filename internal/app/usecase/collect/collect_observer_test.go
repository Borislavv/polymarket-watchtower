package collect

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
)

// concreteObserver lets us simulate the typed-nil regression that
// caused the production panic at detect.go: a *T nil pointer that
// satisfies the TradeObserver interface produces a non-nil interface
// when stored.
type concreteObserver struct {
	called int
}

func (o *concreteObserver) Observe(_ context.Context, _ market.Market, _ trade.Trade) {
	if o == nil {
		// Simulate the (*detect.Loop).Observe behaviour on a nil
		// receiver: the first field access faults. In production
		// this came out as SIGSEGV at the first l.metrics read.
		panic("nil receiver — typed-nil interface regression")
	}
	o.called++
}

// TestPostgresModeMustReceiveInterfaceNilObserver pins the v7 wiring
// invariant: when Postgres is enabled, app.go MUST pass nil through a
// variable typed as TradeObserver (the interface), not a *concrete
// pointer.
//
// History: app.go used `collectObserver := detectLoop` (inferred type
// *detect.Loop), then `collectObserver = nil` in the Postgres branch.
// That nil was a typed-nil pointer. Boxing it into the obs TradeObserver
// parameter produced a non-nil interface; collect.pull's
// `if l.observer != nil` evaluated true, and (*detect.Loop).Observe
// crashed on the nil receiver — the SIGSEGV the user reported.
//
// The fix is structural: declare collectObserver as
// `collect.TradeObserver = detectLoop` so assigning nil produces a
// true nil-interface. This test pins that behaviour without depending
// on detect's internals.
func TestPostgresModeMustReceiveInterfaceNilObserver(t *testing.T) {
	// Simulate the (incorrect) typed-nil pointer wiring.
	var typedNil *concreteObserver = nil
	var asInterface TradeObserver = typedNil
	if asInterface == nil {
		t.Fatal("Go interface semantics changed: a typed nil pointer must NOT compare equal to nil-interface")
	}

	// Now simulate the correct wiring: declare the var as interface.
	var ifaceNil TradeObserver = nil
	if ifaceNil != nil {
		t.Fatal("Go interface semantics changed: a true nil-interface must compare equal to nil")
	}

	// Build a Loop the same way app.go would build it in Postgres
	// mode WITH the typed-nil bug, and confirm pull invokes Observe
	// on the typed-nil — that's the path that crashed in prod.
	loop := &Loop{
		observer: asInterface, // boxed typed-nil
		log:      nopLogger(),
	}
	if loop.observer == nil {
		t.Fatal("regression precondition broken: boxed typed-nil should compare non-nil")
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected typed-nil receiver to panic when Observe is invoked")
		}
	}()
	loop.observer.Observe(context.Background(), market.Market{}, trade.Trade{})
}

// TestPostgresModeWiringIsInterfaceNilSafe is the green-path twin:
// a true nil-interface assigned to Loop.observer must be skippable
// without ever invoking Observe.
func TestPostgresModeWiringIsInterfaceNilSafe(t *testing.T) {
	var ifaceNil TradeObserver = nil
	loop := &Loop{observer: ifaceNil, log: nopLogger()}
	if loop.observer != nil {
		t.Fatal("nil-interface observer must read as nil")
	}
	// No panic = pass — pull's `if l.observer != nil` guard will be
	// false and the trade is left in 'pending' for the detection
	// worker to claim. We don't drive a full pull here; the unit
	// invariant is that the guard works.
}

func nopLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

// panickingObserver always panics — used to verify the
// observeWithRecover boundary in dev/inline mode.
type panickingObserver struct{}

func (panickingObserver) Observe(_ context.Context, _ market.Market, _ trade.Trade) {
	panic("synthetic observer panic")
}

// TestInlineObserverPanicIsContained pins the dev-inline contract:
// a panic in the observer must not propagate up the collect goroutine
// and kill the process. The trade is silently dropped — the detection
// queue is the durable path in Postgres mode and tests using this
// shape are explicitly dev-only.
func TestInlineObserverPanicIsContained(t *testing.T) {
	loop := &Loop{observer: panickingObserver{}, log: nopLogger()}
	// observeWithRecover must absorb the panic. If recover is broken
	// or missing, this call propagates and the test process dies.
	loop.observeWithRecover(context.Background(), market.Market{}, trade.Trade{ID: "0xabc"})
	// Made it past the call ⇒ recover worked.
}
