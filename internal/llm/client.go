// Package llm defines the shared interface that every LLM backend must satisfy.
// Both the LM Studio client and the Gemini client implement this interface so
// that the grader and server are decoupled from any specific provider.
package llm

import "context"

// Message is a single chat turn.  Role is one of "system" or "user".
type Message struct {
	Role    string
	Content string
}

// Client is the minimal contract every LLM backend must satisfy.
type Client interface {
	// Complete sends the conversation to the model and returns the assistant
	// reply text.
	Complete(ctx context.Context, messages []Message) (string, error)
}
