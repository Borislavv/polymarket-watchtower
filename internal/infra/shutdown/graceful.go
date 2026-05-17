package shutdown

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Exec is a unit of long-running work whose cancellation is bound to ctx.
// Fn must return when ctx is Done; returning an error other than
// context.Canceled is reported back to the caller.
type Exec struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Graceful runs every Exec concurrently, cancels them when ctx is cancelled
// or when SIGINT/SIGTERM arrives, then waits up to fadeOutDuration for them
// to drain before returning.
func Graceful(ctx context.Context, execs []Exec, opts ...Options) error {
	if len(execs) == 0 {
		return errors.New("shutdown.Graceful: nothing to execute")
	}

	cfg := config{logger: nopLogger(), fadeOutDuration: 5 * time.Second}
	for _, opt := range opts {
		cfg = opt(cfg)
	}
	if cfg.logger == nil {
		cfg.logger = nopLogger()
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case <-runCtx.Done():
			return
		case sig := <-sigCh:
			cfg.logger.Info().Str("signal", sig.String()).Msg("shutdown signal received")
			cancelRun()
		}
	}()

	errsCh := make(chan error, len(execs))
	var wg sync.WaitGroup
	for _, exec := range execs {
		wg.Add(1)
		go func(exec Exec) {
			defer wg.Done()
			if err := exec.Fn(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				errsCh <- fmt.Errorf("%s: %w", exec.Name, err)
			}
		}(exec)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done); close(errsCh) }()

	select {
	case <-done:
	case <-runCtx.Done():
		select {
		case <-done:
		case <-time.After(cfg.fadeOutDuration):
			cfg.logger.Warn().Dur("fadeout", cfg.fadeOutDuration).Msg("graceful shutdown: fade-out elapsed")
		}
	}

	var errs []error
	for err := range errsCh {
		cfg.logger.Err(err).Msg("component error")
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		cfg.logger.Info().Msg("graceful shutdown: complete")
		return nil
	}
	return errors.Join(errs...)
}
