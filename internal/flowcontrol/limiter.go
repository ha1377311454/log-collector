package flowcontrol

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"log-collector/internal/config"
	"log-collector/internal/logging"
	"log-collector/internal/model"
)

// SubmitFunc 是流控下游接口，通常指向 Exporter.Enqueue。
type SubmitFunc func(context.Context, model.Record) error

// Limiter 使用 x/time/rate 的令牌桶限制进入 Exporter 队列的全局日志速率。
// block 模式等待令牌；drop 模式立即丢弃没有令牌的记录。
type Limiter struct {
	cfg     config.FlowControlConfig
	next    SubmitFunc
	log     *logging.Logger
	limiter *rate.Limiter

	reportMu   sync.Mutex
	dropped    uint64
	lastReport time.Time
}

func New(cfg config.FlowControlConfig, logger *logging.Logger, next SubmitFunc) *Limiter {
	l := &Limiter{cfg: cfg, next: next, log: logger, lastReport: time.Now()}
	if cfg.Enabled {
		l.limiter = rate.NewLimiter(rate.Limit(cfg.RatePerSecond), cfg.Burst)
	}
	return l
}

// Submit 在流控关闭时直接透传。block 使用 Wait 正确处理排队与 Context 取消，
// drop 使用 Allow 进行无等待判断，并按时间窗口汇总丢弃数量。
func (l *Limiter) Submit(ctx context.Context, record model.Record) error {
	if !l.cfg.Enabled {
		return l.next(ctx, record)
	}
	if l.cfg.Mode == "drop" {
		if !l.limiter.Allow() {
			l.recordDrop(time.Now())
			return nil
		}
	} else if err := l.limiter.Wait(ctx); err != nil {
		return err
	}
	return l.next(ctx, record)
}

func (l *Limiter) recordDrop(now time.Time) {
	l.reportMu.Lock()
	l.dropped++
	if now.Sub(l.lastReport) < l.cfg.ReportInterval.Duration {
		l.reportMu.Unlock()
		return
	}
	dropped := l.dropped
	l.dropped = 0
	l.lastReport = now
	l.reportMu.Unlock()
	l.log.Warn("logs dropped by flow control", "dropped", dropped, "rate_per_second", l.cfg.RatePerSecond, "burst", l.cfg.Burst)
}
