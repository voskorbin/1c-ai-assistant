package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// ServerConfig описывает настройки HTTP-сервера.
type ServerConfig struct {
	Host                  string `json:"host"`
	Port                  int    `json:"port"`
	StreamStoreTTLSeconds int    `json:"stream_store_ttl_seconds,omitempty"`
}

// LLMConfig описывает настройки подключения к LLM-провайдеру.
type LLMConfig struct {
	URL                   string  `json:"url"`
	Model                 string  `json:"model"`
	APIKeyEnv             string  `json:"api_key_env"`
	TimeoutSeconds        int     `json:"timeout_seconds"`
	MaxTokens             int     `json:"max_tokens"`
	Temperature           float64 `json:"temperature,omitempty"`
	Reasoning             bool    `json:"reasoning,omitempty"`
	MaxConcurrentRequests int     `json:"max_concurrent_requests,omitempty"`
}

// MCPServerConfig описывает подключение к одному MCP-серверу.
type MCPServerConfig struct {
	Name        string            `json:"name"`
	Enabled     bool              `json:"enabled"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL                string            `json:"url,omitempty"`
	Username           string            `json:"username,omitempty"`
	Password           string            `json:"password,omitempty"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify,omitempty"`
	ReadOnly           bool              `json:"read_only"`
	DeniedVerbs        []string          `json:"denied_verbs,omitempty"`
}

// MCPResourceRef описывает фиксированный MCP-ресурс, который нужно подгрузить.
type MCPResourceRef struct {
	Server string `json:"server"`
	URI    string `json:"uri"`
}

// GatewayConfig описывает параметры генерации и подключённых источников.
type GatewayConfig struct {
	Model                  string           `json:"model"`
	Temperature            float64          `json:"temperature,omitempty"`
	MaxTokens              int              `json:"max_tokens,omitempty"`
	VisionSupported        bool             `json:"vision_supported,omitempty"`
	SystemPrompt           string           `json:"system_prompt,omitempty"`
	MCPResources           []MCPResourceRef `json:"mcp_resources,omitempty"`
	ConfluencePages        []string         `json:"confluence_pages,omitempty"`
	ConfluenceSpacesFilter string           `json:"confluence_spaces_filter,omitempty"`
}

// AppConfig содержит полную конфигурацию шлюза.
type AppConfig struct {
	Server     ServerConfig    `json:"server"`
	LLM        LLMConfig       `json:"llm"`
	MCPServers []MCPServerConfig `json:"mcp_servers,omitempty"`
	Config     GatewayConfig   `json:"config"`
}

// LoadConfig загружает конфигурацию из JSON-файла.
// Поддерживает подстановку переменных окружения в формате $VAR и ${VAR}.
func LoadConfig(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	data = []byte(os.ExpandEnv(string(data)))

	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8000
	}
	if cfg.Server.StreamStoreTTLSeconds == 0 {
		cfg.Server.StreamStoreTTLSeconds = 3600
	}
	if cfg.LLM.TimeoutSeconds == 0 {
		cfg.LLM.TimeoutSeconds = 60
	}
	if cfg.LLM.APIKeyEnv == "" {
		cfg.LLM.APIKeyEnv = "LLM_API_KEY"
	}
	if cfg.LLM.MaxConcurrentRequests == 0 {
		cfg.LLM.MaxConcurrentRequests = 10
	}

	return &cfg, nil
}

// APIKey возвращает API-ключ LLM из переменной окружения.
func (cfg *AppConfig) APIKey() string {
	return os.Getenv(cfg.LLM.APIKeyEnv)
}
