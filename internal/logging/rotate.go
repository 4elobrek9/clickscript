package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	file     *os.File
	size     int64
}

func Open(path string, maxBytes int64, backups int) (*RotatingFile, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("maximum log size must be positive")
	}
	if backups < 1 {
		return nil, fmt.Errorf("at least one log backup is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat log file: %w", err)
	}
	return &RotatingFile{path: path, maxBytes: maxBytes, backups: backups, file: file, size: info.Size()}, nil
}

func (w *RotatingFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingFile) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close log before rotation: %w", err)
	}
	for i := w.backups; i > 0; i-- {
		source := w.path
		if i > 1 {
			source = fmt.Sprintf("%s.%d", w.path, i-1)
		}
		if _, err := os.Stat(source); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat log during rotation: %w", err)
		}
		target := fmt.Sprintf("%s.%d", w.path, i)
		if i == w.backups {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove old log backup: %w", err)
			}
		}
		if err := os.Rename(source, target); err != nil {
			return fmt.Errorf("rotate log to %q: %w", target, err)
		}
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("reopen log after rotation: %w", err)
	}
	w.file = file
	w.size = 0
	return nil
}

func (w *RotatingFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	if err != nil {
		return fmt.Errorf("close log file: %w", err)
	}
	return nil
}
