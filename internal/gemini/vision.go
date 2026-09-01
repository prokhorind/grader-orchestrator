package gemini

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/prokhorind/classroom-grader/internal/vision"
)

// VisionClient uses the Gemini multimodal API to extract plain text from PDF
// or image files.  It sends the raw file bytes as inline data so no separate
// file upload step is needed.
// It implements vision.Client.
type VisionClient struct {
	apiKey  string
	model   string
	timeout time.Duration
}

// NewVisionClient creates a Gemini VisionClient.
// model should be a vision-capable model such as "gemini-2.5-flash" or
// "gemini-2.0-flash".  apiKey is the Gemini API key.
func NewVisionClient(apiKey, model string, timeout time.Duration) *VisionClient {
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	return &VisionClient{
		apiKey:  apiKey,
		model:   model,
		timeout: timeout,
	}
}

// ExtractText reads filePath, detects its MIME type, and sends the raw bytes
// inline to the Gemini model together with an extraction prompt.
// Gemini natively supports PDF and common image formats (PNG, JPEG, WebP, etc.).
func (c *VisionClient) ExtractText(ctx context.Context, filePath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("reading file %s: %w", filePath, err)
	}

	mimeType := geminiMIMEType(filePath)

	sdkClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  c.apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return "", fmt.Errorf("creating Gemini client: %w", err)
	}

	// Build a single user turn with an inline blob (the file) + text prompt.
	contents := []*genai.Content{
		{
			Role: "user",
			Parts: []*genai.Part{
				{
					InlineData: &genai.Blob{
						MIMEType: mimeType,
						Data:     data,
					},
				},
				{Text: vision.ExtractPrompt},
			},
		},
	}

	temp := float32(0.0)
	cfg := &genai.GenerateContentConfig{
		Temperature: &temp,
	}

	resp, err := sdkClient.Models.GenerateContent(ctx, c.model, contents, cfg)
	if err != nil {
		return "", fmt.Errorf("Gemini vision GenerateContent: %w", err)
	}

	text := resp.Text()
	if text == "" {
		return "", fmt.Errorf("Gemini vision returned an empty response for %s", filepath.Base(filePath))
	}
	return strings.TrimSpace(text), nil
}

// geminiMIMEType maps file extensions to MIME types accepted by the Gemini API.
// https://ai.google.dev/gemini-api/docs/vision#supported-formats
func geminiMIMEType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".heic":
		return "image/heic"
	case ".heif":
		return "image/heif"
	default:
		return "application/octet-stream"
	}
}

// Ensure VisionClient satisfies vision.Client at compile time.
var _ vision.Client = (*VisionClient)(nil)
