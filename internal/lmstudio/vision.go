package lmstudio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/prokhorind/classroom-grader/internal/vision"
)

// VisionClient sends image/PDF files to the qwen/qwen3-vl-8b model running
// in LM Studio and returns the extracted plain text.
// It implements vision.Client.
type VisionClient struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

// VisionModel is the recommended model identifier for vision/OCR tasks in LM Studio.
const VisionModel = "qwen/qwen3-vl-8b"

// NewVisionClient creates a VisionClient.
// baseURL should be the LM Studio base (e.g. "http://localhost:1234/v1").
// model is the exact model identifier loaded in LM Studio; pass VisionModel
// ("qwen/qwen3-vl-8b") or whatever vision model you have available.
func NewVisionClient(baseURL, model string, timeout time.Duration) *VisionClient {
	if model == "" {
		model = VisionModel
	}
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	return &VisionClient{
		baseURL: baseURL,
		model:   model,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// --- OpenAI multimodal wire types ----------------------------------------

type visionContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *visionImageURL `json:"image_url,omitempty"`
}

type visionImageURL struct {
	URL string `json:"url"`
}

type visionMessage struct {
	Role    string              `json:"role"`
	Content []visionContentPart `json:"content"`
}

type visionRequest struct {
	Model       string          `json:"model,omitempty"`
	Messages    []visionMessage `json:"messages"`
	Temperature float64         `json:"temperature"`
	Stream      bool            `json:"stream"`
}

type visionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// --- vision.Client implementation ----------------------------------------

// ExtractText reads filePath and sends it to the vision model.
// PDFs are converted page-by-page to PNG images via pdftoppm (poppler),
// then each page is sent to the model and the results are concatenated.
// Images are sent directly as base64 data URIs.
func (c *VisionClient) ExtractText(ctx context.Context, filePath string) (string, error) {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == ".pdf" {
		return c.extractPDF(ctx, filePath)
	}
	return c.extractImage(ctx, filePath)
}

// extractPDF converts a PDF to per-page PNG images using pdftoppm, then runs
// OCR on each page and returns all text concatenated.
func (c *VisionClient) extractPDF(ctx context.Context, pdfPath string) (string, error) {
	// Create a temp dir for the page images.
	tmpDir, err := os.MkdirTemp("", "pdf-pages-*")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	outPrefix := filepath.Join(tmpDir, "page")

	// pdftoppm -r 150 -png input.pdf outPrefix  →  outPrefix-1.png, outPrefix-2.png …
	cmd := exec.CommandContext(ctx, "pdftoppm", "-r", "150", "-png", pdfPath, outPrefix)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("pdftoppm failed: %w\n%s", err, string(out))
	}

	// Collect page images in order.
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return "", fmt.Errorf("reading temp dir: %w", err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("pdftoppm produced no output for %s", pdfPath)
	}

	var parts []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		imgPath := filepath.Join(tmpDir, e.Name())
		text, err := c.extractImage(ctx, imgPath)
		if err != nil {
			return "", fmt.Errorf("extracting page %s: %w", e.Name(), err)
		}
		if t := strings.TrimSpace(text); t != "" {
			parts = append(parts, t)
		}
	}

	return strings.Join(parts, "\n\n"), nil
}

// extractImage sends a single image file to the vision model as a base64 data URI.
func (c *VisionClient) extractImage(ctx context.Context, filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("reading file %s: %w", filePath, err)
	}

	mimeType := detectMIMEType(filePath, data)
	dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data))

	req := visionRequest{
		Model: c.model,
		Messages: []visionMessage{
			{
				Role: "user",
				Content: []visionContentPart{
					{Type: "text", Text: vision.ExtractPrompt},
					{Type: "image_url", ImageURL: &visionImageURL{URL: dataURI}},
				},
			},
		},
		Temperature: 0.0,
		Stream:      false,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshalling vision request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building vision request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling LM Studio vision at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading vision response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LM Studio vision returned HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var result visionResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parsing vision response: %w", err)
	}

	if result.Error != nil {
		return "", fmt.Errorf("LM Studio vision error: %s", result.Error.Message)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("LM Studio vision returned no choices")
	}

	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

// detectMIMEType returns the MIME type for a file based on its extension,
// with a fallback to http.DetectContentType for unknown types.
func detectMIMEType(path string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".tiff", ".tif":
		return "image/tiff"
	}
	// Try extension-based MIME lookup before sniffing content bytes.
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	// Last resort: sniff the first 512 bytes.
	if len(data) > 512 {
		return http.DetectContentType(data[:512])
	}
	return http.DetectContentType(data)
}

// Ensure VisionClient satisfies vision.Client at compile time.
var _ vision.Client = (*VisionClient)(nil)
