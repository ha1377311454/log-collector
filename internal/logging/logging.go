package logging

import (
	"errors"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"

	"log-collector/internal/config"
)

// Logger 封装 zap，保留统一键值对接口，避免底层日志框架渗透到业务模块。
type Logger struct {
	base   *zap.Logger
	roller *lumberjack.Logger
	level  zap.AtomicLevel
}

// New 创建结构化日志器；lumberjack 负责按大小滚动、保留和压缩历史文件。
func New(cfg config.LogConfig) (*Logger, error) {
	level := zapcore.InfoLevel
	if err := level.Set(cfg.Level); err != nil {
		return nil, err
	}
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "time"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encoderCfg.EncodeLevel = zapcore.LowercaseLevelEncoder
	var encoder zapcore.Encoder
	if cfg.Format == "console" {
		encoder = zapcore.NewConsoleEncoder(encoderCfg)
	} else {
		encoder = zapcore.NewJSONEncoder(encoderCfg)
	}

	logger := &Logger{}
	var sinks []zapcore.WriteSyncer
	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0o755); err != nil {
			return nil, err
		}
		logger.roller = &lumberjack.Logger{Filename: cfg.File, MaxSize: cfg.MaxSizeMB, MaxBackups: cfg.MaxBackups, MaxAge: cfg.MaxAgeDays, Compress: cfg.Compress, LocalTime: true}
		sinks = append(sinks, zapcore.AddSync(logger.roller))
	}
	if cfg.AlsoStdout {
		sinks = append(sinks, zapcore.AddSync(os.Stdout))
	}
	if len(sinks) == 0 {
		return nil, errors.New("no log output configured")
	}
	logger.level = zap.NewAtomicLevelAt(level)
	logger.base = zap.New(zapcore.NewCore(encoder, zapcore.NewMultiWriteSyncer(sinks...), logger.level), zap.AddCaller(), zap.AddCallerSkip(1))
	return logger, nil
}

// Nop 返回无输出日志器，供测试使用。
func Nop() *Logger { return &Logger{base: zap.NewNop(), level: zap.NewAtomicLevel()} }

// SetLevel 原子更新运行日志级别，已经创建的 Logger 无需重建。
func (l *Logger) SetLevel(value string) error {
	var level zapcore.Level
	if err := level.Set(value); err != nil {
		return err
	}
	l.level.SetLevel(level)
	return nil
}

func (l *Logger) Debug(message string, args ...any) { l.base.Debug(message, fields(args)...) }
func (l *Logger) Info(message string, args ...any)  { l.base.Info(message, fields(args)...) }
func (l *Logger) Warn(message string, args ...any)  { l.base.Warn(message, fields(args)...) }
func (l *Logger) Error(message string, args ...any) { l.base.Error(message, fields(args)...) }

// Close 刷新 zap 并关闭滚动文件。stdout 的 Sync 错误不会阻止正常退出。
func (l *Logger) Close() error {
	_ = l.base.Sync()
	if l.roller != nil {
		return l.roller.Close()
	}
	return nil
}

func fields(args []any) []zap.Field {
	result := make([]zap.Field, 0, (len(args)+1)/2)
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			result = append(result, zap.Any("extra", args[i]))
			break
		}
		key, ok := args[i].(string)
		if !ok {
			key = "field"
		}
		result = append(result, zap.Any(key, args[i+1]))
	}
	return result
}
