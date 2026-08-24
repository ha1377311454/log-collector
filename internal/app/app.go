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
	"log-collector/internal/webhook"
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
	var exp *exporter.Exporter
	if cfg.Export.Enabled {
		exp, err = exporter.New(cfg.Export, logger)
		if err != nil {
			return err
		}
	}
	var hook *webhook.Client
	if cfg.Webhook.Enabled {
		hook, err = webhook.New(cfg.Webhook)
		if err != nil {
			return err
		}
	}
	disc, err := discovery.New(cfg.Sources, cfg.Performance.CacheTTL.Duration, logger)
	if err != nil {
		return err
	}
	submit := func(ctx context.Context, record model.Record) error {
		if hook != nil {
			if err := hook.Send(ctx, record); err != nil {
				if exp == nil {
					return err
				}
				// Webhook 告警故障不应阻断仍然可用的 OTLP 日志链路。
				logger.Warn("send error log to WeChat webhook", "error", err)
			}
		}
		if exp != nil {
			return exp.Enqueue(ctx, record)
		}
		return nil
	}
	limiter := flowcontrol.New(cfg.FlowControl, logger, submit)
	tail := tailer.New(store, cfg.Performance, logger, limiter.Submit)

	loopCtx, stopLoops := context.WithCancel(context.Background())
	exportCtx, stopExporter := context.WithCancel(context.Background())
	defer stopLoops()
	defer stopExporter()
	var exportErr chan error
	if exp != nil {
		exportErr = make(chan error, 1)
		go func() { exportErr <- exp.Run(exportCtx) }()
	}

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
		if exp != nil {
			stopExporter()
			if err := <-exportErr; err != nil && runErr == nil && !errors.Is(err, context.Canceled) {
				runErr = err
			}
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
		// Exporter 已经停止，不能再 flush 多行缓冲，否则会永久阻塞在无人消费的队列；
		// 但仍应保存已经成功进入下游的文件位点，缩小异常退出后的重复采集窗口。
		if saveErr := store.Save(); saveErr != nil && err == nil {
			err = saveErr
		}
		if err == nil {
			return errors.New("exporter stopped unexpectedly")
		}
		return err
	}
}
