package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ChatContentPart описывает часть мультимодального сообщения (текст или изображение).
type ChatContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// ChatMessage описывает сообщение в формате OpenAI.
type ChatMessage struct {
	Role         string            `json:"role"`
	Content      string            `json:"-"`
	ContentParts []ChatContentPart `json:"-"`
	ToolCalls    []ChatToolCall    `json:"tool_calls,omitempty"`
	ToolCallID   string            `json:"tool_call_id,omitempty"`
}

// MarshalJSON сериализует content как строку или массив частей.
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	type Alias ChatMessage
	if len(m.ContentParts) > 0 {
		return json.Marshal(&struct {
			*Alias
			Content []ChatContentPart `json:"content"`
		}{
			Alias:   (*Alias)(&m),
			Content: m.ContentParts,
		})
	}
	return json.Marshal(&struct {
		*Alias
		Content string `json:"content"`
	}{
		Alias:   (*Alias)(&m),
		Content: m.Content,
	})
}

// UnmarshalJSON десериализует content из строки или массива.
func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	type Alias ChatMessage
	aux := &struct {
		*Alias
		Content json.RawMessage `json:"content"`
	}{
		Alias: (*Alias)(m),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if len(aux.Content) == 0 {
		return nil
	}
	if aux.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(aux.Content, &s); err != nil {
			return err
		}
		m.Content = s
		return nil
	}
	var parts []ChatContentPart
	if err := json.Unmarshal(aux.Content, &parts); err != nil {
		return err
	}
	m.ContentParts = parts
	return nil
}

// ChatTool описывает инструмент, доступный LLM.
type ChatTool struct {
	Type     string           `json:"type"`
	Function ChatToolFunction `json:"function"`
}

// ChatToolFunction описывает функцию инструмента.
type ChatToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ChatToolCall описывает вызов инструмента из ответа LLM.
type ChatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatRequest описывает тело запроса к LLM.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	Tools       []ChatTool    `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	Reasoning   bool          `json:"reasoning"`
}

// ChatResponseMessage описывает сообщение-ответ LLM.
type ChatResponseMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []ChatToolCall `json:"tool_calls"`
}

// ChatResponseChoice описывает вариант ответа LLM (для непотокового режима).
type ChatResponseChoice struct {
	Message      ChatResponseMessage `json:"message"`
	FinishReason string              `json:"finish_reason"`
}

// ChatResponse описывает полный ответ LLM (для непотокового режима).
type ChatResponse struct {
	Choices []ChatResponseChoice `json:"choices"`
}

// StreamToolCallDelta описывает дельту вызова инструмента в SSE.
type StreamToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// StreamDelta описывает дельту в SSE-сообщении.
type StreamDelta struct {
	Content   string                `json:"content"`
	ToolCalls []StreamToolCallDelta `json:"tool_calls,omitempty"`
}

// StreamChoice описывает choice в SSE-сообщении.
type StreamChoice struct {
	Delta        StreamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

// StreamEvent описывает одно SSE-сообщение от LLM.
type StreamEvent struct {
	Choices []StreamChoice `json:"choices"`
}

// LLMClient клиент для обращения к LLM-провайдеру.
type LLMClient struct {
	cfg    *AppConfig
	client *http.Client
	sem    chan struct{}
}

// NewLLMClient создаёт новый клиент LLM.
func NewLLMClient(cfg *AppConfig) *LLMClient {
	return &LLMClient{
		cfg: cfg,
		client: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				MaxConnsPerHost:     100,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     60 * time.Second,
				DisableKeepAlives:   false,
			},
		},
		sem: make(chan struct{}, cfg.LLM.MaxConcurrentRequests),
	}
}

// acquireLLMSlot ожидает свободного слота для вызова LLM.
func (c *LLMClient) acquireLLMSlot(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseLLMSlot освобождает слот для вызова LLM.
func (c *LLMClient) releaseLLMSlot() {
	<-c.sem
}

// Chat выполняет синхронный запрос к LLM.
func (c *LLMClient) Chat(ctx context.Context, model string, messages []ChatMessage, tools []ChatTool, toolChoice string) (*ChatResponse, error) {
	if err := c.acquireLLMSlot(ctx); err != nil {
		return nil, fmt.Errorf("failed to acquire LLM slot: %w", err)
	}
	defer c.releaseLLMSlot()

	reqBody := ChatRequest{
		Model:       model,
		Messages:    messages,
		Stream:      false,
		MaxTokens:   c.cfg.LLM.MaxTokens,
		Temperature: c.cfg.LLM.Temperature,
		Tools:       tools,
		Reasoning:   c.cfg.LLM.Reasoning,
	}
	if toolChoice != "" {
		reqBody.ToolChoice = toolChoice
	} else if len(tools) > 0 {
		reqBody.ToolChoice = "auto"
	}

	respBody, err := c.doRequest(ctx, reqBody)
	if err != nil {
		return nil, err
	}

	var resp ChatResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse LLM response: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("LLM returned no choices")
	}

	return &resp, nil
}

// ChatStreamToWriter запускает потоковую генерацию с поддержкой tool calls
// и передаёт каждую порцию текста в writeChunk. Возвращает итоговый очищенный ответ.
func (c *LLMClient) ChatStreamToWriter(
	ctx context.Context,
	model string,
	messages []ChatMessage,
	tools []ChatTool,
	executeTool func(ChatToolCall) (string, error),
	writeChunk func(string),
	continueMode bool,
) (string, error) {
	maxIterations := 25
	if continueMode {
		maxIterations = 150
	}
	log.Printf("[stream to writer] continueMode=%v maxIterations=%d", continueMode, maxIterations)
	currentMessages := messages
	var fullAnswer strings.Builder
	toolCallCounts := make(map[string]int)
	const maxRepeatToolCalls = 3
	emptyResultStreak := 0
	const maxEmptyResultStreak = 5
	lastToolResultHash := ""
	repeatResultStreak := 0
	const maxRepeatResultStreak = 3

	for iteration := 0; iteration < maxIterations; iteration++ {
		log.Printf("[stream to writer] iteration %d", iteration+1)
		reqBody := ChatRequest{
			Model:       model,
			Messages:    currentMessages,
			Stream:      true,
			MaxTokens:   c.cfg.LLM.MaxTokens,
			Temperature: c.cfg.LLM.Temperature,
			Tools:       tools,
			Reasoning:   c.cfg.LLM.Reasoning,
		}
		if len(tools) > 0 {
			reqBody.ToolChoice = "auto"
		}

		if err := c.acquireLLMSlot(ctx); err != nil {
			return "", fmt.Errorf("failed to acquire LLM slot: %w", err)
		}

		var resp *http.Response
		for attempt := 1; attempt <= 3; attempt++ {
			req, err := c.newRequest(ctx, reqBody)
			if err != nil {
				c.releaseLLMSlot()
				return "", err
			}
			resp, err = c.client.Do(req)
			if err == nil {
				break
			}
			log.Printf("[LLM stream] attempt %d/3 failed: %v", attempt, err)
			if attempt < 3 && isRetryableLLMError(err) {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			log.Printf("[LLM stream] LLM request failed after %d attempts: %v", attempt, err)
			c.releaseLLMSlot()
			return "", fmt.Errorf("Не удалось получить ответ от языковой модели. Повторите запрос позже.")
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			c.releaseLLMSlot()
			return "", fmt.Errorf("LLM returned status %d: %s", resp.StatusCode, string(body))
		}
		defer c.releaseLLMSlot()

		var assistantContent string
		toolCallAcc := make(map[int]*ChatToolCall)
		var hasToolCalls bool

		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err == io.EOF {
				break
			}
			if err != nil {
				resp.Body.Close()
				return "", fmt.Errorf("failed to read stream: %w", err)
			}

			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if line == "data: [DONE]" {
				break
			}

			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			var event StreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			for _, choice := range event.Choices {
				if choice.FinishReason == "tool_calls" {
					hasToolCalls = true
				}
				if choice.Delta.Content != "" {
					assistantContent += choice.Delta.Content
					fullAnswer.WriteString(choice.Delta.Content)
					writeChunk(choice.Delta.Content)
				}
				for _, tcDelta := range choice.Delta.ToolCalls {
					hasToolCalls = true
					tc, ok := toolCallAcc[tcDelta.Index]
					if !ok {
						tc = &ChatToolCall{
							ID:   tcDelta.ID,
							Type: tcDelta.Type,
						}
						toolCallAcc[tcDelta.Index] = tc
					}
					if tcDelta.Function.Name != "" {
						tc.Function.Name = tcDelta.Function.Name
					}
					tc.Function.Arguments += tcDelta.Function.Arguments
				}
			}
		}
		resp.Body.Close()

		// Распознаём текстовые вызовы инструментов (functions.name:idx {json}),
		// которые LLM может выдавать в потоковом режиме вместо поля tool_calls.
		textCalls, cleanedContent := extractToolCallsFromText(assistantContent)
		if len(textCalls) > 0 {
			hasToolCalls = true
			baseIdx := len(toolCallAcc)
			for i := range textCalls {
				toolCallAcc[baseIdx+i] = &textCalls[i]
			}
			assistantContent = cleanedContent
		}

		if !hasToolCalls || len(toolCallAcc) == 0 {
			return cleanToolCallArtifacts(fullAnswer.String()), nil
		}

		toolCalls := make([]ChatToolCall, 0, len(toolCallAcc))
		for i := 0; i < len(toolCallAcc); i++ {
			if tc, ok := toolCallAcc[i]; ok {
				toolCalls = append(toolCalls, *tc)
			}
		}

		currentMessages = append(currentMessages, ChatMessage{
			Role:      "assistant",
			Content:   assistantContent,
			ToolCalls: toolCalls,
		})

		allResultsEmpty := true
		for _, tc := range toolCalls {
			toolKey := tc.Function.Name + ":" + tc.Function.Arguments
			toolCallCounts[toolKey]++
			if toolCallCounts[toolKey] > maxRepeatToolCalls {
				log.Printf("[stream to writer] repeated tool call detected: %s", toolKey)
				return cleanToolCallArtifacts(fullAnswer.String()) + "\n\nАнализ прерван: модель зациклилась на одном и том же запросе. Попробуйте переформулировать вопрос или уточнить объект/реквизит.", nil
			}
			toolResult, err := executeTool(tc)
			if err != nil {
				toolResult = "Ошибка выполнения инструмента: " + err.Error()
				log.Printf("[stream to writer] tool %s error: %s", tc.Function.Name, toolResult)
			} else {
				log.Printf("[stream to writer] tool %s result length: %d", tc.Function.Name, len(toolResult))
			}
			if len(toolResult) > 50 && !strings.Contains(toolResult, "0 совпадений") && !strings.Contains(toolResult, "\"result\":{}") && !strings.Contains(toolResult, "не найдено") {
				allResultsEmpty = false
			}
			currentMessages = append(currentMessages, ChatMessage{
				Role:       "tool",
				Content:    toolResult,
				ToolCallID: tc.ID,
			})
		}
		if allResultsEmpty {
			emptyResultStreak++
			if emptyResultStreak >= maxEmptyResultStreak {
				log.Printf("[stream to writer] empty result streak exceeded")
				return cleanToolCallArtifacts(fullAnswer.String()) + "\n\nНе удалось найти информацию по запросу. Попробуйте переформулировать вопрос, уточнить внутреннее имя объекта/реквизита или указать, где искать.", nil
			}
		} else {
			emptyResultStreak = 0
		}
		// Проверяем, не повторяется ли один и тот же результат подряд.
		combinedResult := ""
		for _, tc := range toolCalls {
			combinedResult += tc.Function.Name + tc.Function.Arguments
		}
		resultHash := fmt.Sprintf("%x", sha1.Sum([]byte(combinedResult)))
		if resultHash == lastToolResultHash {
			repeatResultStreak++
			if repeatResultStreak >= maxRepeatResultStreak {
				log.Printf("[stream to writer] repeated result streak exceeded")
				return cleanToolCallArtifacts(fullAnswer.String()) + "\n\nАнализ прерван: модель не может найти новую информацию. Попробуйте переформулировать вопрос или уточнить объект/реквизит.", nil
			}
		} else {
			repeatResultStreak = 0
			lastToolResultHash = resultHash
		}
	}

	return cleanToolCallArtifacts(fullAnswer.String()), nil
}

// ChatStream запускает потоковую генерацию с поддержкой tool calls и пишет итоговый ответ в StreamState.
func (c *LLMClient) ChatStream(
	ctx context.Context,
	model string,
	messages []ChatMessage,
	tools []ChatTool,
	executeTool func(ChatToolCall) (string, error),
	state *StreamState,
	continueMode bool,
) {
	maxIterations := 25
	if continueMode {
		maxIterations = 150
	}
	log.Printf("[stream chat] continueMode=%v maxIterations=%d", continueMode, maxIterations)
	currentMessages := messages

	for iteration := 0; iteration < maxIterations; iteration++ {
		if state.IsStopped() {
			log.Printf("[stream chat] request %s stopped by user at start of iteration %d", state.RequestID, iteration+1)
			state.SetDone()
			return
		}

		log.Printf("[stream chat] iteration %d", iteration+1)
		reqBody := ChatRequest{
			Model:       model,
			Messages:    currentMessages,
			Stream:      true,
			MaxTokens:   c.cfg.LLM.MaxTokens,
			Temperature: c.cfg.LLM.Temperature,
			Tools:       tools,
			Reasoning:   c.cfg.LLM.Reasoning,
		}
		if len(tools) > 0 {
			reqBody.ToolChoice = "auto"
		}

		log.Printf("[stream chat] iteration %d waiting for LLM slot", iteration+1)
		if err := c.acquireLLMSlot(ctx); err != nil {
			if state.IsStopped() {
				state.SetDone()
			} else {
				state.SetError(err.Error())
			}
			return
		}
		log.Printf("[stream chat] iteration %d got LLM slot", iteration+1)

		req, err := c.newRequest(ctx, reqBody)
		if err != nil {
			c.releaseLLMSlot()
			if state.IsStopped() {
				state.SetDone()
			} else {
				state.SetError(err.Error())
			}
			return
		}

		var resp *http.Response
		for attempt := 1; attempt <= 3; attempt++ {
			log.Printf("[stream chat] iteration %d sending LLM request (attempt %d/3)", iteration+1, attempt)
			resp, err = c.client.Do(req)
			if err == nil {
				log.Printf("[stream chat] iteration %d got LLM response status %d", iteration+1, resp.StatusCode)
				break
			}
			log.Printf("[stream chat] LLM request attempt %d/3 failed: %v", attempt, err)
			if attempt < 3 && isRetryableLLMError(err) {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			log.Printf("[stream chat] LLM request failed after %d attempts: %v", attempt, err)
			c.releaseLLMSlot()
			if state.IsStopped() {
				state.SetDone()
			} else {
				state.SetError("Не удалось получить ответ от языковой модели. Повторите запрос позже.")
			}
			return
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			c.releaseLLMSlot()
			if state.IsStopped() {
				state.SetDone()
			} else {
				state.SetError(fmt.Sprintf("LLM returned status %d: %s", resp.StatusCode, string(body)))
			}
			return
		}

		var assistantContent string
		toolCallAcc := make(map[int]*ChatToolCall)
		var hasToolCalls bool

		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err == io.EOF {
				break
			}
			if err != nil {
				resp.Body.Close()
				c.releaseLLMSlot()
				if state.IsStopped() {
					state.SetDone()
				} else {
					state.SetError(fmt.Sprintf("failed to read stream: %v", err))
				}
				return
			}

			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if line == "data: [DONE]" {
				break
			}

			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			var event StreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			for _, choice := range event.Choices {
				if choice.FinishReason == "tool_calls" {
					hasToolCalls = true
				}
				if choice.Delta.Content != "" {
					assistantContent += choice.Delta.Content
					state.AppendText(choice.Delta.Content)
				}
				for _, tcDelta := range choice.Delta.ToolCalls {
					hasToolCalls = true
					tc, ok := toolCallAcc[tcDelta.Index]
					if !ok {
						tc = &ChatToolCall{
							ID:   tcDelta.ID,
							Type: tcDelta.Type,
						}
						toolCallAcc[tcDelta.Index] = tc
					}
					if tcDelta.Function.Name != "" {
						tc.Function.Name = tcDelta.Function.Name
					}
					tc.Function.Arguments += tcDelta.Function.Arguments
				}
			}

			if state.IsStopped() {
				log.Printf("[stream chat] request %s stopped by user during streaming", state.RequestID)
				resp.Body.Close()
				c.releaseLLMSlot()
				state.SetDone()
				return
			}
		}
		resp.Body.Close()
		c.releaseLLMSlot()

		// Распознаём текстовые вызовы инструментов (functions.name:idx {json}),
		// которые LLM может выдавать в потоковом режиме вместо поля tool_calls.
		textCalls, cleanedContent := extractToolCallsFromText(assistantContent)
		if len(textCalls) > 0 {
			hasToolCalls = true
			baseIdx := len(toolCallAcc)
			for i := range textCalls {
				toolCallAcc[baseIdx+i] = &textCalls[i]
			}
			assistantContent = cleanedContent
		}

		if !hasToolCalls || len(toolCallAcc) == 0 {
			state.CleanText(cleanToolCallArtifacts)
			state.SetDone()
			return
		}

		// Собираем вызовы инструментов в порядке индексов.
		toolCalls := make([]ChatToolCall, 0, len(toolCallAcc))
		for i := 0; i < len(toolCallAcc); i++ {
			if tc, ok := toolCallAcc[i]; ok {
				toolCalls = append(toolCalls, *tc)
			}
		}

		// Добавляем сообщение ассистента с вызовами инструментов.
		currentMessages = append(currentMessages, ChatMessage{
			Role:      "assistant",
			Content:   assistantContent,
			ToolCalls: toolCalls,
		})

		// Выполняем инструменты и добавляем результаты.
		for _, tc := range toolCalls {
			toolResult, err := executeTool(tc)
			if err != nil {
				toolResult = "Ошибка выполнения инструмента: " + err.Error()
				log.Printf("[stream chat] tool %s error: %s", tc.Function.Name, toolResult)
			} else {
				log.Printf("[stream chat] tool %s result length: %d", tc.Function.Name, len(toolResult))
			}
			currentMessages = append(currentMessages, ChatMessage{
				Role:       "tool",
				Content:    toolResult,
				ToolCallID: tc.ID,
			})
		}

		if state.IsStopped() {
			state.SetDone()
			return
		}
	}

	state.CleanText(cleanToolCallArtifacts)
	state.SetDone()
}

var textToolCallRegex = regexp.MustCompile(`functions\.([A-Za-z0-9_]+)(?::(\d+))?\s*(\{[\s\S]*?\})`)

// extractToolCallsFromText извлекает вызовы инструментов из plain-текста ответа LLM
// (форматы functions.name:idx {json} или functions.name {json}) и возвращает
// очищенный текст без этих артефактов.
func extractToolCallsFromText(content string) ([]ChatToolCall, string) {
	var calls []ChatToolCall
	cleaned := content

	for {
		match := textToolCallRegex.FindStringSubmatchIndex(cleaned)
		if match == nil {
			break
		}

		fullStart := match[0]
		fullEnd := match[1]
		name := cleaned[match[2]:match[3]]
		args := cleaned[match[6]:match[7]]

		// Проверяем, что аргументы — валидный JSON с сбалансированными скобками.
		if isBalancedJSON(args) {
			calls = append(calls, ChatToolCall{
				ID:   fmt.Sprintf("call_text_%d", len(calls)),
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      name,
					Arguments: args,
				},
			})
			cleaned = cleaned[:fullStart] + cleaned[fullEnd:]
		} else {
			// Если JSON не сбалансирован, удаляем только найденный фрагмент,
			// чтобы не зациклиться, и продолжаем поиск дальше.
			cleaned = cleaned[:fullStart] + cleaned[fullEnd:]
		}
	}

	return calls, strings.TrimSpace(cleaned)
}

// isBalancedJSON проверяет, что строка начинается с { и заканчивается }
// и фигурные скобки сбалансированы (без учёта строк).
func isBalancedJSON(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return false
	}
	depth := 0
	inString := false
	escaped := false
	for _, r := range s {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == '"' {
				inString = false
			}
			continue
		}
		if r == '"' {
			inString = true
			continue
		}
		if r == '{' {
			depth++
		}
		if r == '}' {
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && !inString
}

func isRetryableLLMError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	if netErr, ok := err.(net.Error); ok {
		return netErr.Temporary() || netErr.Timeout()
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "eof") || strings.Contains(text, "connection reset") || strings.Contains(text, "broken pipe")
}

func (c *LLMClient) doRequest(ctx context.Context, reqBody ChatRequest) ([]byte, error) {
	const maxAttempts = 3
	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := c.newRequest(ctx, reqBody)
		if err != nil {
			return nil, err
		}

		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("[LLM doRequest] attempt %d/%d failed: %v (body size %d)", attempt, maxAttempts, err, len(reqBody.Messages))
			if attempt < maxAttempts && isRetryableLLMError(err) {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			log.Printf("[LLM doRequest] non-retryable error: %v", err)
			return nil, fmt.Errorf("Не удалось получить ответ от языковой модели. Повторите запрос позже.")
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[LLM doRequest] failed to read response: %v", err)
			return nil, fmt.Errorf("Не удалось прочитать ответ от языковой модели. Повторите запрос позже.")
		}

		if resp.StatusCode != http.StatusOK {
			log.Printf("[LLM doRequest] LLM returned status %d: %s", resp.StatusCode, string(respBody))
			return nil, fmt.Errorf("Ошибка при обращении к языковой модели: код ответа %d.", resp.StatusCode)
		}

		log.Printf("[LLM doRequest] success in %v (attempt %d/%d, body size %d)", time.Since(start), attempt, maxAttempts, len(reqBody.Messages))
		return respBody, nil
	}
	log.Printf("[LLM doRequest] failed after %d attempts: %v, total time %v", maxAttempts, lastErr, time.Since(start))
	return nil, fmt.Errorf("Не удалось получить ответ от языковой модели. Повторите запрос позже.")
}

func (c *LLMClient) newRequest(ctx context.Context, reqBody ChatRequest) (*http.Request, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.LLM.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if apiKey := c.cfg.APIKey(); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	return req, nil
}
