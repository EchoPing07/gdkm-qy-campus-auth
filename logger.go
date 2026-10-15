package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	levelDebug = iota
	levelInfo
	levelError
)

func levelNum(s string) int {
	switch s {
	case "debug":
		return levelDebug
	case "error":
		return levelError
	default:
		return levelInfo
	}
}

func levelTag(n int) string {
	switch n {
	case levelDebug:
		return "DEBUG"
	case levelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// Logger stdout + 文件双写；单文件超 max_mb 滚动为 .old，只保留 1 份。
type Logger struct {
	mu       sync.Mutex
	level    int
	path     string // 绝对路径
	maxBytes int64
	file     *os.File
	size     int64
}

func NewLogger() *Logger {
	return &Logger{level: levelInfo, maxBytes: defaultLogMaxMB << 20}
}

// Reconfigure 调整级别 / 文件 / 滚动上限；相对路径基于 baseDir（二进制所在目录）解析。
func (l *Logger) Reconfigure(level, file string, maxMB int, baseDir string) error {
	p := file
	if p == "" {
		p = defaultLogFile
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(baseDir, p)
	}
	mb := int64(maxMB)
	if mb <= 0 {
		mb = defaultLogMaxMB
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil && p != l.path {
		l.file.Close()
		l.file = nil
		l.size = 0
	}
	l.level = levelNum(level)
	l.path = p
	l.maxBytes = mb << 20
	if l.file == nil {
		return l.openLocked()
	}
	return nil
}

func (l *Logger) openLocked() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.file = f
	l.size = st.Size()
	return nil
}

func (l *Logger) write(lv int, ifkey, msg string) {
	line := fmt.Sprintf("%s [%s] [%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), levelTag(lv), ifkey, msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	if lv < l.level {
		return
	}
	os.Stdout.WriteString(line)
	if l.maxBytes > 0 && l.size+int64(len(line)) > l.maxBytes {
		l.rotateLocked()
	}
	if l.file == nil {
		if err := l.openLocked(); err != nil {
			return // 文件不可用时仅保 stdout
		}
	}
	n, err := l.file.WriteString(line)
	l.size += int64(n)
	if err != nil {
		l.file.Close()
		l.file = nil
	}
}

func (l *Logger) rotateLocked() {
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	old := l.path + ".old"
	os.Remove(old) // Windows 下 Rename 不能覆盖已存在文件
	os.Rename(l.path, old)
	l.size = 0
}

func (l *Logger) Debug(ifkey, format string, a ...any) {
	l.write(levelDebug, ifkey, fmt.Sprintf(format, a...))
}
func (l *Logger) Info(ifkey, format string, a ...any) {
	l.write(levelInfo, ifkey, fmt.Sprintf(format, a...))
}
func (l *Logger) Error(ifkey, format string, a ...any) {
	l.write(levelError, ifkey, fmt.Sprintf(format, a...))
}
