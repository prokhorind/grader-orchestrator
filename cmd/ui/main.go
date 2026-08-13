// Command grade-ui starts a local HTTP server that provides a browser-based
// interface for grading Google Classroom submissions.
//
// Usage:
//
//	grade-ui -workspace /path/to/grader-orchestrator
//
// Credentials and token are stored in the OS default config directory
// (~/.config/classroom-grader/ on Linux/macOS,
// %AppData%\classroom-grader\ on Windows).
// You can override them with -credentials / -token or the corresponding
// environment variables if needed.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/prokhorind/classroom-grader/internal/classroom"
	"github.com/prokhorind/classroom-grader/internal/server"
)

func main() {
	workspaceFlag := flag.String("workspace", "", "Root directory for submissions and marks (optional, defaults to ~/Documents/classroom-grader)")
	credsFlag := flag.String("credentials", "", "Path to Google OAuth2 credentials.json (overrides GOOGLE_CREDENTIALS_FILE and default OS path)")
	tokenFlag := flag.String("token", "", "Path to cached OAuth2 token.json (overrides GOOGLE_TOKEN_FILE and default OS path)")
	lmURLFlag := flag.String("lm-url", "http://localhost:1234/v1", "LM Studio API base URL")
	llmBackendFlag := flag.String("llm-backend", "lmstudio", `LLM backend to use: "lmstudio" or "gemini"`)
	geminiAPIKeyFlag := flag.String("gemini-api-key", "", "Google Gemini API key (required when -llm-backend=gemini; overrides GEMINI_API_KEY env var)")
	geminiModelFlag := flag.String("gemini-model", "", `Gemini model name (default "gemini-2.5-flash")`)
	portFlag := flag.Int("port", 8080, "HTTP port to listen on")
	flag.Parse()

	workspacePath := *workspaceFlag
	if workspacePath == "" {
		workspacePath = classroom.DefaultWorkspacePath()
	}

	workspace, err := filepath.Abs(workspacePath)
	if err != nil {
		log.Fatalf("resolving workspace path: %v", err)
	}

	// Resolve credential paths: explicit flags → env vars → OS default config dir.
	creds, token, err := classroom.ResolveCredentialsAndTokenPaths(*credsFlag, *tokenFlag, "")
	if err != nil {
		log.Fatalf("resolving credentials and token paths: %v", err)
	}

	// Gemini API key: flag → GEMINI_API_KEY env var.
	geminiAPIKey := *geminiAPIKeyFlag
	if geminiAPIKey == "" {
		geminiAPIKey = os.Getenv("GEMINI_API_KEY")
	}

	cfg := server.Config{
		Workspace:    workspace,
		CredsFile:    creds,
		TokenFile:    token,
		LMStudioURL:  *lmURLFlag,
		LLMBackend:   *llmBackendFlag,
		GeminiAPIKey: geminiAPIKey,
		GeminiModel:  *geminiModelFlag,
	}

	mux := server.New(cfg)

	addr := fmt.Sprintf(":%d", *portFlag)
	log.Printf("Grader UI → http://localhost%s", addr)
	log.Printf("Backend      → %s", *llmBackendFlag)
	log.Printf("Credentials  → %s", creds)
	log.Printf("Token        → %s", token)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // SSE streams need unlimited write time
		IdleTimeout:  120 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}
