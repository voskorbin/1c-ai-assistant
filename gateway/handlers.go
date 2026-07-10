package main

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ChatPayload описывает тело запроса от 1С.
// Attachment описывает файл, прикреплённый к сообщению пользователя.
type Attachment struct {
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64
}

// ChatPayload описывает тело запроса на чат.
type ChatPayload struct {
	Model         string        `json:"model,omitempty"`
	Question      string        `json:"question"`
	Messages      []ChatMessage `json:"messages"`
	Attachments   []Attachment  `json:"attachments,omitempty"`
	ContextObject string        `json:"context_object,omitempty"`
	PromptHint    string        `json:"prompt_hint,omitempty"`
}

// ChatResult описывает ответ на синхронный запрос.
type ChatResult struct {
	Status  string `json:"status"`
	Success bool   `json:"success"`
	Answer  string `json:"answer,omitempty"`
	Error   string `json:"error,omitempty"`
}

// StreamStartResult описывает ответ на запуск потоковой генерации.
type StreamStartResult struct {
	Status    string `json:"status"`
	Success   bool   `json:"success"`
	RequestID string `json:"request_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// StreamStatusResult описывает статус потоковой генерации.
type StreamStatusResult struct {
	Status  string `json:"status"`
	Success bool   `json:"success"`
	Done    bool   `json:"done"`
	Answer  string `json:"answer"`
	Version int    `json:"version"`
	Error   string `json:"error,omitempty"`
}

// Handlers содержит HTTP-обработчики шлюза.
type Handlers struct {
	cfg         *AppConfig
	llm         *LLMClient
	store       *StreamStore
	mcpManager  *MCPManager
	confluence  *ConfluenceFetcher
}

// NewHandlers создаёт обработчики.
func NewHandlers(cfg *AppConfig, llm *LLMClient, store *StreamStore, mcpManager *MCPManager) *Handlers {
	return &Handlers{
		cfg:        cfg,
		llm:        llm,
		store:      store,
		mcpManager: mcpManager,
		confluence: NewConfluenceFetcher(),
	}
}

// RegisterRoutes регистрирует маршруты.
func (h *Handlers) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/chat", h.withCORS(h.handleChat))
	mux.HandleFunc("/chat/stream", h.withCORS(h.handleStreamStart))
	mux.HandleFunc("/chat/stream-sse", h.withCORS(h.handleStreamSSE))
	mux.HandleFunc("/chat/stop", h.withCORS(h.handleStreamStop))
	mux.HandleFunc("/chat/status/", h.withCORS(h.handleStreamStatus))
	mux.HandleFunc("/tools", h.withCORS(h.handleTools))
	mux.HandleFunc("/health", h.withCORS(h.handleHealth))
	mux.HandleFunc("/logs", h.withCORS(h.handleLogs))
}

// withCORS добавляет CORS-заголовки для запросов из веб-клиента 1С.
func (h *Handlers) withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func (h *Handlers) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload ChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.writeJSON(w, http.StatusBadRequest, ChatResult{Status: "error", Success: false, Error: "invalid JSON"})
		return
	}

	imageCount := 0
	for _, a := range payload.Attachments {
		if strings.HasPrefix(a.MimeType, "image/") {
			imageCount++
		}
	}
	log.Printf("[handleChat] question=%q attachments=%d images=%d", payload.Question, len(payload.Attachments), imageCount)

	mapping := h.cfg.Config

	if hasImageAttachments(payload) && !mapping.VisionSupported {
		h.writeJSON(w, http.StatusBadRequest, ChatResult{Status: "error", Success: false, Error: "Выбранная модель не поддерживает распознавание изображений. Уберите вложение или выберите другую модель."})
		return
	}

	resources, _ := h.mcpManager.FetchResources(r.Context())
	confluencePages, _ := h.confluence.FetchPages(mapping.ConfluencePages)
	resources = append(resources, confluencePages...)

	messages := h.buildMessages(payload, resources, mapping)
	model := h.resolveModel(payload.Model, mapping)

	tools, err := h.mcpManager.ListTools(r.Context())
	if err != nil {
		h.writeJSON(w, http.StatusInternalServerError, ChatResult{Status: "error", Success: false, Error: "Не удалось получить список инструментов: " + err.Error()})
		return
	}
	chatTools := h.toChatTools(tools)

	continueMode := false
	answer, err := h.runChatWithTools(r.Context(), model, messages, chatTools, tools, mapping, continueMode)
	if err != nil {
		h.writeJSON(w, http.StatusInternalServerError, ChatResult{Status: "error", Success: false, Error: err.Error()})
		return
	}

	h.writeJSON(w, http.StatusOK, ChatResult{Status: "ok", Success: true, Answer: answer})
}

func (h *Handlers) handleStreamStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload ChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.writeJSON(w, http.StatusBadRequest, StreamStartResult{Status: "error", Success: false, Error: "invalid JSON"})
		return
	}

	mapping := h.cfg.Config

	if hasImageAttachments(payload) && !mapping.VisionSupported {
		h.writeJSON(w, http.StatusBadRequest, StreamStartResult{Status: "error", Success: false, Error: "Выбранная модель не поддерживает распознавание изображений. Уберите вложение или выберите другую модель."})
		return
	}

	requestID, err := generateRequestID()
	if err != nil {
		h.writeJSON(w, http.StatusInternalServerError, StreamStartResult{Status: "error", Success: false, Error: err.Error()})
		return
	}

	imageCount := 0
	for _, a := range payload.Attachments {
		if strings.HasPrefix(a.MimeType, "image/") {
			imageCount++
		}
	}
	log.Printf("[handleStreamStart] request_id=%s question=%q attachments=%d images=%d", requestID, payload.Question, len(payload.Attachments), imageCount)

	resources, _ := h.mcpManager.FetchResources(r.Context())
	confluencePages, _ := h.confluence.FetchPages(mapping.ConfluencePages)
	resources = append(resources, confluencePages...)

	messages := h.buildMessages(payload, resources, mapping)
	model := h.resolveModel(payload.Model, mapping)

	tools, err := h.mcpManager.ListTools(r.Context())
	if err != nil {
		h.writeJSON(w, http.StatusInternalServerError, StreamStartResult{Status: "error", Success: false, Error: "Не удалось получить список инструментов: " + err.Error()})
		return
	}
	chatTools := h.toChatTools(tools)

	continueMode := false

	streamCtx, cancel := context.WithTimeout(context.Background(), time.Duration(h.cfg.LLM.TimeoutSeconds)*time.Second)

	state := h.store.Create(requestID, cancel)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[handleStreamStart] panic in chat goroutine: %v", r)
				state.SetError("Внутренняя ошибка сервера. Повторите запрос позже.")
			}
			cancel()
		}()
		executeTool := func(tc ChatToolCall) (string, error) {
			return h.executeToolCall(streamCtx, tc, tools, mapping)
		}
		h.llm.ChatStream(streamCtx, model, messages, chatTools, executeTool, state, continueMode)
	}()

	h.writeJSON(w, http.StatusOK, StreamStartResult{Status: "ok", Success: true, RequestID: requestID})
}

// StopPayload описывает тело запроса на остановку генерации.
type StopPayload struct {
	RequestID string `json:"request_id"`
}

// StopResult описывает ответ на запрос остановки генерации.
type StopResult struct {
	Status  string `json:"status"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

func (h *Handlers) handleStreamStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload StopPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.writeJSON(w, http.StatusBadRequest, StopResult{Status: "error", Success: false, Error: "invalid JSON"})
		return
	}

	state, ok := h.store.Get(payload.RequestID)
	if !ok {
		log.Printf("[handleStreamStop] request %s not found", payload.RequestID)
		h.writeJSON(w, http.StatusNotFound, StopResult{Status: "error", Success: false, Error: "request not found"})
		return
	}

	log.Printf("[handleStreamStop] stopping request %s", payload.RequestID)
	state.Stop()
	h.writeJSON(w, http.StatusOK, StopResult{Status: "ok", Success: true})
}

func (h *Handlers) handleStreamSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload ChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeSSEError(w, "invalid JSON")
		return
	}

	mapping := h.cfg.Config

	if hasImageAttachments(payload) && !mapping.VisionSupported {
		writeSSEError(w, "Выбранная модель не поддерживает распознавание изображений. Уберите вложение или выберите другую модель.")
		return
	}

	resources, _ := h.mcpManager.FetchResources(r.Context())
	confluencePages, _ := h.confluence.FetchPages(mapping.ConfluencePages)
	resources = append(resources, confluencePages...)

	messages := h.buildMessages(payload, resources, mapping)
	model := h.resolveModel(payload.Model, mapping)

	tools, err := h.mcpManager.ListTools(r.Context())
	if err != nil {
		writeSSEError(w, "Не удалось получить список инструментов: "+err.Error())
		return
	}
	chatTools := h.toChatTools(tools)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if ok {
		flusher.Flush()
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(h.cfg.LLM.TimeoutSeconds)*time.Second)
	defer cancel()

	executeTool := func(tc ChatToolCall) (string, error) {
		return h.executeToolCall(ctx, tc, tools, mapping)
	}

	writeChunk := func(chunk string) {
		lines := strings.Split(chunk, "\n")
		for _, line := range lines {
			fmt.Fprintf(w, "data: %s\n", line)
		}
		fmt.Fprint(w, "\n")
		if ok {
			flusher.Flush()
		}
	}

	answer, err := h.llm.ChatStreamToWriter(ctx, model, messages, chatTools, executeTool, writeChunk, false)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
	} else {
		fmt.Fprintf(w, "data: %s\n\n", "[DONE]")
	}
	if ok {
		flusher.Flush()
	}

	log.Printf("[stream sse] finished, answer length: %d", len(answer))
}

func writeSSEError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", message)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handlers) handleStreamStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	requestID := strings.TrimPrefix(r.URL.Path, "/chat/status/")
	if requestID == "" {
		h.writeJSON(w, http.StatusBadRequest, StreamStatusResult{Status: "error", Success: false, Error: "missing request_id"})
		return
	}

	state, ok := h.store.Get(requestID)
	if !ok {
		h.writeJSON(w, http.StatusNotFound, StreamStatusResult{Status: "error", Success: false, Error: "request not found"})
		return
	}

	snapshot := state.Snapshot()
	status := "ok"
	if snapshot.Error != "" {
		status = "error"
	}
	h.writeJSON(w, http.StatusOK, StreamStatusResult{
		Status:  status,
		Success: snapshot.Error == "",
		Done:    snapshot.Done,
		Answer:  snapshot.Text,
		Version: snapshot.Version,
		Error:   snapshot.Error,
	})
}

func (h *Handlers) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"status": "ok",
		"mcp":    h.mcpManager.Healthy(),
	}
	h.writeJSON(w, http.StatusOK, status)
}

func (h *Handlers) handleTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tools, err := h.mcpManager.ListTools(r.Context())
	if err != nil {
		h.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"tools":   tools,
	})
}

func (h *Handlers) runChatWithTools(
	ctx context.Context,
	model string,
	messages []ChatMessage,
	chatTools []ChatTool,
	tools []MCPTool,
	mapping GatewayConfig,
	continueMode bool,
) (string, error) {
	_ = ctx

	maxIterations := 25
	if continueMode {
		maxIterations = 150
	}
	log.Printf("[runChatWithTools] continueMode=%v maxIterations=%d chatTools=%d", continueMode, maxIterations, len(chatTools))
	var lastAssistantContent string
	toolCallCounts := make(map[string]int)
	const maxRepeatToolCalls = 3
	lastToolResultHash := ""
	repeatResultStreak := 0
	const maxRepeatResultStreak = 3
	codeSearchStreak := 0
	const maxCodeSearchStreak = 5
	for i := 0; i < maxIterations; i++ {
		log.Printf("[sync chat] iteration %d", i+1)
		toolChoice := "auto"
		if len(chatTools) == 0 {
			toolChoice = ""
		}
		resp, err := h.llm.Chat(ctx, model, messages, chatTools, toolChoice)
		if err != nil {
			return "", err
		}

		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("LLM returned no choices")
		}

		choice := resp.Choices[0]
		lastAssistantContent = choice.Message.Content
		log.Printf("[sync chat] assistant content: %q, tool calls: %d", choice.Message.Content, len(choice.Message.ToolCalls))
		if len(choice.Message.ToolCalls) == 0 {
			return cleanToolCallArtifacts(choice.Message.Content), nil
		}

		messages = append(messages, ChatMessage{
			Role:      "assistant",
			Content:   choice.Message.Content,
			ToolCalls: choice.Message.ToolCalls,
		})

		combinedResult := ""
		for _, tc := range choice.Message.ToolCalls {
			lowerName := strings.ToLower(tc.Function.Name)
			if lowerName == "grep_code" || lowerName == "read_file" {
				codeSearchStreak++
			} else {
				codeSearchStreak = 0
			}
			if codeSearchStreak > maxCodeSearchStreak {
				log.Printf("[sync chat] code search streak exceeded")
				return cleanToolCallArtifacts(lastAssistantContent) + "\n\nВопрос слишком широкий: модель слишком долго ищет в коде. Попробуйте конкретизировать объект, реквизит или сценарий (например, 'заполнение при создании', 'выгрузка в EDI', 'обработка загрузки').", nil
			}
			toolKey := tc.Function.Name + ":" + tc.Function.Arguments
			toolCallCounts[toolKey]++
			if toolCallCounts[toolKey] > maxRepeatToolCalls {
				log.Printf("[sync chat] repeated tool call detected: %s", toolKey)
				return cleanToolCallArtifacts(lastAssistantContent) + "\n\nАнализ прерван: модель зациклилась на одном и том же запросе. Попробуйте переформулировать вопрос или уточнить объект/реквизит.", nil
			}
		log.Printf("[sync chat] executing tool %s with args: %s", tc.Function.Name, tc.Function.Arguments)
		toolResult, err := h.executeToolCall(ctx, tc, tools, mapping)
			if err != nil {
				toolResult = "Ошибка выполнения инструмента: " + err.Error()
				log.Printf("[sync chat] tool %s error: %s", tc.Function.Name, toolResult)
			} else {
				log.Printf("[sync chat] tool %s result length: %d", tc.Function.Name, len(toolResult))
			}
			combinedResult += tc.Function.Name + tc.Function.Arguments

			messages = append(messages, ChatMessage{
				Role:       "tool",
				Content:    toolResult,
				ToolCallID: tc.ID,
			})
		}
		resultHash := fmt.Sprintf("%x", sha1.Sum([]byte(combinedResult)))
		if resultHash == lastToolResultHash {
			repeatResultStreak++
			if repeatResultStreak >= maxRepeatResultStreak {
				log.Printf("[sync chat] repeated result streak exceeded")
				return cleanToolCallArtifacts(lastAssistantContent) + "\n\nАнализ прерван: модель не может найти новую информацию. Попробуйте переформулировать вопрос или уточнить объект/реквизит.", nil
			}
		} else {
			repeatResultStreak = 0
			lastToolResultHash = resultHash
		}
	}

	if lastAssistantContent != "" {
		return cleanToolCallArtifacts(lastAssistantContent), nil
	}

	// Force a final answer using the collected context if the model didn't
	// produce one within the iteration limit.
	finalMessages := append(messages, ChatMessage{
		Role:    "user",
		Content: "На основе собранной информации дай краткий, структурированный ответ на вопрос пользователя. Если информации недостаточно, скажи, что не удалось найти достаточно данных, и предложи уточнить вопрос.",
	})
	resp, err := h.llm.Chat(ctx, model, finalMessages, nil, "")
	if err == nil && len(resp.Choices) > 0 {
		return cleanToolCallArtifacts(resp.Choices[0].Message.Content), nil
	}
	return "Не удалось получить ответ. Попробуйте уточнить вопрос.", nil
}

func (h *Handlers) executeToolCall(ctx context.Context, tc ChatToolCall, tools []MCPTool, mapping GatewayConfig) (string, error) {
	_ = ctx
	lowerName := strings.ToLower(tc.Function.Name)
	if lowerName == "find_metadata_by_synonym" {
		codeIndexPath := ""
		for _, server := range h.cfg.MCPServers {
			if server.Name == "code_index" && len(server.Args) > 0 {
				// Последний аргумент npx-вызова — путь к индексируемому каталогу.
				codeIndexPath = server.Args[len(server.Args)-1]
				break
			}
		}
		if codeIndexPath == "" {
			return "", fmt.Errorf("code_index server is not configured or missing path argument")
		}
		return FindMetadataBySynonym(codeIndexPath, tc.Function.Arguments)
	}
	if strings.HasPrefix(lowerName, "confluence_") {
		return "", fmt.Errorf("confluence tools are disabled; use configured pages only")
	}
	if strings.HasPrefix(lowerName, "jira_") {
		return "", fmt.Errorf("jira tools are disabled")
	}
	
	var tool MCPTool
	found := false
	for _, t := range tools {
		if t.Name == tc.Function.Name {
			tool = t
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("tool not found: %s", tc.Function.Name)
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("failed to parse tool arguments: %w", err)
	}

	// Ограничиваем поиск в Confluence настроенным пространством.
	if mapping.ConfluenceSpacesFilter != "" && isConfluenceSearchTool(tool.Name) {
		if _, ok := args["spaces_filter"]; !ok {
			args["spaces_filter"] = mapping.ConfluenceSpacesFilter
		}
	}

	if err := validateReadOnlyToolCall(tool.Name, args); err != nil {
		log.Printf("[executeToolCall] tool %s blocked by read-only guard: %s", tool.Name, err.Error())
		return "", err
	}

	log.Printf("[executeToolCall] calling tool %s on server %s with args: %s", tool.Name, tool.Server, tc.Function.Arguments)
	toolStart := time.Now()
	result, err := h.mcpManager.CallTool(ctx, tool.Server, tool.Name, args)
	if err != nil {
		log.Printf("[executeToolCall] tool %s error: %s (took %v)", tool.Name, err.Error(), time.Since(toolStart))
		return "", err
	}
	log.Printf("[executeToolCall] tool %s result length: %d, took %v", tool.Name, len(result), time.Since(toolStart))
	return result, nil
}

// isConfluenceSearchTool возвращает true для инструментов поиска в Confluence,
// которые поддерживают фильтрацию по пространствам.
func isConfluenceSearchTool(toolName string) bool {
	lower := strings.ToLower(toolName)
	return lower == "confluence_search" || lower == "confluence_search_user"
}

// validateReadOnlyToolCall блокирует инструменты, которые могут изменить данные.
// Для SQL-инструментов (execute_query и аналогичных) разрешены только запросы,
// начинающиеся с SELECT / ВЫБРАТЬ. Для остальных инструментов аргументы не проверяются.
func validateReadOnlyToolCall(toolName string, args map[string]interface{}) error {
	lowerName := strings.ToLower(toolName)
	isSQLTool := strings.Contains(lowerName, "query") || strings.Contains(lowerName, "sql") || strings.Contains(lowerName, "select")
	if !isSQLTool {
		return nil
	}

	sqlKeys := []string{"query", "sql", "statement"}
	for _, key := range sqlKeys {
		value, ok := args[key]
		if !ok {
			continue
		}
		query, ok := value.(string)
		if !ok {
			continue
		}
		trimmed := strings.ToUpper(strings.TrimSpace(query))
		if !strings.HasPrefix(trimmed, "SELECT") && !strings.HasPrefix(trimmed, "ВЫБРАТЬ") {
			return fmt.Errorf("tool %s: only SELECT queries are allowed (read-only mode)", toolName)
		}
	}
	return nil
}

// hasImageAttachments возвращает true, если среди вложений есть изображения.
func hasImageAttachments(payload ChatPayload) bool {
	for _, att := range payload.Attachments {
		if strings.HasPrefix(att.MimeType, "image/") {
			return true
		}
	}
	return false
}

func (h *Handlers) buildMessages(payload ChatPayload, resources []MCPResourceContent, mapping GatewayConfig) []ChatMessage {
	messages := make([]ChatMessage, 0, len(payload.Messages)+len(resources)+4)

	systemPrompt := mapping.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = "Ты — ассистент для конфигурации 1С. Помогаешь пользователю с вопросами по конфигурации и данным базы. " +
			"Если вопрос требует фактических данных, вызови соответствующий инструмент. " +
			"Не придумывай ответы, которых нет в данных инструментов или предоставленном контексте. " +
			"Если вопрос неясен или не хватает контекста — задай уточняющий вопрос. " +
			"Отвечай только готовым результатом или уточняющим вопросом."
	}

	messages = append(messages, ChatMessage{
		Role:    "system",
		Content: systemPrompt,
	})

	if len(resources) > 0 {
		var sb strings.Builder
		sb.WriteString("Контекст из подключённых источников:\n")
		for _, r := range resources {
			sb.WriteString("\n--- ")
			sb.WriteString(r.URI)
			sb.WriteString(" ---\n")
			sb.WriteString(r.Text)
		}
		messages = append(messages, ChatMessage{
			Role:    "system",
			Content: sb.String(),
		})
		// Повторяем ключевую инструкцию после большого контекста, чтобы модель не забыла.
		messages = append(messages, ChatMessage{
			Role: "system",
			Content: "Важно: если вопрос содержит синоним реквизита 1С (например 'Style code (suppl.)'), вызови find_metadata_by_synonym с параметром synonym. " +
				"Этот инструмент уже возвращает в ответе поле usage — это топ-10 мест использования реквизита в модулях .bsl. Отвечай на основе этих данных. " +
				"Не вызывай grep_code повторно для поиска того же реквизита. " +
				"Пример: пользователь спрашивает 'как заполняется Style code (suppl.)'. Ты вызываешь find_metadata_by_synonym с synonym='Style code (suppl.)', получаешь внутреннее имя и usage, и сразу формируешь ответ. Без дополнительных grep_code/read_file.",
		})
	}

	if payload.ContextObject != "" {
		messages = append(messages, ChatMessage{
			Role: "system",
			Content: "Текущий объект, о котором идёт речь в вопросе. " +
				"Если пользователь говорит 'эта модель', 'этот объект', 'этот элемент', 'этот документ' или спрашивает 'расскажи про это/его/её', " +
				"он имеет в виду именно этот объект. При необходимости используй GUID для получения дополнительных данных через инструменты 1С. " +
				"Контекст объекта: " + payload.ContextObject,
		})
	}

	if payload.PromptHint != "" {
		log.Printf("[buildMessages] appending prompt hint (%d chars)", len(payload.PromptHint))
		messages = append(messages, ChatMessage{
			Role:    "system",
			Content: "Дополнительная инструкция от администратора конфигурации: " + payload.PromptHint,
		})
	}

	messages = append(messages, payload.Messages...)

	if payload.Question != "" || len(payload.Attachments) > 0 {
		msg := ChatMessage{Role: "user"}
		if payload.Question != "" {
			msg.ContentParts = append(msg.ContentParts, ChatContentPart{
				Type: "text",
				Text: payload.Question,
			})
		}
		for _, att := range payload.Attachments {
			if strings.HasPrefix(att.MimeType, "image/") {
				msg.ContentParts = append(msg.ContentParts, ChatContentPart{
					Type: "image_url",
					ImageURL: &struct {
						URL string `json:"url"`
					}{
						URL: fmt.Sprintf("data:%s;base64,%s", att.MimeType, att.Data),
					},
				})
			}
		}
		messages = append(messages, msg)
	}

	return messages
}

func (h *Handlers) resolveModel(requestModel string, mapping GatewayConfig) string {
	if requestModel != "" {
		return requestModel
	}
	if mapping.Model != "" {
		return mapping.Model
	}
	return h.cfg.LLM.Model
}

func (h *Handlers) toChatTools(tools []MCPTool) []ChatTool {
	result := make([]ChatTool, 0, len(tools)+1)

	// Встроенный инструмент для поиска метаданных по синониму.
	result = append(result, ChatTool{
		Type: "function",
		Function: ChatToolFunction{
			Name:        "find_metadata_by_synonym",
			Description: "Найти объект метаданных 1С или его реквизит по синониму (caption). Используй, когда пользователь спрашивает про поле/объект по английскому или русскому синониму, например 'Style code (suppl.)'. Возвращает не более 20 результатов с внутренними именами и типами. Поиск использования в модулях по умолчанию отключён — включай только если пользователь явно просит найти, где заполняется/используется реквизит.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"synonym": map[string]interface{}{
						"type":        "string",
						"description": "Синоним (caption) объекта или реквизита, например 'Style code (suppl.)'.",
					},
					"meta_type": map[string]interface{}{
						"type":        "string",
						"description": "Опционально ограничить тип метаданных: Catalog, Document, InformationRegister, AccumulationRegister.",
					},
					"language": map[string]interface{}{
						"type":        "string",
						"description": "Язык синонима: en или ru. По умолчанию en.",
					},
					"include_usage": map[string]interface{}{
						"type":        "boolean",
						"description": "Если true, для каждого результата добавить до 10 мест использования реквизита в модулях .bsl. По умолчанию false.",
					},
					"max_results": map[string]interface{}{
						"type":        "integer",
						"description": "Максимальное количество результатов. По умолчанию 20.",
					},
				},
				"required": []string{"synonym"},
			},
		},
	})

	for _, t := range tools {
		// Confluence-инструменты поиска отключаем: контекст из Confluence
		// должен браться только из настроенных фиксированных страниц.
		// Jira-инструменты отключаем: рядовые пользователи не должны видеть Jira.
		lowerName := strings.ToLower(t.Name)
		if strings.HasPrefix(lowerName, "confluence_") || strings.HasPrefix(lowerName, "jira_") {
			continue
		}
		result = append(result, ChatTool{
			Type: "function",
			Function: ChatToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return result
}

func (h *Handlers) writeJSON(w http.ResponseWriter, status int, data any) {
	body, err := json.Marshal(data)
	if err != nil {
		body = []byte(`{"status":"error","success":false,"error":"failed to encode response"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func generateRequestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate request id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// cleanToolCallArtifacts удаляет из текста артефакты вызовов инструментов,
// которые модель иногда выводит в виде XML-тегов, plain-формата functions.name:idx {json}
// или специальных токенов <|toolcall...|>.
func cleanToolCallArtifacts(text string) string {
	// Нормализуем переносы строк: Windows (\r\n) и одиночные \r -> \n,
	// чтобы 1С не отображала возврат каретки как квадратик.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// Удаляем блоки рассуждений модели  <thinking>...<\/think>.
	text = regexp.MustCompile(`(?s)\s*<thinking>.*?<\/think>\s*`).ReplaceAllString(text, "")
	// Удаляем блоки <|toolcallssectionbegin|> ... <|toolcallssectionend|>.
	text = regexp.MustCompile(`(?s)\<\|toolcallssectionbegin\|\>.*?\<\|toolcallssectionend\|\>`).ReplaceAllString(text, "")
	// Удаляем отдельные токены <|...|>.
	text = regexp.MustCompile(`\<\|[^|]+\|\>`).ReplaceAllString(text, "")
	// Удаляем блоки <tool_call>...</tool_call>.
	text = regexp.MustCompile(`(?s)<tool_call>.*?</tool_call>`).ReplaceAllString(text, "")
	// Удаляем блоки <functions.name>...</functions.name>.
	text = regexp.MustCompile(`(?s)<functions\.[^>]+>.*?</functions\.[^>]+>`).ReplaceAllString(text, "")
	// Удаляем самозакрывающиеся теги <functions.name ... />.
	text = regexp.MustCompile(`<functions\.[^/]+/>`).ReplaceAllString(text, "")
	// Удаляем trailing plain-формат вызовов инструментов: functions.name:idx {json} ...
	// Убираем только артефакты в конце текста, чтобы не потерять полезный ответ,
	// если в нём встречается слово "functions".
	text = regexp.MustCompile(`(?s)\s*functions\.\w+(?::\d+)?\s*\{.*\}\s*$`).ReplaceAllString(text, "")
	// Удаляем оставшиеся пустые строки по краям.
	return strings.TrimSpace(text)
}
