//go:build windows

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// rotatingWriter 是按大小轮转的文件日志。
//
// 服务模式没有控制台，日志文件是唯一的现场证据；不轮转会随长期运行无限增长，
// 因此在每次写入后检查大小并保留固定份数。
type rotatingWriter struct {
	path string
	max  int64
	keep int

	mu   sync.Mutex
	file *os.File
	size int64
}

func newRotatingWriter(path string, max int64, keep int) (*rotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("创建日志目录: %w", err)
	}
	writer := &rotatingWriter{path: path, max: max, keep: keep}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *rotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("打开日志文件 %s: %w", w.path, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	// 先轮转再写，保证当前日志不会在轮转后留下一个空文件，排查问题时能直接看到最新一条。
	if w.size > 0 && w.size+int64(len(p)) > w.max {
		w.rotate()
		if w.file == nil {
			if err := w.open(); err != nil {
				return 0, err
			}
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close 释放文件句柄；服务退出时由进程结束回收，测试与手工验证需要显式关闭。
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// rotate 关闭当前文件并按 .1、.2 … 递增改名；最旧一份直接覆盖。
func (w *rotatingWriter) rotate() {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}
	for index := w.keep - 1; index >= 1; index-- {
		from := fmt.Sprintf("%s.%d", w.path, index)
		to := fmt.Sprintf("%s.%d", w.path, index+1)
		if _, err := os.Stat(from); err == nil {
			_ = os.Remove(to)
			_ = os.Rename(from, to)
		}
	}
	_ = os.Rename(w.path, w.path+".1")
	_ = w.open()
}

// attachServiceLog 把标准库日志接到控制台与服务日志文件。
func attachServiceLog(dir string) error {
	writer, err := newRotatingWriter(filepath.Join(dir, "dbbak.log"), 10<<20, 5)
	if err != nil {
		return err
	}
	log.SetOutput(io.MultiWriter(os.Stderr, writer))
	log.SetFlags(log.LstdFlags)
	return nil
}
