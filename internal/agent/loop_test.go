package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oxkrypton/Halon/internal/incident"
)

type modelFunc func(context.Context, []Message, []ToolDefinition) (Message, Usage, error)

func (f modelFunc) Complete(ctx context.Context, messages []Message, tools []ToolDefinition) (Message, Usage, error) {
	return f(ctx, messages, tools)
}

func TestLoopExecutesToolsAndReturnsFinalReport(t *testing.T) {
	var observedMessages [][]Message
	model := modelFunc(func(_ context.Context, messages []Message, tools []ToolDefinition) (Message, Usage, error) {
		observedMessages = append(observedMessages, append([]Message(nil), messages...))
		if len(tools) != 1 || tools[0].Type != "function" || tools[0].Function.Name != "query_logs" {
			t.Fatalf("unexpected tools: %+v", tools)
		}
		if len(observedMessages) == 1 {
			return Message{ToolCalls: []ToolCall{{
				ID: "call-1", Type: "function",
				Function: FunctionCall{Name: "query_logs", Arguments: `{"path":"/api/v1/users/:id"}`},
			}}}, Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}, nil
		}
		last := messages[len(messages)-1]
		if last.Role != "tool" || last.ToolCallID != "call-1" || last.Content == nil || !strings.Contains(*last.Content, "deadline exceeded") {
			t.Fatalf("tool result was not returned to model: %+v", last)
		}
		return NewTextMessage("assistant", "## 事件摘要\n已确认上游超时"), Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25}, nil
	})
	tool := Tool{
		Name: "query_logs", Description: "Query logs", Level: LevelReadOnly,
		Parameters: map[string]any{"type": "object"},
		Handler: func(_ context.Context, arguments json.RawMessage) (any, error) {
			var input map[string]string
			if err := json.Unmarshal(arguments, &input); err != nil {
				return nil, err
			}
			return map[string]string{"path": input["path"], "error": "deadline exceeded"}, nil
		},
	}
	registry, err := NewRegistry(LevelReadOnly, tool)
	if err != nil {
		t.Fatal(err)
	}
	loop := NewLoop(model, registry, 4, 1024, 4096, testLogger())
	result, err := loop.Run(context.Background(), testEvent(), LevelReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if result.Steps != 2 || len(result.Executions) != 1 || !strings.Contains(result.FinalMessage, "事件摘要") {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Usage.TotalTokens != 37 || result.Usage.PromptTokens != 30 {
		t.Fatalf("unexpected usage: %+v", result.Usage)
	}
}

func TestRegistryEnforcesOperationLevel(t *testing.T) {
	readTool := Tool{Name: "read", Description: "Read", Handler: func(context.Context, json.RawMessage) (any, error) {
		return "ok", nil
	}}
	restartTool := Tool{Name: "restart", Description: "Restart", Level: LevelOperator, Handler: func(context.Context, json.RawMessage) (any, error) {
		return "restarted", nil
	}}
	registry, err := NewRegistry(LevelReadOnly, readTool, restartTool)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Definitions()) != 1 {
		t.Fatalf("readonly registry exposed %d tools, want 1", len(registry.Definitions()))
	}
	output, execution := registry.Execute(context.Background(), ToolCall{ID: "denied", Function: FunctionCall{Name: "restart", Arguments: `{}`}})
	if execution.Error == "" || !strings.Contains(output, "not available") {
		t.Fatalf("restricted tool execution = %q, %+v", output, execution)
	}

	operatorRegistry, _ := NewRegistry(LevelOperator, readTool, restartTool)
	if len(operatorRegistry.Definitions()) != 2 {
		t.Fatalf("operator registry exposed %d tools, want 2", len(operatorRegistry.Definitions()))
	}
	output, execution = operatorRegistry.Execute(context.Background(), ToolCall{ID: "allowed", Function: FunctionCall{Name: "restart", Arguments: `{}`}})
	if execution.Error != "" || output != `"restarted"` {
		t.Fatalf("operator execution = %q, %+v", output, execution)
	}
}

func TestLoopReturnsToolErrorsToModel(t *testing.T) {
	calls := 0
	model := modelFunc(func(_ context.Context, messages []Message, _ []ToolDefinition) (Message, Usage, error) {
		calls++
		if calls == 1 {
			return Message{ToolCalls: []ToolCall{{ID: "failure", Function: FunctionCall{Name: "read", Arguments: `{}`}}}}, Usage{}, nil
		}
		last := messages[len(messages)-1]
		if last.Content == nil || !strings.Contains(*last.Content, "database unavailable") {
			t.Fatalf("missing tool error: %+v", last)
		}
		return NewTextMessage("assistant", "无法确定根因"), Usage{}, nil
	})
	registry, _ := NewRegistry(LevelReadOnly, Tool{Name: "read", Description: "Read", Handler: func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("database unavailable")
	}})
	result, err := NewLoop(model, registry, 3, 1024, 4096, testLogger()).Run(context.Background(), testEvent(), LevelReadOnly)
	if err != nil || result.FinalMessage != "无法确定根因" || result.Executions[0].Error == "" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestLoopStopsAtMaxSteps(t *testing.T) {
	model := modelFunc(func(context.Context, []Message, []ToolDefinition) (Message, Usage, error) {
		return Message{ToolCalls: []ToolCall{{ID: "again", Function: FunctionCall{Name: "read", Arguments: `{}`}}}}, Usage{}, nil
	})
	registry, _ := NewRegistry(LevelReadOnly, Tool{Name: "read", Description: "Read", Handler: func(context.Context, json.RawMessage) (any, error) {
		return map[string]bool{"ok": true}, nil
	}})
	result, err := NewLoop(model, registry, 2, 1024, 4096, testLogger()).Run(context.Background(), testEvent(), LevelReadOnly)
	if !errors.Is(err, ErrMaxSteps) || result.Steps != 2 || len(result.Executions) != 2 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestTruncateToolOutput(t *testing.T) {
	output := truncateToolOutput("0123456789", 4)
	if output != "0123\n...[tool output truncated]" {
		t.Fatalf("truncated output = %q", output)
	}
}

func testEvent() incident.TimeoutEvent {
	return incident.TimeoutEvent{
		EventID: "event-1", RequestID: "request-1", OccurredAt: time.Now().UTC(),
		Method: http.MethodGet, Path: "/api/v1/users/1", Route: "/api/v1/users/:id",
		StatusCode: 504, LatencyMS: 3000,
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLoopPersistsFullToolOutputBeyondModelLimit(t *testing.T) {
	full := strings.Repeat("x", 5000)
	calls := 0
	modelView := ""
	model := modelFunc(func(_ context.Context, messages []Message, _ []ToolDefinition) (Message, Usage, error) {
		calls++
		if calls == 1 {
			return Message{ToolCalls: []ToolCall{{ID: "big", Function: FunctionCall{Name: "read", Arguments: `{}`}}}}, Usage{}, nil
		}
		last := messages[len(messages)-1]
		if last.Role != "tool" || last.Content == nil {
			t.Fatalf("expected tool message, got %+v", last)
		}
		modelView = *last.Content
		return NewTextMessage("assistant", "done"), Usage{}, nil
	})
	registry, _ := NewRegistry(LevelReadOnly, Tool{Name: "read", Description: "Read", Handler: func(context.Context, json.RawMessage) (any, error) {
		return full, nil
	}})
	result, err := NewLoop(model, registry, 3, 1024, 8192, testLogger()).Run(context.Background(), testEvent(), LevelReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(modelView, "[tool output truncated]") || len(modelView) > 1024+64 {
		t.Fatalf("model view not capped at context limit: %d bytes", len(modelView))
	}
	stored := result.Executions[0].Output
	if !strings.Contains(stored, full) || result.Executions[0].OutputTruncated {
		t.Fatalf("stored output lost evidence: %d bytes, truncated=%v", len(stored), result.Executions[0].OutputTruncated)
	}
}

func TestLoopMarksStoredToolOutputTruncated(t *testing.T) {
	full := strings.Repeat("y", 5000)
	calls := 0
	model := modelFunc(func(_ context.Context, _ []Message, _ []ToolDefinition) (Message, Usage, error) {
		calls++
		if calls == 1 {
			return Message{ToolCalls: []ToolCall{{ID: "big", Function: FunctionCall{Name: "read", Arguments: `{}`}}}}, Usage{}, nil
		}
		return NewTextMessage("assistant", "done"), Usage{}, nil
	})
	registry, _ := NewRegistry(LevelReadOnly, Tool{Name: "read", Description: "Read", Handler: func(context.Context, json.RawMessage) (any, error) {
		return full, nil
	}})
	result, err := NewLoop(model, registry, 3, 1024, 2048, testLogger()).Run(context.Background(), testEvent(), LevelReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	execution := result.Executions[0]
	if !execution.OutputTruncated || !strings.Contains(execution.Output, "[tool output truncated]") || len(execution.Output) > 2048+64 {
		t.Fatalf("stored output = %d bytes, truncated=%v", len(execution.Output), execution.OutputTruncated)
	}
}
