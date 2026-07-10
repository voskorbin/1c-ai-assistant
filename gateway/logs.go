package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// logRingBuffer хранит последние N строк логов в памяти.
type logRingBuffer struct {
	mu      sync.RWMutex
	lines   []string
	size    int
	writer  io.Writer
}

// newLogRingBuffer создаёт буфер заданного размера.
func newLogRingBuffer(size int) *logRingBuffer {
	return &logRingBuffer{
		lines: make([]string, 0, size),
		size:  size,
	}
}

// Write добавляет строку в буфер.
func (b *logRingBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	scanner := bufio.NewScanner(bytes.NewReader(p))
	for scanner.Scan() {
		b.lines = append(b.lines, scanner.Text())
		if len(b.lines) > b.size {
			b.lines = b.lines[len(b.lines)-b.size:]
		}
	}
	return len(p), nil
}

// Snapshot возвращает копию строк, отфильтрованных по уровню.
func (b *logRingBuffer) Snapshot(level string, limit int) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	level = strings.ToLower(strings.TrimSpace(level))
	result := make([]string, 0, len(b.lines))
	for _, line := range b.lines {
		if level != "" && !strings.Contains(strings.ToLower(line), level) {
			continue
		}
		result = append(result, line)
	}

	if limit > 0 && len(result) > limit {
		result = result[len(result)-limit:]
	}

	return result
}

// handleLogs возвращает последние логи в plain text.
func (h *Handlers) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limitStr := r.URL.Query().Get("limit")
	level := r.URL.Query().Get("level")

	limit := 1000
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}

	lines := logBuffer.Snapshot(level, limit)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}
