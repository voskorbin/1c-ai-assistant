package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
)

// stdioTransport реализует JSON-RPC поверх stdin/stdout дочернего процесса.
type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	nextID int32
}

type stdioCallResult struct {
	resp json.RawMessage
	err  error
}

// newStdioTransport запускает MCP-сервер как дочерний процесс.
func newStdioTransport(cfg MCPServerConfig) (MCPTransport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)

	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start MCP server: %w", err)
	}

	return &stdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		nextID: 1,
	}, nil
}

// Call выполняет синхронный JSON-RPC вызов.
func (t *stdioTransport) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	done := make(chan stdioCallResult, 1)

	go func() {
		done <- t.callNoCtx(method, params)
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		return r.resp, r.err
	}
}

func (t *stdioTransport) callNoCtx(method string, params interface{}) stdioCallResult {
	id := int(atomic.AddInt32(&t.nextID, 1))

	req := mcpJSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return stdioCallResult{err: err}
	}

	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		return stdioCallResult{err: fmt.Errorf("failed to write request: %w", err)}
	}

	for {
		line, err := t.stdout.ReadString('\n')
		if err != nil {
			return stdioCallResult{err: fmt.Errorf("failed to read response: %w", err)}
		}

		line = trimNewline(line)
		if line == "" {
			continue
		}

		var resp mcpJSONRPCResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}

		if resp.ID != id {
			continue
		}

		if resp.Error != nil {
			return stdioCallResult{err: fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)}
		}

		return stdioCallResult{resp: resp.Result}
	}
}

// Notify отправляет JSON-RPC уведомление (без id) серверу.
func (t *stdioTransport) Notify(ctx context.Context, method string, params interface{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write notification: %w", err)
	}

	return nil
}

// Close завершает дочерний процесс и его дерево (Windows).
func (t *stdioTransport) Close() error {
	_ = t.stdin.Close()

	if t.cmd.Process == nil {
		return nil
	}

	// Пытаемся завершить дерево процессов (актуально для Windows).
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(t.cmd.Process.Pid)).Run()
	} else {
		_ = t.cmd.Process.Kill()
	}

	// Ждём завершения, но не возвращаем ошибку, если процесс уже завершился.
	_ = t.cmd.Wait()
	return nil
}
