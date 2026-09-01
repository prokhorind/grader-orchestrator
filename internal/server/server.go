// Package server implements the HTTP server for the grader web UI.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prokhorind/classroom-grader/internal/classroom"
	"github.com/prokhorind/classroom-grader/internal/gemini"
	"github.com/prokhorind/classroom-grader/internal/grader"
	"github.com/prokhorind/classroom-grader/internal/llm"
	"github.com/prokhorind/classroom-grader/internal/lmstudio"
	"github.com/prokhorind/classroom-grader/internal/vision"
	"golang.org/x/oauth2"
)

// Config holds the server-level configuration resolved at startup.
type Config struct {
	Workspace           string
	CredsFile           string
	TokenFile           string
	LMStudioURL         string
	LMStudioModel       string // grading model, e.g. "qwen/qwen3-coder-30b"
	LMStudioVisionModel string // OCR/vision model, e.g. "qwen/qwen3-vl-8b"
	LLMBackend          string // "lmstudio" (default) or "gemini"
	GeminiAPIKey        string
	GeminiModel         string
}

// srv is the internal handler type that owns mutable state.
// All exported behaviour is wired through New() which returns http.Handler.
type srv struct {
	mu                  sync.RWMutex
	workspace           string // mutable — can be changed via /api/workspace
	credsFile           string
	tokenFile           string
	lmStudioURL         string // mutable — can be changed via /api/lm-url
	lmStudioModel       string // mutable — grading model
	lmStudioVisionModel string // mutable — OCR/vision model
	llmBackend          string // mutable — "lmstudio" or "gemini"
	geminiAPIKey        string // mutable — can be changed via /api/gemini-config
	geminiModel         string // mutable
}

// getWorkspace returns the current workspace path under a read lock.
func (s *srv) getWorkspace() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workspace
}

// setWorkspace updates the workspace path under a write lock.
func (s *srv) setWorkspace(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspace = path
}

// New wires up all HTTP routes and returns the handler.
func New(cfg Config) http.Handler {
	backend := cfg.LLMBackend
	if backend == "" {
		backend = "lmstudio"
	}
	s := &srv{
		workspace:           cfg.Workspace,
		credsFile:           cfg.CredsFile,
		tokenFile:           cfg.TokenFile,
		lmStudioURL:         cfg.LMStudioURL,
		lmStudioModel:       cfg.LMStudioModel,
		lmStudioVisionModel: cfg.LMStudioVisionModel,
		llmBackend:          backend,
		geminiAPIKey:        cfg.GeminiAPIKey,
		geminiModel:         cfg.GeminiModel,
	}

	mux := http.NewServeMux()

	// Serve static files from the embedded "static/" sub-directory at the root path.
	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, staticFS, "index.html")
	})

	mux.HandleFunc("/api/auth-status", s.handleAuthStatus)
	mux.HandleFunc("/api/upload-credentials", s.handleUploadCredentials)
	mux.HandleFunc("/api/auth-exchange", s.handleAuthExchange)
	mux.HandleFunc("/api/workspace", s.handleWorkspace)
	mux.HandleFunc("/api/lm-url", s.handleLMURL)
	mux.HandleFunc("/api/lm-models", s.handleLMModels)
	mux.HandleFunc("/api/llm-backend", s.handleLLMBackend)
	mux.HandleFunc("/api/gemini-config", s.handleGeminiConfig)
	mux.HandleFunc("/api/gemini-models", s.handleGeminiModels)
	mux.HandleFunc("/api/courses", s.handleCourses)
	mux.HandleFunc("/api/assignments", s.handleAssignments)
	mux.HandleFunc("/api/students", s.handleStudents)
	mux.HandleFunc("/api/local-assignments", s.handleLocalAssignments)
	mux.HandleFunc("/api/local-versions", s.handleLocalVersions)
	mux.HandleFunc("/api/grade", s.handleGrade)
	mux.HandleFunc("/api/regrade", s.handleRegrade)
	mux.HandleFunc("/api/marks", s.handleMarks)
	mux.HandleFunc("/api/patch-mark", s.handlePatchMark)

	return mux
}

// ── API: /api/workspace ───────────────────────────────────────────────────────

type workspaceResponse struct {
	Workspace string `json:"workspace"`
}

// handleWorkspace handles GET (read current path) and POST (update path).
func (s *srv) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOK(w, workspaceResponse{Workspace: s.getWorkspace()})

	case http.MethodPost:
		var req workspaceResponse
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Workspace == "" {
			jsonError(w, "workspace path is required", http.StatusBadRequest)
			return
		}
		abs, err := filepath.Abs(req.Workspace)
		if err != nil {
			jsonError(w, "invalid path: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Create the directory if it doesn't exist yet.
		if err := os.MkdirAll(abs, 0755); err != nil {
			jsonError(w, "creating workspace dir: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.setWorkspace(abs)
		log.Printf("[workspace] changed to %s", abs)
		jsonOK(w, workspaceResponse{Workspace: abs})

	default:
		jsonError(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

// getLMStudioConfig returns the LM Studio URL, grading model, and vision model
// under a read lock.
func (s *srv) getLMStudioConfig() (url, model, visionModel string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lmStudioURL, s.lmStudioModel, s.lmStudioVisionModel
}

// getLLMBackend returns the active backend name under a read lock.
func (s *srv) getLLMBackend() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.llmBackend
}

// getGeminiConfig returns the Gemini API key and model under a read lock.
func (s *srv) getGeminiConfig() (apiKey, model string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.geminiAPIKey, s.geminiModel
}

// buildLLMClient constructs the active LLM client from current server config.
func (s *srv) buildLLMClient(timeout time.Duration) (llm.Client, error) {
	backend := s.getLLMBackend()
	switch backend {
	case "gemini":
		apiKey, model := s.getGeminiConfig()
		if apiKey == "" {
			return nil, fmt.Errorf("Gemini API key is not configured — set it in Settings")
		}
		if model == "" {
			return nil, fmt.Errorf("Gemini model is not selected — pick one from the model list in Settings")
		}
		return gemini.NewClient(apiKey, model, timeout), nil
	default: // "lmstudio"
		url, model, _ := s.getLMStudioConfig()
		return lmstudio.NewClient(url, model, timeout), nil
	}
}

// buildVisionClient constructs the vision (OCR) client matching the active
// LLM backend.  Returns nil when vision is not applicable — callers treat nil
// as "no vision".
func (s *srv) buildVisionClient(timeout time.Duration) vision.Client {
	backend := s.getLLMBackend()
	switch backend {
	case "gemini":
		apiKey, model := s.getGeminiConfig()
		if apiKey == "" || model == "" {
			return nil
		}
		return gemini.NewVisionClient(apiKey, model, timeout)
	default: // "lmstudio"
		url, _, visionModel := s.getLMStudioConfig()
		if url == "" {
			return nil
		}
		if visionModel == "" {
			visionModel = lmstudio.VisionModel // default: qwen/qwen3-vl-8b
		}
		return lmstudio.NewVisionClient(url, visionModel, timeout)
	}
}

// ── API: /api/lm-url ──────────────────────────────────────────────────────────

type lmURLResponse struct {
	LMURL         string `json:"lm_url"`
	LMModel       string `json:"lm_model"`        // grading model, e.g. "qwen/qwen3-coder-30b"
	LMVisionModel string `json:"lm_vision_model"` // OCR model, e.g. "qwen/qwen3-vl-8b"
}

// handleLMURL handles GET (read current URL + models) and POST (update).
func (s *srv) handleLMURL(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		url, model, visionModel := s.getLMStudioConfig()
		jsonOK(w, lmURLResponse{LMURL: url, LMModel: model, LMVisionModel: visionModel})

	case http.MethodPost:
		var req lmURLResponse
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.LMURL == "" {
			jsonError(w, "lm_url is required", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.lmStudioURL = req.LMURL
		s.lmStudioModel = req.LMModel
		s.lmStudioVisionModel = req.LMVisionModel
		s.mu.Unlock()
		log.Printf("[lm-url] changed to %s (model=%q vision_model=%q)", req.LMURL, req.LMModel, req.LMVisionModel)
		url, model, visionModel := s.getLMStudioConfig()
		jsonOK(w, lmURLResponse{LMURL: url, LMModel: model, LMVisionModel: visionModel})

	default:
		jsonError(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

// ── API: /api/lm-models — proxy GET /v1/models to LM Studio ─────────────────

// handleLMModels proxies a GET to LM Studio's /v1/models endpoint and returns
// the list of loaded model IDs.  The browser can't call LM Studio directly due
// to CORS, so this server-side proxy is needed.
func (s *srv) handleLMModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	lmURL, _, _ := s.getLMStudioConfig()
	if lmURL == "" {
		lmURL = "http://localhost:1234/v1"
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, lmURL+"/models", nil)
	if err != nil {
		jsonError(w, "building request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		jsonError(w, "LM Studio unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		jsonError(w, "reading response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Parse and return just the list of model IDs.
	var modelsResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &modelsResp); err != nil {
		jsonError(w, "parsing models response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ids := make([]string, 0, len(modelsResp.Data))
	for _, m := range modelsResp.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	jsonOK(w, ids)
}

// ── API: /api/llm-backend ─────────────────────────────────────────────────────

type llmBackendResponse struct {
	Backend string `json:"backend"` // "lmstudio" or "gemini"
}

// handleLLMBackend handles GET (read active backend) and POST (switch backend).
func (s *srv) handleLLMBackend(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOK(w, llmBackendResponse{Backend: s.getLLMBackend()})

	case http.MethodPost:
		var req llmBackendResponse
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Backend != "lmstudio" && req.Backend != "gemini" {
			jsonError(w, `backend must be "lmstudio" or "gemini"`, http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.llmBackend = req.Backend
		s.mu.Unlock()
		log.Printf("[llm-backend] switched to %s", req.Backend)
		jsonOK(w, llmBackendResponse{Backend: req.Backend})

	default:
		jsonError(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

// ── API: /api/gemini-config ───────────────────────────────────────────────────

type geminiConfigResponse struct {
	APIKey string `json:"api_key"`
	Model  string `json:"model"`
}

// handleGeminiConfig handles GET (read config, key masked) and POST (update).
func (s *srv) handleGeminiConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		apiKey, model := s.getGeminiConfig()
		masked := ""
		if len(apiKey) > 8 {
			masked = apiKey[:4] + strings.Repeat("*", len(apiKey)-8) + apiKey[len(apiKey)-4:]
		} else if apiKey != "" {
			masked = strings.Repeat("*", len(apiKey))
		}
		jsonOK(w, geminiConfigResponse{APIKey: masked, Model: model})

	case http.MethodPost:
		var req geminiConfigResponse
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		if req.APIKey != "" {
			s.geminiAPIKey = req.APIKey
		}
		if req.Model != "" {
			s.geminiModel = req.Model
		}
		s.mu.Unlock()
		log.Printf("[gemini-config] updated (model=%s)", req.Model)
		jsonOK(w, map[string]string{"status": "ok"})

	default:
		jsonError(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

// ── API: /api/gemini-models ───────────────────────────────────────────────────

// handleGeminiModels returns all Gemini models that support generateContent,
// fetched live from the API using the currently configured API key.
// GET only; returns 400 if no API key is set.
func (s *srv) handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	apiKey, _ := s.getGeminiConfig()
	if apiKey == "" {
		jsonError(w, "Gemini API key is not configured — set it in Settings first", http.StatusBadRequest)
		return
	}
	models, err := gemini.ListModels(r.Context(), apiKey)
	if err != nil {
		jsonError(w, "listing Gemini models: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, models)
}

// ── API: /api/courses ─────────────────────────────────────────────────────────

func (s *srv) handleCourses(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svc, _, err := classroom.NewService(ctx, s.credsFile, s.tokenFile)
	if err != nil {
		jsonError(w, fmt.Sprintf("auth error: %v", err), http.StatusUnauthorized)
		return
	}
	courses, err := classroom.ListCourses(ctx, svc)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, courses)
}

// ── API: /api/assignments?courseId=... ────────────────────────────────────────

func (s *srv) handleAssignments(w http.ResponseWriter, r *http.Request) {
	courseID := r.URL.Query().Get("courseId")
	if courseID == "" {
		jsonError(w, "courseId is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	svc, _, err := classroom.NewService(ctx, s.credsFile, s.tokenFile)
	if err != nil {
		jsonError(w, fmt.Sprintf("auth error: %v", err), http.StatusUnauthorized)
		return
	}
	assignments, err := classroom.ListAssignments(ctx, svc, courseID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, assignments)
}

// ── API: /api/students?courseId=... ──────────────────────────────────────────

func (s *srv) handleStudents(w http.ResponseWriter, r *http.Request) {
	courseID := r.URL.Query().Get("courseId")
	if courseID == "" {
		jsonError(w, "courseId is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	svc, _, err := classroom.NewService(ctx, s.credsFile, s.tokenFile)
	if err != nil {
		jsonError(w, fmt.Sprintf("auth error: %v", err), http.StatusUnauthorized)
		return
	}
	students, err := classroom.ListStudents(ctx, svc, courseID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, students)
}

// ── API: /api/local-assignments — scan submissions/ on disk ──────────────────

type localAssignment struct {
	CourseID       string `json:"course_id"`
	AssignmentName string `json:"assignment_name"`
}

func (s *srv) handleLocalAssignments(w http.ResponseWriter, r *http.Request) {
	root := filepath.Join(s.getWorkspace(), "submissions")
	var results []localAssignment

	courseDirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			jsonOK(w, results)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for _, cd := range courseDirs {
		if !cd.IsDir() {
			continue
		}
		assignDirs, err := os.ReadDir(filepath.Join(root, cd.Name()))
		if err != nil {
			continue
		}
		for _, ad := range assignDirs {
			if !ad.IsDir() {
				continue
			}
			results = append(results, localAssignment{
				CourseID:       cd.Name(),
				AssignmentName: ad.Name(),
			})
		}
	}
	jsonOK(w, results)
}

// ── API: /api/local-versions?courseId=...&assignment=... ─────────────────────

type localVersion struct {
	Timestamp    string `json:"timestamp"`
	StudentCount int    `json:"student_count"`
}

func (s *srv) handleLocalVersions(w http.ResponseWriter, r *http.Request) {
	courseID := r.URL.Query().Get("courseId")
	assignment := r.URL.Query().Get("assignment")
	if courseID == "" || assignment == "" {
		jsonError(w, "courseId and assignment are required", http.StatusBadRequest)
		return
	}

	assignDir := filepath.Join(s.getWorkspace(), "submissions", courseID, assignment)
	studentDirs, err := os.ReadDir(assignDir)
	if err != nil {
		if os.IsNotExist(err) {
			jsonOK(w, []localVersion{})
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Collect all unique timestamps across all student dirs
	tsCount := map[string]int{}
	for _, sd := range studentDirs {
		if !sd.IsDir() {
			continue
		}
		tsDirs, err := os.ReadDir(filepath.Join(assignDir, sd.Name()))
		if err != nil {
			continue
		}
		for _, td := range tsDirs {
			if td.IsDir() {
				tsCount[td.Name()]++
			}
		}
	}

	var versions []localVersion
	for ts, count := range tsCount {
		versions = append(versions, localVersion{Timestamp: ts, StudentCount: count})
	}
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Timestamp > versions[j].Timestamp // newest first
	})
	jsonOK(w, versions)
}

// ── request types ─────────────────────────────────────────────────────────────

type gradeRequest struct {
	CourseID        string `json:"course_id"`
	CourseName      string `json:"course_name"`
	AssignmentID    string `json:"assignment_id"`
	AssignmentTitle string `json:"assignment_title"`
	Students        string `json:"students"` // comma-separated full names, empty = all
}

type regradeRequest struct {
	CourseID       string `json:"course_id"`
	AssignmentName string `json:"assignment_name"`
	Timestamp      string `json:"timestamp"` // empty = latest
	Students       string `json:"students"`
}

// saveSolutionTemp reads the uploaded solution file from the multipart request,
// writes it to a temp file, and returns its path plus a cleanup function.
func saveSolutionTemp(r *http.Request, field string) (path string, cleanup func(), err error) {
	f, hdr, err := r.FormFile(field)
	if err != nil {
		return "", func() {}, fmt.Errorf("solution file required: %w", err)
	}
	defer f.Close()

	ext := filepath.Ext(hdr.Filename)
	tmp, err := os.CreateTemp("", "solution-*"+ext)
	if err != nil {
		return "", func() {}, fmt.Errorf("creating temp file: %w", err)
	}

	if _, err := io.Copy(tmp, f); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", func() {}, fmt.Errorf("writing solution temp file: %w", err)
	}
	tmp.Close()

	return tmp.Name(), func() { os.Remove(tmp.Name()) }, nil
}

// readRulesFile reads the uploaded grading rules file from the multipart request.
func readRulesFile(r *http.Request) (string, error) {
	f, _, err := r.FormFile("rules")
	if err != nil {
		if err == http.ErrMissingFile {
			return "", fmt.Errorf("grading rules file is required: please upload a rules file")
		}
		return "", fmt.Errorf("reading grading rules form file: %w", err)
	}
	defer f.Close()

	var buf strings.Builder
	if _, err := io.Copy(&buf, f); err != nil {
		return "", fmt.Errorf("reading uploaded grading rules file: %w", err)
	}
	return buf.String(), nil
}

// ── API: /api/grade — SSE stream, fetch from Classroom then grade ─────────────

func (s *srv) handleGrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		jsonError(w, "multipart parse error: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req gradeRequest
	if err := json.Unmarshal([]byte(r.FormValue("params")), &req); err != nil {
		jsonError(w, "invalid params JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	tmpPath, cleanup, err := saveSolutionTemp(r, "solution")
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	rulesContent, err := readRulesFile(r)
	if err != nil {
		cleanup()
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	courseFolderName := req.CourseName
	if courseFolderName == "" {
		courseFolderName = req.CourseID
	}

	// Snapshot mutable fields once for the lifetime of this request.
	workspace := s.getWorkspace()
	credsFile := s.credsFile
	tokenFile := s.tokenFile

	const defaultTimeout = 5 * time.Minute
	llmClient, err := s.buildLLMClient(defaultTimeout)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	visionClient := s.buildVisionClient(defaultTimeout)

	sseStream(w, func(send func(string)) {
		defer cleanup()
		ctx := r.Context()

		send(logEvent("Authenticating with Google Classroom…"))
		svc, httpClient, err := classroom.NewService(ctx, credsFile, tokenFile)
		if err != nil {
			send(errorEvent("auth failed: " + err.Error()))
			return
		}

		send(logEvent(fmt.Sprintf("Fetching submissions for assignment %q…", req.AssignmentTitle)))
		filter := classroom.NewStudentFilter(splitCSV(req.Students))
		submissionsDir := filepath.Join(workspace, "submissions")

		subs, err := classroom.DownloadSubmissions(ctx, svc, httpClient,
			req.CourseID, courseFolderName, req.AssignmentID, req.AssignmentTitle, submissionsDir, filter, visionClient)
		if err != nil {
			send(errorEvent("download failed: " + err.Error()))
			return
		}
		send(logEvent(fmt.Sprintf("Downloaded %d submissions", len(subs))))

		marks, err := runGrader(ctx, workspace, llmClient, tmpPath, rulesContent, subs, send)
		if err != nil {
			send(errorEvent(err.Error()))
			return
		}

		// Use the timestamp from the first submission (all share the same one per fetch run).
		timestamp := ""
		if len(subs) > 0 {
			timestamp = subs[0].Version
		}
		if timestamp == "" {
			timestamp = time.Now().Local().Format("2006-01-02T15-04-05")
		}

		outPath, err := grader.WriteMarks(workspace, courseFolderName, req.AssignmentTitle, timestamp, marks)
		if err != nil {
			send(errorEvent("writing marks: " + err.Error()))
			return
		}
		send(logEvent(fmt.Sprintf("Saved → %s", outPath)))
		send(doneEvent(marks))
	})
}

// ── API: /api/regrade — SSE stream, use already-downloaded files ──────────────

func (s *srv) handleRegrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		jsonError(w, "multipart parse error: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req regradeRequest
	if err := json.Unmarshal([]byte(r.FormValue("params")), &req); err != nil {
		jsonError(w, "invalid params JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	tmpPath, cleanup, err := saveSolutionTemp(r, "solution")
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	rulesContent, err := readRulesFile(r)
	if err != nil {
		cleanup()
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Snapshot mutable fields once for the lifetime of this request.
	workspace := s.getWorkspace()

	const defaultTimeout = 5 * time.Minute
	llmClient, err := s.buildLLMClient(defaultTimeout)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	sseStream(w, func(send func(string)) {
		defer cleanup()
		ctx := r.Context()

		send(logEvent(fmt.Sprintf("Loading submissions from disk: %s / %s", req.CourseID, req.AssignmentName)))
		subs, resolvedTS, err := loadSubmissionsFromDisk(workspace, req.CourseID, req.AssignmentName, req.Timestamp, splitCSV(req.Students))
		if err != nil {
			send(errorEvent(err.Error()))
			return
		}
		send(logEvent(fmt.Sprintf("Found %d student submissions on disk", len(subs))))

		marks, err := runGrader(ctx, workspace, llmClient, tmpPath, rulesContent, subs, send)
		if err != nil {
			send(errorEvent(err.Error()))
			return
		}

		outPath, err := grader.WriteMarks(workspace, req.CourseID, req.AssignmentName, resolvedTS, marks)
		if err != nil {
			send(errorEvent("writing marks: " + err.Error()))
			return
		}
		send(logEvent(fmt.Sprintf("Saved → %s", outPath)))
		send(doneEvent(marks))
	})
}

// ── API: /api/marks?courseId=...&assignment=... ───────────────────────────────

// ── API: /api/marks?courseId=...&assignment=...&timestamp=... ─────────────────
// timestamp is optional; if omitted, the latest timestamp dir is used.

func (s *srv) handleMarks(w http.ResponseWriter, r *http.Request) {
	courseID := r.URL.Query().Get("courseId")
	assignment := r.URL.Query().Get("assignment")
	if courseID == "" || assignment == "" {
		jsonError(w, "courseId and assignment are required", http.StatusBadRequest)
		return
	}

	assignDir := filepath.Join(s.getWorkspace(), "submissions",
		classroom.Sanitize(courseID),
		classroom.Sanitize(assignment))

	timestamp := r.URL.Query().Get("timestamp")
	if timestamp == "" {
		// Find the latest timestamp dir that contains a marks.json.
		ts, err := latestMarksTimestamp(assignDir)
		if err != nil {
			jsonOK(w, []any{})
			return
		}
		timestamp = ts
	}

	path := filepath.Join(assignDir, timestamp, "marks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			jsonOK(w, []any{})
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// latestMarksTimestamp returns the lexicographically newest subdir of assignDir
// that contains a marks.json file.
func latestMarksTimestamp(assignDir string) (string, error) {
	entries, err := os.ReadDir(assignDir)
	if err != nil {
		return "", err
	}
	var latest string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() <= latest {
			continue
		}
		if _, err := os.Stat(filepath.Join(assignDir, e.Name(), "marks.json")); err == nil {
			latest = e.Name()
		}
	}
	if latest == "" {
		return "", fmt.Errorf("no marks.json found under %s", assignDir)
	}
	return latest, nil
}

// ── API: /api/patch-mark — update a single student mark without re-grading ────
// POST JSON body: { course_id, assignment_name, timestamp, student_name, mark, deductions, comment }
// timestamp is optional; if omitted the latest timestamp with a marks.json is used.

type patchMarkRequest struct {
	CourseID       string `json:"course_id"`
	AssignmentName string `json:"assignment_name"`
	Timestamp      string `json:"timestamp"`
	StudentName    string `json:"student_name"`
	Mark           int    `json:"mark"`
	Deductions     string `json:"deductions"`
	Comment        string `json:"comment"`
}

func (s *srv) handlePatchMark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req patchMarkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.CourseID == "" || req.AssignmentName == "" || req.StudentName == "" {
		jsonError(w, "course_id, assignment_name and student_name are required", http.StatusBadRequest)
		return
	}
	if req.Mark < 1 || req.Mark > 12 {
		jsonError(w, "mark must be between 1 and 12", http.StatusBadRequest)
		return
	}

	assignDir := filepath.Join(s.getWorkspace(), "submissions",
		classroom.Sanitize(req.CourseID),
		classroom.Sanitize(req.AssignmentName))

	timestamp := req.Timestamp
	if timestamp == "" {
		ts, err := latestMarksTimestamp(assignDir)
		if err != nil {
			jsonError(w, "no marks found for this assignment", http.StatusNotFound)
			return
		}
		timestamp = ts
	}

	marksPath := filepath.Join(assignDir, timestamp, "marks.json")
	marks := grader.ReadMarksFile(marksPath)
	if marks == nil {
		jsonError(w, "marks.json not found for timestamp "+timestamp, http.StatusNotFound)
		return
	}

	// Find and update the matching student, or append if not present.
	found := false
	for i, m := range marks {
		if m.StudentName == req.StudentName {
			marks[i].Mark = req.Mark
			marks[i].Deductions = req.Deductions
			marks[i].Comment = req.Comment
			found = true
			break
		}
	}
	if !found {
		marks = append(marks, grader.Mark{
			StudentName: req.StudentName,
			Mark:        req.Mark,
			Deductions:  req.Deductions,
			Comment:     req.Comment,
		})
	}

	// Sort and write back.
	sort.Slice(marks, func(i, j int) bool {
		return marks[i].StudentName < marks[j].StudentName
	})
	data, err := json.MarshalIndent(marks, "", "  ")
	if err != nil {
		jsonError(w, "marshalling marks: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(marksPath, data, 0644); err != nil {
		jsonError(w, "writing marks.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[patch-mark] %s / %s / %s → %d", req.CourseID, req.AssignmentName, req.StudentName, req.Mark)
	jsonOK(w, map[string]string{"status": "ok", "timestamp": timestamp})
}

// ── shared grading helper ─────────────────────────────────────────────────────

func runGrader(ctx context.Context, workspace string, llmClient llm.Client, solutionPath, systemPrompt string, subs []classroom.Submission, send func(string)) ([]grader.Mark, error) {
	g := grader.New(grader.Config{
		WorkspaceRoot:   workspace,
		TeacherSolution: solutionPath,
		SystemPrompt:    systemPrompt,
	}, llmClient)

	total := len(subs)
	var marks []grader.Mark
	for i, sub := range subs {
		send(logEvent(fmt.Sprintf("[%d/%d] Grading %s…", i+1, total, sub.StudentName)))
		send(progressEvent(i, total))

		batch, err := g.GradeAll(ctx, []classroom.Submission{sub})
		if err != nil {
			return nil, fmt.Errorf("grading %s: %w", sub.StudentName, err)
		}
		if len(batch) > 0 {
			m := batch[0]
			marks = append(marks, m)
			send(logEvent(fmt.Sprintf("  → %s: %d/12", m.StudentName, m.Mark)))
		}
	}
	send(progressEvent(total, total))
	return marks, nil
}

// ── disk loader for re-grade ──────────────────────────────────────────────────

func loadSubmissionsFromDisk(workspace, courseID, assignmentName, timestamp string, studentFilter []string) ([]classroom.Submission, string, error) {
	assignDir := filepath.Join(workspace, "submissions", courseID, assignmentName)
	studentDirs, err := os.ReadDir(assignDir)
	if err != nil {
		return nil, "", fmt.Errorf("reading assignment dir %s: %w", assignDir, err)
	}

	// Resolve the timestamp we'll actually use: explicit > latest across students.
	resolvedTS := timestamp
	if resolvedTS == "" {
		// Find the latest timestamp present across any student dir.
		for _, sd := range studentDirs {
			if !sd.IsDir() {
				continue
			}
			latest, err := latestSubdirName(filepath.Join(assignDir, sd.Name()))
			if err != nil {
				continue
			}
			if latest > resolvedTS {
				resolvedTS = latest
			}
		}
	}

	filter := classroom.NewStudentFilter(studentFilter)
	var submissions []classroom.Submission

	for _, sd := range studentDirs {
		if !sd.IsDir() {
			continue
		}
		studentID := sd.Name()
		studentDir := filepath.Join(assignDir, studentID)

		versionDir := filepath.Join(studentDir, resolvedTS)
		if _, err := os.Stat(versionDir); err != nil {
			continue // this student has no entry for the resolved timestamp
		}

		profile, _ := readStudentJSON(filepath.Join(versionDir, "student.json"))
		if profile.ID == "" {
			profile.ID = studentID
		}

		if !filter.Match(profile) {
			continue
		}

		fileEntries, err := os.ReadDir(versionDir)
		if err != nil {
			continue
		}
		var files []classroom.DownloadedFile
		for _, fe := range fileEntries {
			if !fe.IsDir() {
				files = append(files, classroom.DownloadedFile{
					Name: fe.Name(),
					Path: filepath.Join(versionDir, fe.Name()),
				})
			}
		}
		submissions = append(submissions, classroom.Submission{
			StudentID:    profile.ID,
			StudentName:  profile.FullName,
			StudentEmail: profile.Email,
			Files:        files,
		})
	}
	return submissions, resolvedTS, nil
}

func latestSubdir(dir string) (string, error) {
	name, err := latestSubdirName(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func latestSubdirName(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var latest string
	for _, e := range entries {
		if e.IsDir() && e.Name() > latest {
			latest = e.Name()
		}
	}
	if latest == "" {
		return "", fmt.Errorf("no timestamp subdirectories in %s", dir)
	}
	return latest, nil
}

func readStudentJSON(path string) (classroom.StudentProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return classroom.StudentProfile{}, err
	}
	var p classroom.StudentProfile
	return p, json.Unmarshal(data, &p)
}

// ── SSE helpers ───────────────────────────────────────────────────────────────

func sseStream(w http.ResponseWriter, fn func(send func(string))) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	send := func(data string) {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	fn(send)
}

type sseEvent struct {
	Type    string        `json:"type"`
	Message string        `json:"message,omitempty"`
	Current int           `json:"current,omitempty"`
	Total   int           `json:"total,omitempty"`
	Marks   []grader.Mark `json:"marks,omitempty"`
}

func logEvent(msg string) string {
	b, _ := json.Marshal(sseEvent{Type: "log", Message: msg})
	return string(b)
}

func errorEvent(msg string) string {
	b, _ := json.Marshal(sseEvent{Type: "error", Message: msg})
	return string(b)
}

func progressEvent(current, total int) string {
	b, _ := json.Marshal(sseEvent{Type: "progress", Current: current, Total: total})
	return string(b)
}

func doneEvent(marks []grader.Mark) string {
	b, _ := json.Marshal(sseEvent{Type: "done", Marks: marks})
	return string(b)
}

// ── JSON response helpers ─────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ── misc helpers ──────────────────────────────────────────────────────────────

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ── API: /api/auth-status ─────────────────────────────────────────────────────

type AuthStatus struct {
	CredentialsExists bool   `json:"credentials_exists"`
	TokenExists       bool   `json:"token_exists"`
	CredentialsPath   string `json:"credentials_path"`
	TokenPath         string `json:"token_path"`
	AuthURL           string `json:"auth_url,omitempty"`
}

func (s *srv) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	status := AuthStatus{
		CredentialsPath: s.credsFile,
		TokenPath:       s.tokenFile,
	}

	if _, err := os.Stat(s.credsFile); err == nil {
		status.CredentialsExists = true
	}
	if _, err := os.Stat(s.tokenFile); err == nil {
		status.TokenExists = true
	}

	// Even when a token file exists, check whether it is still valid.
	if status.CredentialsExists && status.TokenExists {
		if tok, err := classroom.LoadToken(s.tokenFile); err != nil || !tok.Valid() {
			status.TokenExists = false
		}
	}

	if status.CredentialsExists && !status.TokenExists {
		config, err := classroom.OAuthConfigFromFile(s.credsFile)
		if err == nil {
			status.AuthURL = config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
		}
	}

	jsonOK(w, status)
}

// ── API: /api/upload-credentials ─────────────────────────────────────────────

func (s *srv) handleUploadCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		jsonError(w, "multipart parse error: "+err.Error(), http.StatusBadRequest)
		return
	}

	f, _, err := r.FormFile("credentials")
	if err != nil {
		jsonError(w, "credentials file required", http.StatusBadRequest)
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		jsonError(w, "reading uploaded file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := classroom.OAuthConfigFromBytes(data); err != nil {
		jsonError(w, "invalid credentials file: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(filepath.Dir(s.credsFile), 0700); err != nil {
		jsonError(w, "creating config dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(s.credsFile, data, 0600); err != nil {
		jsonError(w, "saving credentials: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"status": "ok", "path": s.credsFile})
}

// ── API: /api/auth-exchange ───────────────────────────────────────────────────

type AuthExchangeRequest struct {
	Code string `json:"code"`
}

func (s *srv) handleAuthExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req AuthExchangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Code == "" {
		jsonError(w, "code is required", http.StatusBadRequest)
		return
	}

	config, err := classroom.OAuthConfigFromFile(s.credsFile)
	if err != nil {
		jsonError(w, fmt.Sprintf("loading credentials: %v", err), http.StatusInternalServerError)
		return
	}

	tok, err := config.Exchange(r.Context(), req.Code)
	if err != nil {
		jsonError(w, fmt.Sprintf("exchanging code: %v", err), http.StatusBadRequest)
		return
	}

	if err := classroom.SaveToken(s.tokenFile, tok); err != nil {
		jsonError(w, fmt.Sprintf("saving token: %v", err), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"status": "ok"})
}
