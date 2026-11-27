package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/incident"
)

type OperationLevel string

const (
	LevelReadOnly OperationLevel = "readonly"
	LevelOperator OperationLevel = "operator"
	LevelFull     OperationLevel = "full"
)

func ParseOperationLevel(value string) (OperationLevel, error) {
	level := OperationLevel(strings.ToLower(strings.TrimSpace(value)))
	switch level {
	case LevelReadOnly, LevelOperator, LevelFull:
		return level, nil
	default:
		return "", fmt.Errorf("unsupported operation level %q", value)
	}
}

func (l OperationLevel) Allows(required OperationLevel) bool {
	return levelRank(l) >= levelRank(required)
}

func levelRank(level OperationLevel) int {
	switch level {
	case LevelFull:
		return 2
	case LevelOperator:
		return 1
	default:
		return 0
	}
}

type ToolDefinition struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

type FunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Permission  authz.Permission
	Handler     ToolHandler
}

type ToolHandler func(context.Context, json.RawMessage) (any, error)

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

func NewTextMessage(role, content string) Message {
	return Message{Role: role, Content: &content}
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ToolExecution struct {
	ToolCallID string         `json:"tool_call_id"`
	Name       string         `json:"name"`
	Arguments  string         `json:"arguments"`
	Level      OperationLevel `json:"level"`
	Output     string         `json:"output"`
	// OutputTruncated is true when Output was capped by the persistence
	// limit (AGENT_MAX_STORED_TOOL_OUTPUT). The smaller model-context cap
	// (AGENT_MAX_TOOL_OUTPUT) only applies to the message fed back to the
	// model and never affects this persisted record.
	OutputTruncated bool      `json:"output_truncated,omitempty"`
	Error           string    `json:"error,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	DurationMS      float64   `json:"duration_ms"`
}

type RunResult struct {
	Incident     incident.Event  `json:"incident"`
	FinalMessage string          `json:"final_message"`
	Steps        int             `json:"steps"`
	Executions   []ToolExecution `json:"executions"`
	Usage        Usage           `json:"usage"`
}

var ErrMaxSteps = errors.New("agent reached maximum model steps")
