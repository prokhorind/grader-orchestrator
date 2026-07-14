// Command grade-ui starts a local HTTP server that provides a browser-based
// interface for grading Google Classroom submissions.
//
// Usage:
//
//	grade-ui \
//	  -workspace /path/to/grader-orchestrator \
//	  -mcp-root  /path/to/google-classroom-mcp \
//	  -port      8080
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/prokhorind/classroom-grader/internal/server"
)

func main() {
	workspaceFlag := flag.String("workspace", "", "Root directory containing prompts/ and submissions/ (required)")
	mcpRootFlag := flag.String("mcp-root", "", "google-classroom-mcp project root — credentials and token loaded from <mcp-root>/.secrets/")
	credsFlag := flag.String("credentials", "", "Path to Google OAuth2 credentials.json (overrides GOOGLE_CREDENTIALS_FILE)")
	tokenFlag := flag.String("token", "", "Path to cached OAuth2 token.json (overrides GOOGLE_TOKEN_FILE)")
	lmURLFlag := flag.String("lm-url", "http://localhost:1234/v1", "LM Studio API base URL")
	portFlag := flag.Int("port", 8080, "HTTP port to listen on")
	flag.Parse()

	if *workspaceFlag == "" {
		fmt.Fprintln(os.Stderr, "Error: -workspace is required")
		flag.Usage()
		os.Exit(1)
	}

	workspace, err := filepath.Abs(*workspaceFlag)
	if err != nil {
		log.Fatalf("resolving workspace path: %v", err)
	}

	creds := firstNonEmpty(*credsFlag, os.Getenv("GOOGLE_CREDENTIALS_FILE"))
	token := firstNonEmpty(*tokenFlag, os.Getenv("GOOGLE_TOKEN_FILE"))

	if *mcpRootFlag != "" {
		mcpRoot, err := filepath.Abs(*mcpRootFlag)
		if err != nil {
			log.Fatalf("resolving mcp-root path: %v", err)
		}
		if creds == "" {
			creds = filepath.Join(mcpRoot, ".secrets", "credentials.json")
		}
		if token == "" {
			token = filepath.Join(mcpRoot, ".secrets", "token.json")
		}
	}

	cfg := server.Config{
		Workspace:   workspace,
		CredsFile:   creds,
		TokenFile:   token,
		LMStudioURL: *lmURLFlag,
	}

	mux := server.New(cfg)

	addr := fmt.Sprintf(":%d", *portFlag)
	log.Printf("Grader UI → http://localhost%s", addr)

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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
