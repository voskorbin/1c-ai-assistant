package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// StreamState хранит состояние потоковой генерации.
type StreamState struct {
	RequestID      string
	Done           bool
	Stopped        bool
	Text           string
	Error          string
	Version        int
	CreatedAt      time.Time
	LastAccessedAt time.Time
	cancel         context.CancelFunc
	mu             sync.RWMutex
}

// StreamStore хранит активные и завершённые потоковые запросы.
type StreamStore struct {
	mu          sync.RWMutex
	states      map[string]*StreamState
	stopCleanup chan struct{}
}

// NewStreamStore создаёт новое хранилище состояний.
func NewStreamStore() *StreamStore {
	return &StreamStore{
		states:      make(map[string]*StreamState),
		stopCleanup: make(chan struct{}),
	}
}

// Create создаёт новое состояние запроса.
func (s *StreamStore) Create(requestID string, cancel context.CancelFunc) *StreamState {
	now := time.Now()
	state := &StreamState{
		RequestID:      requestID,
		CreatedAt:      now,
		LastAccessedAt: now,
		cancel:         cancel,
	}

	s.mu.Lock()
	s.states[requestID] = state
	s.mu.Unlock()

	return state
}

// Stop отменяет контекст запроса и помечает его как остановленный пользователем.
func (st *StreamState) Stop() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cancel != nil {
		st.cancel()
	}
	st.Stopped = true
}

// IsStopped возвращает true, если запрос был остановлен пользователем.
func (st *StreamState) IsStopped() bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.Stopped
}

// Get возвращает состояние запроса по идентификатору.
func (s *StreamStore) Get(requestID string) (*StreamState, bool) {
	s.mu.RLock()
	state, ok := s.states[requestID]
	s.mu.RUnlock()

	if ok && state != nil {
		state.mu.Lock()
		state.LastAccessedAt = time.Now()
		state.mu.Unlock()
	}

	return state, ok
}

// AppendText добавляет текст к накопленному ответу.
func (st *StreamState) AppendText(text string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.Text += text
	st.Version++
}

// SetDone помечает запрос как завершённый.
func (st *StreamState) SetDone() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.Done = true
}

// CleanText применяет функцию очистки к накопленному тексту.
func (st *StreamState) CleanText(clean func(string) string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.Text = clean(st.Text)
	st.Version++
}

// SetError сохраняет ошибку и помечает запрос как завершённый.
func (st *StreamState) SetError(err string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.Error = err
	st.Done = true
}

// Snapshot возвращает актуальную копию состояния.
func (st *StreamState) Snapshot() StreamState {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.LastAccessedAt = time.Now()
	return StreamState{
		RequestID:      st.RequestID,
		Done:           st.Done,
		Text:           st.Text,
		Error:          st.Error,
		Version:        st.Version,
		CreatedAt:      st.CreatedAt,
		LastAccessedAt: st.LastAccessedAt,
	}
}

// StartCleanup запускает фоновую очистку устаревших состояний.
func (s *StreamStore) StartCleanup(interval, ttl time.Duration) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[StreamStore] panic in cleanup goroutine: %v", r)
			}
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.cleanup(ttl)
			case <-s.stopCleanup:
				return
			}
		}
	}()
}

// Stop останавливает фоновую очистку состояний.
func (s *StreamStore) Stop() {
	close(s.stopCleanup)
}

func (s *StreamStore) cleanup(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)

	s.mu.Lock()
	defer s.mu.Unlock()

	for id, state := range s.states {
		state.mu.RLock()
		lastAccess := state.LastAccessedAt
		cancel := state.cancel
		state.mu.RUnlock()

		if lastAccess.Before(cutoff) {
			if cancel != nil {
				cancel()
			}
			delete(s.states, id)
		}
	}
}
