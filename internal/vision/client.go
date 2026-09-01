// Package vision defines the shared interface for extracting plain text from
// binary files (PDFs, images) using a multimodal LLM backend.
// Two implementations exist:
//   - lmstudio.VisionClient  — calls qwen/qwen3-vl-8b via LM Studio
//   - gemini.VisionClient    — sends the file inline to the Gemini API
package vision

import "context"

// Client is the minimal contract every vision backend must satisfy.
type Client interface {
	// ExtractText sends the file at filePath to the vision model and returns
	// the plain-text transcription of its content.
	ExtractText(ctx context.Context, filePath string) (string, error)
}

// extractPrompt is the instruction sent alongside every image/PDF page.
const ExtractPrompt = "Transcribe all text from this image exactly as written. " +
	"Preserve code formatting, indentation, and structure. " +
	"Output only the transcribed text, no commentary."
