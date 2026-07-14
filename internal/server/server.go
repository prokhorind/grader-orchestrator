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
	"time"

	"github.com/prokhorind/classroom-grader/internal/classroom"
	"github.com/prokhorind/classroom-grader/internal/grader"
	"github.com/prokhorind/classroom-grader/internal/lmstudio"
)

// Config holds the server-level configuration resolved at startup.
type Config struct {
	Workspace   string
	CredsFile   string
	TokenFile   string
	LMStudioURL string
}

// New wires up all HTTP routes and returns the handler.
func New(cfg Config) http.Handler {
	mux := http.NewServeMux()

	// Serve static files from the embedded "static/" sub-directory at the root path.
	// StripPrefix removes the leading "/" so the file server looks inside "static/".
	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, staticFS, "index.html")
	})

	mux.HandleFunc("/api/courses", cfg.handleCourses)
	mux.HandleFunc("/api/assignments", cfg.handleAssignments)
	mux.HandleFunc("/api/local-assignments", cfg.handleLocalAssignments)
	mux.HandleFunc("/api/local-versions", cfg.handleLocalVersions)
	mux.HandleFunc("/api/grade", cfg.handleGrade)
	mux.HandleFunc("/api/regrade", cfg.handleRegrade)
	mux.HandleFunc("/api/marks", cfg.handleMarks)

	return mux
}

// ── API: /api/courses ─────────────────────────────────────────────────────────

func (cfg Config) handleCourses(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svc, _, err := classroom.NewService(ctx, cfg.CredsFile, cfg.TokenFile)
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

func (cfg Config) handleAssignments(w http.ResponseWriter, r *http.Request) {
	courseID := r.URL.Query().Get("courseId")
	if courseID == "" {
		jsonError(w, "courseId is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	svc, _, err := classroom.NewService(ctx, cfg.CredsFile, cfg.TokenFile)
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

// ── API: /api/local-assignments — scan submissions/ on disk ──────────────────

type localAssignment struct {
	CourseID       string `json:"course_id"`
	AssignmentName string `json:"assignment_name"`
}

func (cfg Config) handleLocalAssignments(w http.ResponseWriter, r *http.Request) {
	root := filepath.Join(cfg.Workspace, "submissions")
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

func (cfg Config) handleLocalVersions(w http.ResponseWriter, r *http.Request) {
	courseID := r.URL.Query().Get("courseId")
	assignment := r.URL.Query().Get("assignment")
	if courseID == "" || assignment == "" {
		jsonError(w, "courseId and assignment are required", http.StatusBadRequest)
		return
	}

	assignDir := filepath.Join(cfg.Workspace, "submissions", courseID, assignment)
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
	AssignmentID    string `json:"assignment_id"`
	AssignmentTitle string `json:"assignment_title"`
	Students        string `json:"students"` // comma-separated surnames, empty = all
	LMModel         string `json:"lm_model"`
	LMTimeoutMin    int    `json:"lm_timeout_min"` // 0 → default 5 min
}

type regradeRequest struct {
	CourseID       string `json:"course_id"`
	AssignmentName string `json:"assignment_name"`
	Timestamp      string `json:"timestamp"` // empty = latest
	Students       string `json:"students"`
	LMModel        string `json:"lm_model"`
	LMTimeoutMin   int    `json:"lm_timeout_min"`
}

// saveSolutionTemp reads the uploaded solution file from the multipart request,
// writes it to a temp file, and returns its path plus a cleanup function.
func saveSolutionTemp(r *http.Request, field string) (path string, cleanup func(), err error) {
	f, hdr, err := r.FormFile(field)
	if err != nil {
		return "", func() {}, fmt.Errorf("solution file required: %w", err)
	}
	defer f.Close()

	// Preserve the original file extension so the grader prompt context is accurate
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

// ── API: /api/grade — SSE stream, fetch from Classroom then grade ─────────────
// Accepts multipart/form-data with fields:

func (cfg Config) handleGrade(w http.ResponseWriter, r *http.Request) {
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

	sseStream(w, func(send func(string)) {
		defer cleanup()
		ctx := r.Context()

		send(logEvent("Authenticating with Google Classroom…"))
		svc, httpClient, err := classroom.NewService(ctx, cfg.CredsFile, cfg.TokenFile)
		if err != nil {
			send(errorEvent("auth failed: " + err.Error()))
			return
		}

		send(logEvent(fmt.Sprintf("Fetching submissions for assignment %q…", req.AssignmentTitle)))
		filter := classroom.NewStudentFilter(splitCSV(req.Students))
		submissionsDir := filepath.Join(cfg.Workspace, "submissions")

		subs, err := classroom.DownloadSubmissions(ctx, svc, httpClient,
			req.CourseID, req.AssignmentID, req.AssignmentTitle, submissionsDir, filter)
		if err != nil {
			send(errorEvent("download failed: " + err.Error()))
			return
		}
		send(logEvent(fmt.Sprintf("Downloaded %d submissions", len(subs))))

		marks, err := runGrader(ctx, cfg, tmpPath, req.LMModel, req.LMTimeoutMin, subs, send)
		if err != nil {
			send(errorEvent(err.Error()))
			return
		}

		outPath, err := grader.WriteMarks(cfg.Workspace, req.CourseID, req.AssignmentTitle, marks)
		if err != nil {
			send(errorEvent("writing marks: " + err.Error()))
			return
		}
		send(logEvent(fmt.Sprintf("Saved → %s", outPath)))
		send(doneEvent(marks))
	})
}

// ── API: /api/regrade — SSE stream, use already-downloaded files ──────────────
// Accepts multipart/form-data with fields:
//   params   – JSON-encoded regradeRequest (without solution_path)
//   solution – the teacher solution file

func (cfg Config) handleRegrade(w http.ResponseWriter, r *http.Request) {
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

	sseStream(w, func(send func(string)) {
		defer cleanup()
		ctx := r.Context()

		send(logEvent(fmt.Sprintf("Loading submissions from disk: %s / %s", req.CourseID, req.AssignmentName)))
		subs, err := loadSubmissionsFromDisk(cfg.Workspace, req.CourseID, req.AssignmentName, req.Timestamp, splitCSV(req.Students))
		if err != nil {
			send(errorEvent(err.Error()))
			return
		}
		send(logEvent(fmt.Sprintf("Found %d student submissions on disk", len(subs))))

		marks, err := runGrader(ctx, cfg, tmpPath, req.LMModel, req.LMTimeoutMin, subs, send)
		if err != nil {
			send(errorEvent(err.Error()))
			return
		}

		outPath, err := grader.WriteMarks(cfg.Workspace, req.CourseID, req.AssignmentName, marks)
		if err != nil {
			send(errorEvent("writing marks: " + err.Error()))
			return
		}
		send(logEvent(fmt.Sprintf("Saved → %s", outPath)))
		send(doneEvent(marks))
	})
}

// ── API: /api/marks?courseId=...&assignment=... ───────────────────────────────

func (cfg Config) handleMarks(w http.ResponseWriter, r *http.Request) {
	courseID := r.URL.Query().Get("courseId")
	assignment := r.URL.Query().Get("assignment")
	if courseID == "" || assignment == "" {
		jsonError(w, "courseId and assignment are required", http.StatusBadRequest)
		return
	}

	path := filepath.Join(cfg.Workspace, "submissions", courseID, classroom.Sanitize(assignment), "marks.json")
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

// ── shared grading helper ─────────────────────────────────────────────────────

func runGrader(ctx context.Context, cfg Config, solutionPath, lmModel string, lmTimeoutMin int, subs []classroom.Submission, send func(string)) ([]grader.Mark, error) {
	promptPath := filepath.Join(cfg.Workspace, "prompts", "grader.md")
	systemPrompt, err := os.ReadFile(promptPath)
	if err != nil {
		return nil, fmt.Errorf("reading grader prompt from %s: %w", promptPath, err)
	}

	timeout := time.Duration(lmTimeoutMin) * time.Minute
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	lmClient := lmstudio.NewClient(cfg.LMStudioURL, lmModel, timeout)
	g := grader.New(grader.Config{
		WorkspaceRoot:   cfg.Workspace,
		TeacherSolution: solutionPath,
		SystemPrompt:    string(systemPrompt),
	}, lmClient)

	// Wrap grader with per-student SSE progress by using a logging interceptor
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

func loadSubmissionsFromDisk(workspace, courseID, assignmentName, timestamp string, studentFilter []string) ([]classroom.Submission, error) {
	assignDir := filepath.Join(workspace, "submissions", courseID, assignmentName)
	studentDirs, err := os.ReadDir(assignDir)
	if err != nil {
		return nil, fmt.Errorf("reading assignment dir %s: %w", assignDir, err)
	}

	filter := classroom.NewStudentFilter(studentFilter)
	var submissions []classroom.Submission

	for _, sd := range studentDirs {
		if !sd.IsDir() {
			continue
		}
		studentID := sd.Name()
		studentDir := filepath.Join(assignDir, studentID)

		var versionDir string
		if timestamp != "" {
			versionDir = filepath.Join(studentDir, timestamp)
			if _, err := os.Stat(versionDir); err != nil {
				continue // this student has no entry for the requested timestamp
			}
		} else {
			versionDir, err = latestSubdir(studentDir)
			if err != nil {
				log.Printf("[regrade] WARN: skipping %s: %v", studentID, err)
				continue
			}
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
	return submissions, nil
}

func latestSubdir(dir string) (string, error) {
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
	return filepath.Join(dir, latest), nil
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

// sseStream sets SSE headers and calls fn with a send function.
// fn should write events via send() and return when done.
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
