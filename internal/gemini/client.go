// Package gemini provides a Google Gemini backend that implements llm.Client.
// It uses the official google.golang.org/genai SDK and talks to the Gemini API
// with an API key (BackendGeminiAPI).
package gemini

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/prokhorind/classroom-grader/internal/llm"
)

// DefaultModel is the fallback used when no model has been explicitly selected.
// This is intentionally left empty so the server requires an explicit model
// choice from the live /api/gemini-models list rather than guessing.
const DefaultModel = ""

// Client wraps the Gemini SDK and implements llm.Client.
type Client struct {
	apiKey  string
	model   string
	timeout time.Duration
}

// NewClient creates a Gemini client.
// Both apiKey and model are required. model should be the short name as
// returned by ListModels (e.g. "gemini-2.5-flash"), without the "models/" prefix —
// the SDK adds the prefix automatically.
func NewClient(apiKey, model string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	return &Client{apiKey: apiKey, model: model, timeout: timeout}
}

// Complete implements llm.Client.
// It treats the first message with role "system" as the system instruction and
// sends the remaining messages as conversation contents.
func (c *Client) Complete(ctx context.Context, messages []llm.Message) (string, error) {
	// Apply the per-call timeout.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	sdkClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  c.apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return "", fmt.Errorf("creating Gemini client: %w", err)
	}

	// Separate the system prompt from the conversation turns.
	var systemText string
	var contents []*genai.Content
	for _, m := range messages {
		switch strings.ToLower(m.Role) {
		case "system":
			systemText = m.Content
		case "user":
			contents = append(contents, &genai.Content{
				Role:  "user",
				Parts: []*genai.Part{{Text: m.Content}},
			})
		case "assistant", "model":
			contents = append(contents, &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{{Text: m.Content}},
			})
		}
	}

	temp := float32(0.1) // low temperature for deterministic grading
	cfg := &genai.GenerateContentConfig{
		Temperature: &temp,
	}
	if systemText != "" {
		cfg.SystemInstruction = &genai.Content{
			Parts: []*genai.Part{{Text: systemText}},
		}
	}

	resp, err := sdkClient.Models.GenerateContent(ctx, c.model, contents, cfg)
	if err != nil {
		return "", fmt.Errorf("Gemini GenerateContent: %w", err)
	}

	text := resp.Text()
	if text == "" {
		return "", fmt.Errorf("Gemini returned an empty response")
	}
	return text, nil
}

// ModelInfo is a trimmed view of a Gemini model returned by ListModels.
type ModelInfo struct {
	// Name is the model identifier as returned by the API (e.g. "models/gemini-2.5-flash-latest").
	Name string `json:"name"`
	// ShortName strips the "models/" prefix for convenient use as a model ID.
	ShortName string `json:"short_name"`
	// DisplayName is the human-readable label (e.g. "Gemini 2.5 Flash").
	DisplayName string `json:"display_name"`
}

// ListModels fetches all models that support generateContent from the Gemini API
// and returns them sorted alphabetically by name.
// Only an API key is required — no model is needed for this call.
func ListModels(ctx context.Context, apiKey string) ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	sdkClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("creating Gemini client: %w", err)
	}

	// All() handles pagination internally and yields every model.
	var models []ModelInfo
	for m, err := range sdkClient.Models.All(ctx) {
		if err != nil {
			return nil, fmt.Errorf("listing Gemini models: %w", err)
		}
		// Only expose models that support content generation.
		if !supportsAction(m.SupportedActions, "generateContent") {
			continue
		}
		short := strings.TrimPrefix(m.Name, "models/")
		models = append(models, ModelInfo{
			Name:        m.Name,
			ShortName:   short,
			DisplayName: m.DisplayName,
		})
	}
	return models, nil
}

// supportsAction reports whether the given action string is present in the list.
func supportsAction(actions []string, target string) bool {
	for _, a := range actions {
		if strings.EqualFold(a, target) {
			return true
		}
	}
	return false
}
