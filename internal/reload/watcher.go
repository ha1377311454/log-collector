package reload

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"log-collector/internal/logging"
)

const debounceDelay = 300 * time.Millisecond

// Watcher 监听配置文件所在目录，兼容编辑器通过 rename/create 覆盖原文件的保存方式。
type Watcher struct {
	path     string
	watcher  *fsnotify.Watcher
	log      *logging.Logger
	onChange func()
}

func New(path string, logger *logging.Logger, onChange func()) (*Watcher, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(absolute)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}
	return &Watcher{path: filepath.Clean(absolute), watcher: w, log: logger, onChange: onChange}, nil
}

func (w *Watcher) Close() error { return w.watcher.Close() }

// Run 对同一次保存产生的多个事件防抖，只在目标配置文件稳定后触发一次重载。
func (w *Watcher) Run(ctx context.Context) error {
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				return nil
			}
			if filepath.Clean(event.Name) != w.path || event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(debounceDelay)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounceDelay)
			}
			timerC = timer.C
		case <-timerC:
			timerC = nil
			w.onChange()
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return nil
			}
			w.log.Warn("configuration watcher error", "error", err)
		case <-ctx.Done():
			return nil
		}
	}
}
