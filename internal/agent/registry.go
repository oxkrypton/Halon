package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/oxkrypton/Halon/internal/authz"
)

type Registry struct {
	role  authz.Role
	tools map[string]Tool
}

func NewRegistry(level OperationLevel, tools ...Tool) (*Registry, error) {
	role, err := authz.ParseRole(string(level))
	if err != nil {
		return nil, err
	}
	registry := &Registry{role: role, tools: make(map[string]Tool)}
	for _, tool := range tools {
		if tool.Name == "" || tool.Description == "" || tool.Handler == nil {
			return nil, fmt.Errorf("tool %q must have a name, description, and handler", tool.Name)
		}
		if tool.Permission == "" {
			tool.Permission = authz.PermToolObserve
		}
		if _, err := authz.ParsePermission(string(tool.Permission)); err != nil {
			return nil, fmt.Errorf("tool %q: %w", tool.Name, err)
		}
		if _, exists := registry.tools[tool.Name]; exists {
			return nil, fmt.Errorf("duplicate tool %q", tool.Name)
		}
		if role.Allows(tool.Permission) {
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
	if required, ok := tool.Permission.RequiredRole(); ok {
		execution.Level = OperationLevel(required.String())
	}
	// Re-check at execution time: Definitions() already filters tools, but a
	// stale tool call from the model must never bypass a permission change.
	if !r.role.Allows(tool.Permission) {
		execution.Error = fmt.Sprintf("operation level %s does not allow tool %s", r.role, tool.Name)
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
