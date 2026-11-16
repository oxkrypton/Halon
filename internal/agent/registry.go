package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

type Registry struct {
	level OperationLevel
	tools map[string]Tool
}

func NewRegistry(level OperationLevel, tools ...Tool) (*Registry, error) {
	registry := &Registry{level: level, tools: make(map[string]Tool)}
	for _, tool := range tools {
		if tool.Name == "" || tool.Description == "" || tool.Handler == nil {
			return nil, fmt.Errorf("tool %q must have a name, description, and handler", tool.Name)
		}
		if tool.Level == "" {
			tool.Level = LevelReadOnly
		}
		if _, err := ParseOperationLevel(string(tool.Level)); err != nil {
			return nil, err
		}
		if _, exists := registry.tools[tool.Name]; exists {
			return nil, fmt.Errorf("duplicate tool %q", tool.Name)
		}
		if level.Allows(tool.Level) {
			registry.tools[tool.Name] = tool
		}
	}
	return registry, nil
}

func (r *Registry) Definitions() []ToolDefinition {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	definitions := make([]ToolDefinition, 0, len(names))
	for _, name := range names {
		tool := r.tools[name]
		parameters := tool.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		definitions = append(definitions, ToolDefinition{
			Type: "function",
			Function: FunctionDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  parameters,
			},
		})
	}
	return definitions
}

func (r *Registry) Execute(ctx context.Context, call ToolCall) (string, ToolExecution) {
	execution := ToolExecution{
		ToolCallID: call.ID,
		Name:       call.Function.Name,
		Arguments:  call.Function.Arguments,
		Level:      LevelReadOnly,
	}
	tool, ok := r.tools[call.Function.Name]
	if !ok {
		execution.Error = fmt.Sprintf("tool %q is not available at the configured operation level", call.Function.Name)
		return errorJSON(execution.Error), execution
	}
	execution.Level = tool.Level
	if !r.level.Allows(tool.Level) {
		execution.Error = fmt.Sprintf("operation level %s does not allow tool %s", r.level, tool.Name)
		return errorJSON(execution.Error), execution
	}
	if !json.Valid([]byte(call.Function.Arguments)) {
		execution.Error = "tool arguments are not valid JSON"
		return errorJSON(execution.Error), execution
	}
	result, err := tool.Handler(ctx, json.RawMessage(call.Function.Arguments))
	if err != nil {
		execution.Error = err.Error()
		return errorJSON(err.Error()), execution
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		execution.Error = fmt.Sprintf("encode tool result: %v", err)
		return errorJSON(execution.Error), execution
	}
	execution.Output = string(encoded)
	return execution.Output, execution
}

func errorJSON(message string) string {
	encoded, _ := json.Marshal(map[string]string{"error": message})
	return string(encoded)
}
