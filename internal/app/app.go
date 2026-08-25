package app

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"log-collector/internal/config"
	"log-collector/internal/discovery"
	"log-collector/internal/exporter"
	"log-collector/internal/flowcontrol"
	"log-collector/internal/logging"
	"log-collector/internal/model"
	"log-collector/internal/reload"
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

type recordSubmitter func(context.Context, model.Record) error

// submitRecord 将所有外部输出视为尽力而为；输出失败不能阻断日志采集。
func submitRecord(ctx context.Context, record model.Record, logger *logging.Logger, sendWebhook, sendExport recordSubmitter) error {
	if sendWebhook != nil {
		if err := sendWebhook(ctx, record); err != nil {
			logger.Warn("send error log to WeChat webhook", "error", err)
		}
	}
	if sendExport != nil {
		if err := sendExport(ctx, record); err != nil {
			logger.Warn("enqueue log for OTLP export", "error", err)
		}
	}
	return nil
}

// Run 将发现、读取、位点保存拆成三个独立周期，慢读取不会推迟下一次进程发现。
func Run(ctx context.Context, cfg config.Config, configPath string, logger *logging.Logger) error {
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
	var expRef atomic.Pointer[exporter.Exporter]
	expRef.Store(exp)
	var hookRef atomic.Pointer[webhook.Client]
	if cfg.Webhook.Enabled {
		hook, err := webhook.New(cfg.Webhook, logger)
		if err != nil {
			return err
		}
		hookRef.Store(hook)
	}
	disc, err := discovery.New(cfg.Sources, cfg.Performance.CacheTTL.Duration, logger)
	if err != nil {
		return err
	}
	loopCtx, stopLoops := context.WithCancel(context.Background())
	exportCtx, stopExporter := context.WithCancel(context.Background())
	defer stopLoops()
	defer stopExporter()
	submit := func(ctx context.Context, record model.Record) error {
		var sendWebhook recordSubmitter
		if hook := hookRef.Load(); hook != nil {
			sendWebhook = hook.Send
		}
		var sendExport recordSubmitter
		if activeExp := expRef.Load(); activeExp != nil {
			sendExport = func(ctx context.Context, record model.Record) error {
				enqueueCtx, cancel := context.WithCancel(ctx)
				stop := context.AfterFunc(exportCtx, cancel)
				defer stop()
				defer cancel()
				return activeExp.Enqueue(enqueueCtx, record)
			}
		}
		return submitRecord(ctx, record, logger, sendWebhook, sendExport)
	}
	limiter := flowcontrol.New(cfg.FlowControl, logger, submit)
	tail := tailer.New(store, cfg.Performance, logger, limiter.Submit)
	activeCfg := cfg
	reloadConfig := func() {
		next, err := config.Load(configPath)
		if err != nil {
			logger.Warn("configuration hot reload rejected", "error", err)
			return
		}
		restartChanges := restartRequiredChanges(activeCfg, next)
		webhookChanged := !reflect.DeepEqual(activeCfg.Webhook, next.Webhook)
		levelChanged := activeCfg.Log.Level != next.Log.Level
		var nextHook *webhook.Client
		if webhookChanged && next.Webhook.Enabled {
			nextHook, err = webhook.New(next.Webhook, logger)
			if err != nil {
				logger.Warn("configuration hot reload rejected", "error", err)
				return
			}
		}
		if levelChanged {
			if err := logger.SetLevel(next.Log.Level); err != nil {
				logger.Warn("configuration hot reload rejected", "error", err)
				return
			}
		}
		if webhookChanged {
			hookRef.Store(nextHook)
		}
		activeCfg.Webhook = next.Webhook
		activeCfg.Log.Level = next.Log.Level
		if webhookChanged || levelChanged {
			logger.Info("configuration hot reload applied",
				"log_level", next.Log.Level,
				"wechat_webhook_enabled", next.Webhook.Enabled,
				"ignore_keyword_count", len(next.Webhook.IgnoreKeywords),
				"error_type_keyword_count", len(next.Webhook.ErrorTypeKeywords),
			)
		}
		if len(restartChanges) > 0 {
			logger.Warn("configuration changes require restart", "sections", restartChanges)
		}
	}
	configWatcher, err := reload.New(configPath, logger, reloadConfig)
	if err != nil {
		return err
	}
	defer configWatcher.Close()

	var exportErr chan error
	if exp != nil {
		exportErr = make(chan error, 1)
		go func() {
			err := exp.Run(exportCtx)
			// 解除所有可能阻塞在已停止 Exporter 队列上的 Enqueue 调用。
			stopExporter()
			exportErr <- err
		}()
	}

	initial, discoverErr := disc.Discover(loopCtx)
	if discoverErr != nil {
		logger.Warn("initial discovery completed with errors", "error", discoverErr)
	}
	targets := &targetStore{}
	targets.Set(initial)
	errCh := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := configWatcher.Run(loopCtx); err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}()
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
			if exportErr != nil {
				if err := <-exportErr; err != nil && !errors.Is(err, context.Canceled) {
					logger.Warn("OTLP exporter stopped during shutdown", "error", err)
				}
			}
		}
		return runErr
	}
	for {
		select {
		case <-ctx.Done():
			return shutdown(nil)
		case err := <-errCh:
			return shutdown(err)
		case err := <-exportErr:
			// Run 返回后队列已无人消费，先禁用提交，避免采集线程在满队列上永久阻塞。
			expRef.Store(nil)
			exportErr = nil
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn("OTLP exporter stopped; continuing without OTLP export", "error", err)
			} else {
				logger.Warn("OTLP exporter stopped unexpectedly; continuing without OTLP export")
			}
		}
	}
}

func restartRequiredChanges(active, next config.Config) []string {
	var changes []string
	activeLog, nextLog := active.Log, next.Log
	activeLog.Level, nextLog.Level = "", ""
	for _, section := range []struct {
		name   string
		active any
		next   any
	}{
		{name: "log except level", active: activeLog, next: nextLog},
		{name: "state", active: active.State, next: next.State},
		{name: "sources", active: active.Sources, next: next.Sources},
		{name: "export", active: active.Export, next: next.Export},
		{name: "performance", active: active.Performance, next: next.Performance},
		{name: "flow_control", active: active.FlowControl, next: next.FlowControl},
	} {
		if !reflect.DeepEqual(section.active, section.next) {
			changes = append(changes, section.name)
		}
	}
	return changes
}
