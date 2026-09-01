// Package lmstudio provides a minimal OpenAI-compatible client for LM Studio's
// local inference server (default: http://localhost:1234/v1).
// It implements llm.Client so it can be used interchangeably with other backends.
package lmstudio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/prokhorind/classroom-grader/internal/llm"
)

// Client talks to LM Studio's /v1/chat/completions endpoint.
type Client struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

// NewClient creates a client pointing at baseURL (e.g. "http://localhost:1234/v1").
// model is the model identifier shown in LM Studio (e.g. "lmstudio-community/Meta-Llama-3-8B-Instruct-GGUF").
// Pass an empty model string to use whatever model LM Studio has loaded.
func NewClient(baseURL, model string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	return &Client{
		baseURL: baseURL,
		model:   model,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// wireMessage is the JSON shape sent to /v1/chat/completions.
// We keep this internal so the public API uses the shared llm.Message type.
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the JSON body sent to /v1/chat/completions.
type chatRequest struct {
	Model       string        `json:"model,omitempty"`
	Messages    []wireMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

// chatResponse is the subset of the OpenAI response we care about.
type chatResponse struct {
	Choices []struct {
		Message wireMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// modelsResponse is the subset of the OpenAI /v1/models response we need.
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// resolveModel returns c.model if set, otherwise queries /v1/models and
// returns the first loaded model's ID.  This handles the case where LM Studio
// has multiple models loaded and requires an explicit model field.
func (c *Client) resolveModel(ctx context.Context) (string, error) {
	if c.model != "" {
		return c.model, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return "", fmt.Errorf("building models request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// If we can't reach /v1/models just fall back to empty — LM Studio
		// with a single model will still accept an empty model field.
		return "", nil
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", nil // best-effort; let the chat call surface any real error
	}

	var mr modelsResponse
	if err := json.Unmarshal(raw, &mr); err != nil || len(mr.Data) == 0 {
		return "", nil
	}

	return mr.Data[0].ID, nil
}

// Complete sends messages to LM Studio and returns the assistant reply text.
// It implements llm.Client.
func (c *Client) Complete(ctx context.Context, messages []llm.Message) (string, error) {
	wire := make([]wireMessage, len(messages))
	for i, m := range messages {
		wire[i] = wireMessage{Role: m.Role, Content: m.Content}
	}

	model, err := c.resolveModel(ctx)
	if err != nil {
		return "", fmt.Errorf("resolving model: %w", err)
	}

	reqBody := chatRequest{
		Model:       model,
		Messages:    wire,
		Temperature: 0.1, // low temp for deterministic grading
		Stream:      false,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshalling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling LM Studio at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LM Studio returned HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var result chatResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}

	if result.Error != nil {
		return "", fmt.Errorf("LM Studio error: %s", result.Error.Message)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("LM Studio returned no choices")
	}

	return result.Choices[0].Message.Content, nil
}

// Ensure Client implements the llm.Client interface at compile time.
var _ interface {
	Complete(ctx context.Context, messages []llm.Message) (string, error)
} = (*Client)(nil)
