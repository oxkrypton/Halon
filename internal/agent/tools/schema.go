package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxBatchQueries = 8

type batchItemResult struct {
	Source string `json:"source"`
	Data   any    `json:"data,omitempty"`
	Error  string `json:"error,omitempty"`
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func enumStringProperty(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

func booleanProperty(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func integerProperty(description string, minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "description": description, "minimum": minimum, "maximum": maximum}
}

func batchQuerySchema(itemProperties map[string]any, description string) map[string]any {
	return objectSchema(map[string]any{
		"queries": map[string]any{
			"type":        "array",
			"description": description,
			"minItems":    1,
			"maxItems":    maxBatchQueries,
			"items":       objectSchema(itemProperties, "source"),
		},
	}, "queries")
}

func decodeArguments[T any](arguments json.RawMessage) (T, error) {
	var input T
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("decode tool arguments: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return input, fmt.Errorf("tool arguments must contain one JSON object")
	}
	return input, nil
}

func decodeBatchQueries(raw json.RawMessage) ([]json.RawMessage, error) {
	input, err := decodeArguments[struct {
		Queries []json.RawMessage `json:"queries"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if len(input.Queries) == 0 || len(input.Queries) > maxBatchQueries {
		return nil, fmt.Errorf("queries must contain 1..%d items", maxBatchQueries)
	}
	return input.Queries, nil
}

func extractQuerySource(raw json.RawMessage) (string, error) {
	var envelope struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("decode query item: %w", err)
	}
	source := strings.TrimSpace(envelope.Source)
	if source == "" {
		return "", errors.New("source is required")
	}
	return source, nil
}

func decodeSourceQuery[T any](raw json.RawMessage, source, allowedFields string) (T, error) {
	input, err := decodeArguments[T](raw)
	if err != nil {
		return input, fmt.Errorf("invalid %s query: %w; allowed fields: %s", source, err, allowedFields)
	}
	return input, nil
}

func runBatchQueries(raw json.RawMessage, dispatch func(source string, item json.RawMessage) (any, error)) (any, error) {
	items, err := decodeBatchQueries(raw)
	if err != nil {
		return nil, err
	}
	results := make([]batchItemResult, 0, len(items))
	var itemErrors []error
	succeeded := 0
	for _, item := range items {
		source, err := extractQuerySource(item)
		if err != nil {
			results = append(results, batchItemResult{Source: source, Error: err.Error()})
			itemErrors = append(itemErrors, err)
			continue
		}
		data, err := dispatch(source, item)
		if err != nil {
			results = append(results, batchItemResult{Source: source, Error: err.Error()})
			itemErrors = append(itemErrors, err)
			continue
		}
		succeeded++
		results = append(results, batchItemResult{Source: source, Data: data})
	}
	if succeeded == 0 {
		if len(itemErrors) == 1 {
			return nil, itemErrors[0]
		}
		return nil, errors.Join(itemErrors...)
	}
	return map[string]any{"results": results}, nil
}

func queryContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, queryTimeout)
}
