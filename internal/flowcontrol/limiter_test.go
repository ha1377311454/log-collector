package flowcontrol

import (
	"context"
	"errors"
	"testing"
	"time"

	"log-collector/internal/config"
	"log-collector/internal/logging"
	"log-collector/internal/model"
)

func TestDropModeDropsAfterBurstExhausted(t *testing.T) {
	accepted := 0
	limiter := New(config.FlowControlConfig{Enabled: true, RatePerSecond: 0.001, Burst: 1, Mode: "drop", ReportInterval: config.Duration{Duration: time.Hour}}, logging.Nop(), func(context.Context, model.Record) error { accepted++; return nil })
	if err := limiter.Submit(context.Background(), model.Record{}); err != nil {
		t.Fatal(err)
	}
	if err := limiter.Submit(context.Background(), model.Record{}); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 {
		t.Fatalf("accepted = %d, want 1", accepted)
	}
}

func TestBlockModeHonorsContextCancellation(t *testing.T) {
	limiter := New(config.FlowControlConfig{Enabled: true, RatePerSecond: 0.001, Burst: 1, Mode: "block", ReportInterval: config.Duration{Duration: time.Hour}}, logging.Nop(), func(context.Context, model.Record) error { return nil })
	if err := limiter.Submit(context.Background(), model.Record{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Submit(ctx, model.Record{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestDisabledLimiterPassesThrough(t *testing.T) {
	accepted := 0
	limiter := New(config.FlowControlConfig{}, logging.Nop(), func(context.Context, model.Record) error { accepted++; return nil })
	for range 3 {
		if err := limiter.Submit(context.Background(), model.Record{}); err != nil {
			t.Fatal(err)
		}
	}
	if accepted != 3 {
		t.Fatalf("accepted = %d, want 3", accepted)
	}
}
