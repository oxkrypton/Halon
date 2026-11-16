package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oxkrypton/Halon/internal/incident"
)

const DefaultSystemPrompt = `You are Halon's Kubernetes incident-response agent. Investigate the supplied gateway timeout with observable evidence before making changes.

Rules:
1. Start with traffic logs, error details, Prometheus metrics, pod state, events, and recent logs. Correlate timestamps and request IDs.
2. Distinguish evidence, inference, and uncertainty. Never invent metrics, commands, actions, or a root cause.
3. Use only the tools exposed in this run. Their availability is enforced by the configured operation level.
4. Prefer the smallest reversible remediation. Do not restart or scale repeatedly, and do not mutate anything without supporting evidence.
5. After a mutation, verify the resulting state and say whether the incident was actually resolved.
6. Finish without a tool call when the investigation is complete. Write the final response as a Chinese Markdown report with these sections: 事件摘要、排查过程、根因分析、证据、执行操作、修复验证、后续建议. If evidence is insufficient, explicitly say 无法确定根因.`

type Loop struct {
	model           Model
	registry        *Registry
	maxSteps        int
	maxToolOutput   int
	maxStoredOutput int
	logger          *slog.Logger
}

func NewLoop(model Model, registry *Registry, maxSteps, maxToolOutput, maxStoredOutput int, logger *slog.Logger) *Loop {
	if logger == nil {
		logger = slog.Default()
	}
	return &Loop{model: model, registry: registry, maxSteps: maxSteps, maxToolOutput: maxToolOutput, maxStoredOutput: maxStoredOutput, logger: logger}
}

func (l *Loop) Run(ctx context.Context, event incident.TimeoutEvent, level OperationLevel) (RunResult, error) {
	result := RunResult{Incident: event}
	if err := event.Validate(); err != nil {
		return result, err
	}
	eventJSON, _ := json.Marshal(event)
	messages := []Message{
		NewTextMessage("system", DefaultSystemPrompt),
		NewTextMessage("user", fmt.Sprintf("Operation level: %s\nInvestigate this gateway timeout event:\n%s", level, eventJSON)),
	}

	for step := 1; step <= l.maxSteps; step++ {
		result.Steps = step
		assistant, usage, err := l.model.Complete(ctx, messages, l.registry.Definitions())
		result.Usage.PromptTokens += usage.PromptTokens
		result.Usage.CompletionTokens += usage.CompletionTokens
		result.Usage.TotalTokens += usage.TotalTokens
		if err != nil {
			return result, err
		}
		if assistant.Role == "" {
			assistant.Role = "assistant"
		}
		messages = append(messages, assistant)
		if len(assistant.ToolCalls) == 0 {
			if assistant.Content == nil || strings.TrimSpace(*assistant.Content) == "" {
				return result, errors.New("model returned an empty final response")
			}
			result.FinalMessage = *assistant.Content
			return result, nil
		}

		for _, call := range assistant.ToolCalls {
			started := time.Now()
			output, execution := l.registry.Execute(ctx, call)
			execution.StartedAt = started.UTC()
			execution.DurationMS = float64(time.Since(started)) / float64(time.Millisecond)
			// Persist against the generous storage cap so incident reports
			// keep full evidence; only the model feedback message is bound
			// by the smaller context cap.
			execution.Output = truncateToolOutput(output, l.maxStoredOutput)
			execution.OutputTruncated = l.maxStoredOutput > 0 && len(output) > l.maxStoredOutput
			result.Executions = append(result.Executions, execution)
			l.logger.Info("agent tool executed",
				"event_id", event.EventID,
				"step", step,
				"tool", call.Function.Name,
				"level", execution.Level,
				"duration_ms", execution.DurationMS,
				"error", execution.Error,
			)
			messages = append(messages, Message{
				Role:       "tool",
				Content:    stringPointer(truncateToolOutput(output, l.maxToolOutput)),
				ToolCallID: call.ID,
				Name:       call.Function.Name,
			})
		}
	}
	return result, ErrMaxSteps
}

func truncateToolOutput(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	cutoff := limit
	for cutoff > 0 && !utf8.ValidString(value[:cutoff]) {
		cutoff--
	}
	return value[:cutoff] + "\n...[tool output truncated]"
}

func stringPointer(value string) *string {
	return &value
}
