package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Model interface {
	Complete(context.Context, []Message, []ToolDefinition) (Message, Usage, error)
}

type OpenAIClient struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

func NewOpenAIClient(baseURL, apiKey, model string, timeout time.Duration) *OpenAIClient {
	return &OpenAIClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: timeout},
	}
}

type chatCompletionRequest struct {
	Model      string           `json:"model"`
	Messages   []Message        `json:"messages"`
	Tools      []ToolDefinition `json:"tools,omitempty"`
	ToolChoice string           `json:"tool_choice,omitempty"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (c *OpenAIClient) Complete(ctx context.Context, messages []Message, tools []ToolDefinition) (Message, Usage, error) {
	if c.apiKey == "" {
		return Message{}, Usage{}, errors.New("OPENAI_API_KEY is required")
	}
	if c.model == "" {
		return Message{}, Usage{}, errors.New("OPENAI_MODEL is required")
	}
	requestBody := chatCompletionRequest{Model: c.model, Messages: messages, Tools: tools}
	if len(tools) > 0 {
		requestBody.ToolChoice = "auto"
	}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return Message{}, Usage{}, err
	}

	var lastError error
	retryAfter := ""
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, retryDelay(attempt, retryAfter)); err != nil {
				return Message{}, Usage{}, err
			}
		}
		response, err := c.do(ctx, payload)
		if err == nil {
			return response.message, response.usage, nil
		}
		lastError = err
		var responseError *openAIResponseError
		if errors.As(err, &responseError) {
			retryAfter = responseError.retryAfter
			if responseError.statusCode < 500 && responseError.statusCode != http.StatusTooManyRequests {
				return Message{}, Usage{}, err
			}
		}
	}
	return Message{}, Usage{}, lastError
}

type completionResult struct {
	message Message
	usage   Usage
}

type openAIResponseError struct {
	statusCode int
	message    string
	retryAfter string
}

func (e *openAIResponseError) Error() string {
	return fmt.Sprintf("OpenAI API returned %d: %s", e.statusCode, e.message)
}

func (c *OpenAIClient) do(ctx context.Context, payload []byte) (completionResult, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return completionResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "halon-agent/1.0")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return completionResult{}, fmt.Errorf("call OpenAI-compatible API: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return completionResult{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var decoded chatCompletionResponse
		_ = json.Unmarshal(body, &decoded)
		message := strings.TrimSpace(string(body))
		if decoded.Error != nil {
			message = decoded.Error.Message
		}
		if len(message) > 4096 {
			message = message[:4096]
		}
		return completionResult{}, &openAIResponseError{
			statusCode: response.StatusCode,
			message:    message,
			retryAfter: response.Header.Get("Retry-After"),
		}
	}
	var decoded chatCompletionResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return completionResult{}, fmt.Errorf("decode OpenAI-compatible response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return completionResult{}, errors.New("OpenAI-compatible response has no choices")
	}
	message := decoded.Choices[0].Message
	for i := range message.ToolCalls {
		if message.ToolCalls[i].Type == "" {
			message.ToolCalls[i].Type = "function"
		}
	}
	return completionResult{message: message, usage: decoded.Usage}, nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryDelay(attempt int, retryAfter string) time.Duration {
	if retryAfter != "" {
		if seconds, err := strconv.Atoi(retryAfter); err == nil {
			return min(time.Duration(seconds)*time.Second, 5*time.Second)
		}
	}
	return min(time.Duration(1<<attempt)*200*time.Millisecond, 2*time.Second)
}
