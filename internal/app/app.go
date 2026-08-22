package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"log-collector/internal/config"
	"log-collector/internal/discovery"
	"log-collector/internal/exporter"
	"log-collector/internal/flowcontrol"
	"log-collector/internal/logging"
	"log-collector/internal/model"
	"log-collector/internal/state"
	"log-collector/internal/tailer"
)

type targetStore struct {
	mu      sync.RWMutex
	targets []model.FileTarget
}

func (s *targetStore) Set(targets []model.FileTarget) {
	s.mu.Lock()
	s.targets = targets
	s.mu.Unlock()
}
func (s *targetStore) Get() []model.FileTarget {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]model.FileTarget(nil), s.targets...)
}

// Run 将发现、读取、位点保存拆成三个独立周期，慢读取不会推迟下一次进程发现。
func Run(ctx context.Context, cfg config.Config, logger *logging.Logger) error {
	store, err := state.Load(cfg.State.Path)
	if err != nil {
		return err
	}
	exp, err := exporter.New(cfg.Export, logger)
	if err != nil {
		return err
	}
	disc, err := discovery.New(cfg.Sources, cfg.Performance.CacheTTL.Duration, logger)
	if err != nil {
		return err
	}
	limiter := flowcontrol.New(cfg.FlowControl, logger, exp.Enqueue)
	tail := tailer.New(store, cfg.Performance, logger, limiter.Submit)

	loopCtx, stopLoops := context.WithCancel(context.Background())
	exportCtx, stopExporter := context.WithCancel(context.Background())
	defer stopLoops()
	defer stopExporter()
	exportErr := make(chan error, 1)
	go func() { exportErr <- exp.Run(exportCtx) }()

	initial, discoverErr := disc.Discover(loopCtx)
	if discoverErr != nil {
		logger.Warn("initial discovery completed with errors", "error", discoverErr)
	}
	targets := &targetStore{}
	targets.Set(initial)
	errCh := make(chan error, 3)
	var wg sync.WaitGroup
	startLoop := func(interval time.Duration, action func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := action(); err != nil {
						select {
						case errCh <- err:
						default:
						}
						return
					}
				case <-loopCtx.Done():
					return
				}
			}
		}()
	}
	startLoop(cfg.Performance.DiscoveryInterval.Duration, func() error {
		found, err := disc.Discover(loopCtx)
		if err != nil {
			logger.Warn("discovery completed with errors", "error", err)
		}
		targets.Set(found)
		return nil
	})
	startLoop(cfg.Performance.ReadInterval.Duration, func() error {
		err := tail.Poll(loopCtx, targets.Get())
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	startLoop(cfg.Performance.PositionFlushInterval.Duration, func() error {
		store.Cleanup(time.Now().Add(-cfg.Performance.CacheTTL.Duration))
		return store.SaveIfDirty()
	})

	shutdown := func(runErr error) error {
		stopLoops()
		wg.Wait()
		if err := tail.Close(exportCtx); err != nil && runErr == nil {
			runErr = err
		}
		stopExporter()
		if err := <-exportErr; err != nil && runErr == nil && !errors.Is(err, context.Canceled) {
			runErr = err
		}
		return runErr
	}
	select {
	case <-ctx.Done():
		return shutdown(nil)
	case err := <-errCh:
		return shutdown(err)
	case err := <-exportErr:
		stopLoops()
		wg.Wait()
		if err == nil {
			return errors.New("exporter stopped unexpectedly")
		}
		return err
	}
}
