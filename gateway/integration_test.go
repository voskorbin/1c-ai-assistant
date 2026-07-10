package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// mockLLMServer имитирует LLM-провайдер для тестирования шлюза.
type mockLLMServer struct {
	server *http.Server
	url    string
}

func newMockLLMServer() (*mockLLMServer, error) {
	mux := http.NewServeMux()
	m := &mockLLMServer{}
	mux.HandleFunc("/v1/chat/completions", m.handleChat)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	m.server = &http.Server{Handler: mux}
	m.url = "http://" + listener.Addr().String()

	go func() {
		_ = m.server.Serve(listener)
	}()

	return m, nil
}

func (m *mockLLMServer) handleChat(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		chunks := []string{"Привет", ", ", "это", " ", "тестовый", " ", "ответ", "."}
		for _, chunk := range chunks {
			event := StreamEvent{
				Choices: []StreamChoice{{
					Delta: StreamDelta{Content: chunk},
				}},
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	resp := ChatResponse{
		Choices: []ChatResponseChoice{{
			Message: ChatResponseMessage{
				Role:    "assistant",
				Content: "Привет, это тестовый ответ.",
			},
			FinishReason: "stop",
		}},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (m *mockLLMServer) shutdown(ctx context.Context) error {
	return m.server.Shutdown(ctx)
}

func newTestGateway(mockLLMURL string) (*http.Server, string, error) {
	cfg := &AppConfig{
		Server: ServerConfig{
			Host:                  "127.0.0.1",
			Port:                  0,
			StreamStoreTTLSeconds: 3600,
		},
		LLM: LLMConfig{
			URL:                   mockLLMURL + "/v1/chat/completions",
			Model:                 "test-model",
			APIKeyEnv:             "LLM_API_KEY",
			TimeoutSeconds:        30,
			MaxTokens:             1024,
			Temperature:         0.3,
			MaxConcurrentRequests: 10,
		},
		Config: GatewayConfig{
			Model:       "test-model",
			Temperature: 0.3,
		},
	}

	llm := NewLLMClient(cfg)
	store := NewStreamStore()
	store.StartCleanup(5*time.Minute, time.Duration(cfg.Server.StreamStoreTTLSeconds)*time.Second)
	mcpManager := NewMCPManager(cfg)

	handlers := NewHandlers(cfg, llm, store, mcpManager)
	mux := http.NewServeMux()
	handlers.RegisterRoutes(mux)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}

	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(listener)
	}()

	return server, "http://" + listener.Addr().String(), nil
}

func TestGatewayHealth(t *testing.T) {
	mock, err := newMockLLMServer()
	if err != nil {
		t.Fatalf("failed to start mock LLM: %v", err)
	}
	defer mock.shutdown(context.Background())

	server, gatewayURL, err := newTestGateway(mock.url)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer server.Shutdown(context.Background())

	resp, err := http.Get(gatewayURL + "/health")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected health status: %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("unexpected health body: %s", string(body))
	}
}

func TestGatewayChat(t *testing.T) {
	mock, err := newMockLLMServer()
	if err != nil {
		t.Fatalf("failed to start mock LLM: %v", err)
	}
	defer mock.shutdown(context.Background())

	server, gatewayURL, err := newTestGateway(mock.url)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer server.Shutdown(context.Background())

	payload := ChatPayload{
		Question: "Привет",
		Messages: []ChatMessage{},
	}
	data, _ := json.Marshal(payload)

	resp, err := http.Post(gatewayURL+"/chat", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("chat request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected chat status: %d", resp.StatusCode)
	}

	var result ChatResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode chat response: %v", err)
	}

	if !result.Success {
		t.Fatalf("chat failed: %s", result.Error)
	}

	if !strings.Contains(result.Answer, "тестовый ответ") {
		t.Fatalf("unexpected answer: %s", result.Answer)
	}
}

func TestGatewayStream(t *testing.T) {
	mock, err := newMockLLMServer()
	if err != nil {
		t.Fatalf("failed to start mock LLM: %v", err)
	}
	defer mock.shutdown(context.Background())

	server, gatewayURL, err := newTestGateway(mock.url)
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer server.Shutdown(context.Background())

	payload := ChatPayload{
		Question: "Привет",
		Messages: []ChatMessage{},
	}
	data, _ := json.Marshal(payload)

	resp, err := http.Post(gatewayURL+"/chat/stream", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("stream start request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected stream start status: %d, body: %s", resp.StatusCode, string(body))
	}

	var startResult StreamStartResult
	if err := json.NewDecoder(resp.Body).Decode(&startResult); err != nil {
		t.Fatalf("failed to decode stream start response: %v", err)
	}

	if !startResult.Success {
		t.Fatalf("stream start failed: %s", startResult.Error)
	}

	var statusResult StreamStatusResult
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(gatewayURL + "/chat/status/" + startResult.RequestID)
		if err != nil {
			t.Fatalf("status request failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err := json.Unmarshal(body, &statusResult); err != nil {
			t.Fatalf("failed to decode status: %v", err)
		}

		if statusResult.Done {
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	if !statusResult.Done {
		t.Fatal("stream did not complete in time")
	}

	if !strings.Contains(statusResult.Answer, "тестовый ответ") {
		t.Fatalf("unexpected stream answer: %s", statusResult.Answer)
	}
}
