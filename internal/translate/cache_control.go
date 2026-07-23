package translate

import (
	"encoding/json"
	"errors"
	"fmt"
)

var (
	// ErrAnthropicCacheControlInvalid marks an explicit cache policy Anthropic
	// would reject. It is never silently stripped or rewritten.
	ErrAnthropicCacheControlInvalid = errors.New("invalid Anthropic cache_control")
	// ErrAnthropicCacheControlOverflow marks explicit client breakpoints beyond
	// the provider's supported capacity.
	ErrAnthropicCacheControlOverflow = errors.New("Anthropic cache_control breakpoint capacity exceeded")
)

const anthropicCacheControlCapacity = 4

// applyAnthropicCachePolicy validates client controls and uses only spare
// provider capacity for router-generated cache breakpoints.
func applyAnthropicCachePolicy(body []byte, injectRouterBreakpoints bool) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode Anthropic cache policy: %w", err)
	}
	count, err := validateAnthropicCacheControls(request)
	if err != nil {
		return nil, err
	}
	if count > anthropicCacheControlCapacity {
		return nil, fmt.Errorf("%w: got %d, maximum is %d", ErrAnthropicCacheControlOverflow, count, anthropicCacheControlCapacity)
	}
	if !injectRouterBreakpoints {
		return body, nil
	}
	remaining := anthropicCacheControlCapacity - count
	if remaining == 0 {
		return body, nil
	}
	if addCacheControlToLastSystemBlock(request) {
		remaining--
	}
	if remaining > 0 {
		addCacheControlToLastMessageBlock(request)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode Anthropic cache policy: %w", err)
	}
	return encoded, nil
}

func validateAnthropicCacheControls(request map[string]any) (int, error) {
	count := 0
	visit := func(block any) error {
		object, ok := block.(map[string]any)
		if !ok {
			return nil
		}
		policy, exists := object["cache_control"]
		if !exists {
			return nil
		}
		if err := validateAnthropicCacheControl(policy); err != nil {
			return err
		}
		count++
		return nil
	}
	if tools, ok := request["tools"].([]any); ok {
		for _, tool := range tools {
			if err := visit(tool); err != nil {
				return 0, err
			}
		}
	}
	if system, ok := request["system"].([]any); ok {
		for _, block := range system {
			if err := visit(block); err != nil {
				return 0, err
			}
		}
	}
	if messages, ok := request["messages"].([]any); ok {
		for _, message := range messages {
			object, _ := message.(map[string]any)
			blocks, _ := object["content"].([]any)
			for _, block := range blocks {
				if err := visit(block); err != nil {
					return 0, err
				}
			}
		}
	}
	return count, nil
}

func validateAnthropicCacheControl(v any) error {
	policy, ok := v.(map[string]any)
	if !ok || policy["type"] != "ephemeral" {
		return fmt.Errorf("%w: expected type=ephemeral", ErrAnthropicCacheControlInvalid)
	}
	for key, value := range policy {
		switch key {
		case "type":
		case "ttl":
			ttl, ok := value.(string)
			if !ok || (ttl != "5m" && ttl != "1h") {
				return fmt.Errorf("%w: ttl must be 5m or 1h", ErrAnthropicCacheControlInvalid)
			}
		default:
			return fmt.Errorf("%w: unsupported field %q", ErrAnthropicCacheControlInvalid, key)
		}
	}
	return nil
}

func addCacheControlToLastSystemBlock(request map[string]any) bool {
	system, ok := request["system"].([]any)
	if !ok || len(system) == 0 {
		return false
	}
	last, ok := system[len(system)-1].(map[string]any)
	if !ok {
		return false
	}
	if _, exists := last["cache_control"]; exists {
		return false
	}
	last["cache_control"] = map[string]any{"type": "ephemeral"}
	return true
}

func addCacheControlToLastMessageBlock(request map[string]any) bool {
	messages, ok := request["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	message, ok := messages[len(messages)-1].(map[string]any)
	if !ok {
		return false
	}
	blocks, ok := message["content"].([]any)
	if !ok {
		if content, ok := message["content"].(string); ok {
			blocks = []any{map[string]any{"type": "text", "text": content}}
			message["content"] = blocks
		} else {
			return false
		}
	}
	if len(blocks) == 0 {
		return false
	}
	last, ok := blocks[len(blocks)-1].(map[string]any)
	if !ok {
		return false
	}
	if _, exists := last["cache_control"]; exists {
		return false
	}
	last["cache_control"] = map[string]any{"type": "ephemeral"}
	return true
}
