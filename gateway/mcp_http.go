package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// httpTransport реализует JSON-RPC поверх HTTP.
type httpTransport struct {
	cfg       MCPServerConfig
	client    *http.Client
	nextID    int32
	basicAuth string
}

// newHTTPTransport создаёт HTTP-транспорт для MCP-сервера.
func newHTTPTransport(cfg MCPServerConfig) (MCPTransport, error) {
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}
	if cfg.InsecureSkipVerify {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	transport := &httpTransport{
		cfg:    cfg,
		client: httpClient,
		nextID: 1,
	}

	if cfg.Username != "" {
		transport.basicAuth = basicAuthHeader(cfg.Username, cfg.Password)
	}

	return transport, nil
}

// Call выполняет синхронный JSON-RPC вызов через POST с retry при временных сбоях.
func (t *httpTransport) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	id := int(atomic.AddInt32(&t.nextID, 1))

	reqBody := mcpJSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}

		req.Header.Set("Content-Type", "application/json")
		if t.basicAuth != "" {
			req.Header.Set("Authorization", "Basic "+t.basicAuth)
		}

		resp, err := t.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP request failed: %w", err)
			if isRetryableHTTPError(err) && attempt < maxAttempts {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return nil, lastErr
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		if resp.StatusCode >= 500 && attempt < maxAttempts {
			lastErr = fmt.Errorf("HTTP status %d: %s", resp.StatusCode, string(respBody))
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return nil, fmt.Errorf("HTTP status %d: %s", resp.StatusCode, string(respBody))
		}

		if resp.StatusCode == http.StatusNoContent {
			return json.RawMessage("{}"), nil
		}

		var rpcResp mcpJSONRPCResponse
		if err := json.Unmarshal(respBody, &rpcResp); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}

		if rpcResp.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
		}

		return rpcResp.Result, nil
	}

	return nil, lastErr
}

func isRetryableHTTPError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

// Notify отправляет JSON-RPC уведомление через POST и не ожидает ответа.
func (t *httpTransport) Notify(ctx context.Context, method string, params interface{}) error {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(data))
		if err != nil {
			return err
		}

		req.Header.Set("Content-Type", "application/json")
		if t.basicAuth != "" {
			req.Header.Set("Authorization", "Basic "+t.basicAuth)
		}

		resp, err := t.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP notification failed: %w", err)
			if isRetryableHTTPError(err) && attempt < maxAttempts {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return lastErr
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 500 && attempt < maxAttempts {
			lastErr = fmt.Errorf("HTTP notification status %d: %s", resp.StatusCode, string(body))
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		if resp.StatusCode >= 400 {
			return fmt.Errorf("HTTP notification status %d: %s", resp.StatusCode, string(body))
		}

		return nil
	}

	return lastErr
}

// Close закрывает idle-соединения HTTP-клиента.
func (t *httpTransport) Close() error {
	t.client.CloseIdleConnections()
	return nil
}

func basicAuthHeader(username, password string) string {
	creds := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(creds))
}
