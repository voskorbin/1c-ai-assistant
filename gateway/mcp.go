package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// MCP JSON-RPC base types.
type mcpJSONRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type mcpJSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpJSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *mcpJSONRPCError `json:"error"`
}

// MCPTool описывает инструмент, полученный от MCP-сервера.
type MCPTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
	Server      string                 `json:"server"`
}

// MCPResourceContent описывает содержимое ресурса.
type MCPResourceContent struct {
	URI  string `json:"uri"`
	Text string `json:"text"`
	Type string `json:"type"`
}

// MCPTransport — интерфейс транспорта для MCP-сервера.
type MCPTransport interface {
	Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error)
	Notify(ctx context.Context, method string, params interface{}) error
	Close() error
}

// resourceCacheEntry хранит закэшированные MCP-ресурсы.
type resourceCacheEntry struct {
	resources []MCPResourceContent
	fetchedAt time.Time
}

// toolsCacheEntry хранит закэшированный список инструментов MCP.
type toolsCacheEntry struct {
	tools     []MCPTool
	fetchedAt time.Time
}

// MCPManager управляет подключениями к MCP-серверам.
type MCPManager struct {
	cfg                *AppConfig
	transports         map[string]MCPTransport
	mu                 sync.RWMutex
	nextID             int
	resourceCache      *resourceCacheEntry
	resourceCacheMu    sync.RWMutex
	resourceCacheTTL   time.Duration
	resourceSingleflight singleflight.Group
	toolsCache         map[string]toolsCacheEntry
	toolsCacheMu       sync.RWMutex
	toolsCacheTTL      time.Duration
	toolsSingleflight  singleflight.Group
}

// NewMCPManager создаёт менеджер MCP.
func NewMCPManager(cfg *AppConfig) *MCPManager {
	return &MCPManager{
		cfg:              cfg,
		transports:       make(map[string]MCPTransport),
		nextID:           1,
		resourceCacheTTL: 5 * time.Minute,
		toolsCache:       make(map[string]toolsCacheEntry),
		toolsCacheTTL:    15 * time.Minute,
	}
}

// Initialize подключается ко всем настроенным MCP-серверам.
func (m *MCPManager) Initialize(ctx context.Context) error {
	initialized := make([]string, 0, len(m.cfg.MCPServers))
	for _, serverCfg := range m.cfg.MCPServers {
		if !serverCfg.Enabled {
			continue
		}

		transport, err := m.createTransport(serverCfg)
		if err != nil {
			m.closeTransports(initialized)
			return fmt.Errorf("failed to create transport for MCP server %s: %w", serverCfg.Name, err)
		}

		if _, err := transport.Call(ctx, "initialize", map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]interface{}{
				"name":    "ai-assistant-gateway",
				"version": "1.0.0",
			},
		}); err != nil {
			_ = transport.Close()
			m.closeTransports(initialized)
			return fmt.Errorf("failed to initialize MCP server %s: %w", serverCfg.Name, err)
		}

		if err := transport.Notify(ctx, "notifications/initialized", map[string]interface{}{}); err != nil {
			_ = transport.Close()
			m.closeTransports(initialized)
			return fmt.Errorf("failed to send initialized notification to MCP server %s: %w", serverCfg.Name, err)
		}

		m.mu.Lock()
		m.transports[serverCfg.Name] = transport
		m.mu.Unlock()
		initialized = append(initialized, serverCfg.Name)
	}

	return nil
}

func (m *MCPManager) closeTransports(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range names {
		if transport, ok := m.transports[name]; ok {
			_ = transport.Close()
			delete(m.transports, name)
		}
	}
}

// Healthy возвращает true, если все включённые MCP-серверы инициализированы.
func (m *MCPManager) Healthy() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, cfg := range m.cfg.MCPServers {
		if !cfg.Enabled {
			continue
		}
		if _, ok := m.transports[cfg.Name]; !ok {
			return false
		}
	}

	return true
}

// Close закрывает все транспорты.
func (m *MCPManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var lastErr error
	for name, transport := range m.transports {
		if err := transport.Close(); err != nil {
			lastErr = fmt.Errorf("failed to close MCP server %s: %w", name, err)
		}
	}

	return lastErr
}

// FetchResources подгружает фиксированные MCP-ресурсы с кэшированием.
func (m *MCPManager) FetchResources(ctx context.Context) ([]MCPResourceContent, error) {
	start := time.Now()
	m.resourceCacheMu.RLock()
	entry := m.resourceCache
	m.resourceCacheMu.RUnlock()
	if entry != nil && time.Since(entry.fetchedAt) < m.resourceCacheTTL {
		log.Printf("[MCPManager] FetchResources cache hit (took %v)", time.Since(start))
		return entry.resources, nil
	}

	result, err, _ := m.resourceSingleflight.Do("resources", func() (interface{}, error) {
		fetched, fetchErr := m.fetchResourcesUncached(ctx)
		if fetchErr == nil {
			m.resourceCacheMu.Lock()
			m.resourceCache = &resourceCacheEntry{resources: fetched, fetchedAt: time.Now()}
			m.resourceCacheMu.Unlock()
		}
		return fetched, fetchErr
	})
	if err != nil {
		return nil, err
	}

	log.Printf("[MCPManager] FetchResources resources=%d, total=%v", len(result.([]MCPResourceContent)), time.Since(start))
	return result.([]MCPResourceContent), nil
}

func (m *MCPManager) fetchResourcesUncached(ctx context.Context) ([]MCPResourceContent, error) {
	result := make([]MCPResourceContent, 0)
	for _, ref := range m.cfg.Config.MCPResources {
		transport, ok := m.getTransport(ref.Server)
		if !ok {
			continue
		}

		resp, err := transport.Call(ctx, "resources/read", map[string]interface{}{
			"uri": ref.URI,
		})
		if err != nil {
			log.Printf("[MCPManager] failed to fetch resource %s: %v", ref.URI, err)
			continue
		}

		var readResult struct {
			Contents []MCPResourceContent `json:"contents"`
		}
		if err := json.Unmarshal(resp, &readResult); err != nil {
			log.Printf("[MCPManager] failed to parse resource %s: %v", ref.URI, err)
			continue
		}

		result = append(result, readResult.Contents...)
	}

	return result, nil
}

// ListTools возвращает список доступных инструментов от всех подключенных MCP-серверов с кэшированием.
func (m *MCPManager) ListTools(ctx context.Context) ([]MCPTool, error) {
	start := time.Now()

	m.toolsCacheMu.RLock()
	entry, ok := m.toolsCache["all"]
	m.toolsCacheMu.RUnlock()
	if ok && entry.fetchedAt.Add(m.toolsCacheTTL).After(time.Now()) {
		log.Printf("[MCPManager] ListTools cache hit (took %v)", time.Since(start))
		return entry.tools, nil
	}

	result, err, _ := m.toolsSingleflight.Do("tools", func() (interface{}, error) {
		fetched, fetchErr := m.listToolsUncached(ctx)
		if fetchErr == nil {
			m.toolsCacheMu.Lock()
			m.toolsCache["all"] = toolsCacheEntry{tools: fetched, fetchedAt: time.Now()}
			m.toolsCacheMu.Unlock()
		}
		return fetched, fetchErr
	})
	if err != nil {
		return nil, err
	}

	log.Printf("[MCPManager] ListTools fetched %d tools, total=%v", len(result.([]MCPTool)), time.Since(start))
	return result.([]MCPTool), nil
}

func (m *MCPManager) listToolsUncached(ctx context.Context) ([]MCPTool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]MCPTool, 0)
	for name, transport := range m.transports {
		serverCfg := m.getServerConfig(name)

		resp, err := transport.Call(ctx, "tools/list", map[string]interface{}{})
		if err != nil {
			log.Printf("[MCPManager] failed to list tools from %s: %v", name, err)
			continue
		}

		var listResult struct {
			Tools []struct {
				Name        string                 `json:"name"`
				Description string                 `json:"description"`
				InputSchema map[string]interface{} `json:"inputSchema"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(resp, &listResult); err != nil {
			log.Printf("[MCPManager] failed to parse tools from %s: %v", name, err)
			continue
		}

		for _, t := range listResult.Tools {
			if serverCfg.ReadOnly && m.isWriteTool(t.Name, serverCfg.DeniedVerbs) {
				continue
			}
			desc := t.Description
			if name == "code_index" && strings.Contains(desc, "path_glob") {
				desc = desc + "\nПримеры path_glob: '**/*.bsl' — только модули; '**/*.xml' — описания метаданных (где искать синонимы реквизитов); '**/*.{bsl,xml}' — и модули, и метаданные."
			}
			result = append(result, MCPTool{
				Name:        t.Name,
				Description: desc,
				InputSchema: t.InputSchema,
				Server:      name,
			})
		}
	}

	return result, nil
}

// CallTool вызывает инструмент на указанном сервере.
func (m *MCPManager) CallTool(ctx context.Context, serverName, toolName string, arguments map[string]interface{}) (string, error) {
	transport, ok := m.getTransport(serverName)
	if !ok {
		return "", fmt.Errorf("MCP server not found: %s", serverName)
	}

	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	resp, err := transport.Call(callCtx, "tools/call", map[string]interface{}{
		"name":      toolName,
		"arguments": arguments,
	})
	if err != nil {
		return "", err
	}

	log.Printf("[CallTool] raw response for %s: %s", toolName, string(resp))

	var callResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp, &callResult); err != nil {
		return "", err
	}

	var text string
	for _, c := range callResult.Content {
		if c.Type == "text" {
			text += c.Text + "\n"
		}
	}
	text = strings.TrimSpace(text)

	if callResult.IsError {
		return "", fmt.Errorf("tool execution failed: %s", text)
	}

	return text, nil
}

func (m *MCPManager) createTransport(cfg MCPServerConfig) (MCPTransport, error) {
	switch cfg.Transport {
	case "stdio":
		return newStdioTransport(cfg)
	case "http":
		return newHTTPTransport(cfg)
	default:
		return nil, fmt.Errorf("unsupported MCP transport: %s", cfg.Transport)
	}
}

func (m *MCPManager) getTransport(name string) (MCPTransport, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	transport, ok := m.transports[name]
	return transport, ok
}

func (m *MCPManager) getServerConfig(name string) MCPServerConfig {
	for _, cfg := range m.cfg.MCPServers {
		if cfg.Name == name {
			return cfg
		}
	}
	return MCPServerConfig{}
}

func (m *MCPManager) isWriteTool(toolName string, deniedVerbs []string) bool {
	verbs := deniedVerbs
	if len(verbs) == 0 {
		verbs = []string{
			"add", "create", "update", "delete", "remove",
			"transition", "assign", "link", "post", "put",
			"patch", "write", "modify", "edit", "append",
			"set", "clear", "move",
		}
	}

	parts := strings.Split(strings.ToLower(toolName), "_")
	for _, part := range parts {
		for _, verb := range verbs {
			if part == verb {
				return true
			}
		}
	}

	return false
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
