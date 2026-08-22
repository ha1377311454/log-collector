package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Position 记录某个 device:inode 当前已读取到的字节偏移。
type Position struct {
	Path      string    `json:"path"`
	Offset    int64     `json:"offset"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store 是线程安全的内存位点表，并负责将快照持久化为 JSON。
type Store struct {
	path       string
	mu         sync.RWMutex
	positions  map[string]Position
	dirty      bool
	generation uint64
}

// Load 从磁盘恢复位点；文件不存在表示首次启动，不视为错误。
func Load(path string) (*Store, error) {
	s := &Store{path: path, positions: make(map[string]Position)}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) != 0 {
		if err := json.Unmarshal(b, &s.positions); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Get 查询指定 device:inode 的读取位点。
func (s *Store) Get(key string) (Position, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.positions[key]
	return p, ok
}

// Set 更新内存位点，磁盘写入由 Save 统一完成。
func (s *Store) Set(key string, p Position) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.positions[key]; ok && old.Offset == p.Offset && old.Path == p.Path {
		return
	}
	s.positions[key] = p
	s.dirty = true
	s.generation++
}

// Cleanup 删除长时间没有更新的历史 inode，防止位点表随轮转文件无限增长。
func (s *Store) Cleanup(olderThan time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, position := range s.positions {
		if position.UpdatedAt.Before(olderThan) {
			// 文件仍存在且 inode 未变化时，它只是长时间没有新日志，不能删除位点。
			// 否则重启后 start_at=beginning 会重复采集，start_at=end 会跳过停机期间内容。
			if info, err := os.Stat(position.Path); err == nil && fileIdentity(info) == key {
				continue
			}
			delete(s.positions, key)
			s.dirty = true
			s.generation++
		}
	}
}

func fileIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return strconv.FormatUint(uint64(stat.Dev), 10) + ":" + strconv.FormatUint(stat.Ino, 10)
}

// SaveIfDirty 只在位点变化后持久化，避免空闲时周期性 fsync。
func (s *Store) SaveIfDirty() error { return s.save(false) }

// Save 强制持久化，用于优雅退出。generation 防止并发 Set 被错误清除 dirty 标记。
func (s *Store) Save() error { return s.save(true) }

func (s *Store) save(force bool) error {
	s.mu.Lock()
	if !force && !s.dirty {
		s.mu.Unlock()
		return nil
	}
	// 状态文件供程序恢复使用，无需缩进；紧凑 JSON 可明显降低大位点表的编码和刷盘量。
	b, err := json.Marshal(s.positions)
	generation := s.generation
	s.mu.Unlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".positions-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	s.mu.Lock()
	if s.generation == generation {
		s.dirty = false
	}
	s.mu.Unlock()
	return nil
}
