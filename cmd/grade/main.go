// Command grade fetches student submissions from Google Classroom and grades
// them using a local LM Studio model, writing marks.json to the submissions folder.
//
// Usage:
//
//	grade \
//	  -class "Моя школа — 10-А" \
//	  -assignment "41. SQL завдання" \
//	  -solution /path/to/solutions/python/41.sql/sol.sql
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prokhorind/classroom-grader/internal/grader"
	"github.com/prokhorind/classroom-grader/internal/lmstudio"
	"github.com/prokhorind/google-classroom-mcp/classroom"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

func main() {
	// ── Flags ────────────────────────────────────────────────────────────────
	classFlag := flag.String("class", "", "Google Classroom course name or ID — supports Cyrillic, spaces, special chars (required)")
	assignmentFlag := flag.String("assignment", "", "Assignment name — supports Cyrillic, spaces, special chars (required)")
	solutionFlag := flag.String("solution", "", "Path to teacher solution file (required)")
	workspaceFlag := flag.String("workspace", "", "Root directory containing prompts/ and submissions/ (required)")
	mcpRootFlag := flag.String("mcp-root", "", "google-classroom-mcp project root — credentials and token are loaded from <mcp-root>/.secrets/ (overrides -credentials and -token)")
	credsFlag := flag.String("credentials", "", "Path to Google OAuth2 credentials.json (overrides GOOGLE_CREDENTIALS_FILE)")
	tokenFlag := flag.String("token", "", "Path to cached OAuth2 token.json (overrides GOOGLE_TOKEN_FILE)")
	lmURLFlag := flag.String("lm-url", "http://localhost:1234/v1", "LM Studio API base URL")
	lmModelFlag := flag.String("lm-model", "", "LM Studio model identifier (leave empty to use loaded model)")
	lmTimeoutFlag := flag.Duration("lm-timeout", 5*time.Minute, "Timeout per LM Studio request")
	studentsFlag := flag.String("students", "", "Comma-separated surnames to grade (empty = all students)")
	skipFetchFlag := flag.Bool("skip-fetch", false, "Skip downloading submissions (use already-downloaded files)")
	flag.Parse()

	// ── Validate required flags ───────────────────────────────────────────────
	if *classFlag == "" || *assignmentFlag == "" || *solutionFlag == "" || *workspaceFlag == "" {
		fmt.Fprintln(os.Stderr, "Error: -class, -assignment, -solution, and -workspace are required")
		flag.Usage()
		os.Exit(1)
	}

	// ── Resolve paths to absolute ─────────────────────────────────────────────
	workspace, err := filepath.Abs(*workspaceFlag)
	if err != nil {
		log.Fatalf("resolving workspace path: %v", err)
	}
	solutionPath, err := filepath.Abs(*solutionFlag)
	if err != nil {
		log.Fatalf("resolving solution path: %v", err)
	}

	// ── Resolve credentials from mcp-root if provided ─────────────────────────
	if *mcpRootFlag != "" {
		mcpRoot, err := filepath.Abs(*mcpRootFlag)
		if err != nil {
			log.Fatalf("resolving mcp-root path: %v", err)
		}
		if *credsFlag == "" {
			*credsFlag = filepath.Join(mcpRoot, ".secrets", "credentials.json")
		}
		if *tokenFlag == "" {
			*tokenFlag = filepath.Join(mcpRoot, ".secrets", "token.json")
		}
	}

	// ── Load grader system prompt ─────────────────────────────────────────────
	promptPath := filepath.Join(workspace, "prompts", "grader.md")
	systemPrompt, err := os.ReadFile(promptPath)
	if err != nil {
		log.Fatalf("reading grader prompt from %s: %v", promptPath, err)
	}

	ctx := context.Background()

	// ── Fetch or reuse submissions ────────────────────────────────────────────
	var submissions []classroom.Submission
	var courseID string

	if *skipFetchFlag {
		log.Printf("[main] --skip-fetch: loading submissions from disk")
		submissions, courseID, err = loadSubmissionsFromDisk(workspace, *classFlag, *assignmentFlag)
		if err != nil {
			log.Fatalf("loading submissions from disk: %v", err)
		}
	} else {
		submissions, courseID, err = fetchSubmissions(ctx, workspace, *classFlag, *assignmentFlag, *credsFlag, *tokenFlag, *studentsFlag)
		if err != nil {
			log.Fatalf("fetching submissions: %v", err)
		}
	}

	if len(submissions) == 0 {
		log.Println("[main] no submissions found — nothing to grade")
		os.Exit(0)
	}
	log.Printf("[main] grading %d submissions", len(submissions))

	// ── Grade via LM Studio ───────────────────────────────────────────────────
	lmClient := lmstudio.NewClient(*lmURLFlag, *lmModelFlag, *lmTimeoutFlag)

	g := grader.New(grader.Config{
		WorkspaceRoot:   workspace,
		TeacherSolution: solutionPath,
		SystemPrompt:    string(systemPrompt),
	}, lmClient)

	marks, err := g.GradeAll(ctx, submissions)
	if err != nil {
		log.Fatalf("grading: %v", err)
	}

	// ── Write marks.json ──────────────────────────────────────────────────────
	outPath, err := grader.WriteMarks(workspace, courseID, *assignmentFlag, marks)
	if err != nil {
		log.Fatalf("writing marks: %v", err)
	}

	fmt.Printf("\nDone. Graded %d students → %s\n", len(marks), outPath)
}

// fetchSubmissions authenticates with Google Classroom and downloads submissions.
// Returns the submissions and the resolved numeric course ID.
func fetchSubmissions(ctx context.Context, workspace, className, assignmentName, credsFile, tokenFile, studentsCSV string) ([]classroom.Submission, string, error) {
	credsFile = firstNonEmpty(credsFile, os.Getenv("GOOGLE_CREDENTIALS_FILE"))
	tokenFile = firstNonEmpty(tokenFile, os.Getenv("GOOGLE_TOKEN_FILE"))

	if credsFile == "" {
		return nil, "", fmt.Errorf("Google credentials file required: use -credentials or GOOGLE_CREDENTIALS_FILE env var")
	}
	if tokenFile == "" {
		return nil, "", fmt.Errorf("Google token file required: use -token or GOOGLE_TOKEN_FILE env var")
	}

	svc, httpClient, err := classroom.NewService(ctx, credsFile, tokenFile)
	if err != nil {
		return nil, "", fmt.Errorf("creating classroom service: %w", err)
	}

	// Resolve course by partial name match
	courses, err := classroom.ListCourses(ctx, svc)
	if err != nil {
		return nil, "", fmt.Errorf("listing courses: %w", err)
	}
	course, err := findByName(courses, className, func(c classroom.Course) string { return c.Name })
	if err != nil {
		// Also try exact ID match
		for _, c := range courses {
			if c.ID == className {
				course = c
				err = nil
				break
			}
		}
		if err != nil {
			return nil, "", fmt.Errorf("finding course %q: %w", className, err)
		}
	}

	// Resolve assignment by partial name match
	assignments, err := classroom.ListAssignments(ctx, svc, course.ID)
	if err != nil {
		return nil, "", fmt.Errorf("listing assignments: %w", err)
	}
	assignment, err := findByName(assignments, assignmentName, func(a classroom.Assignment) string { return a.Title })
	if err != nil {
		return nil, "", fmt.Errorf("finding assignment %q: %w", assignmentName, err)
	}

	submissionsDir := filepath.Join(workspace, "submissions")
	filter := classroom.NewStudentFilter(splitCSV(studentsCSV))

	log.Printf("[main] fetching submissions: course=%s (%s) assignment=%q", course.Name, course.ID, assignment.Title)
	subs, err := classroom.DownloadSubmissions(ctx, svc, httpClient, course.ID, assignment.ID, assignment.Title, submissionsDir, filter)
	return subs, course.ID, err
}

// loadSubmissionsFromDisk reconstructs []classroom.Submission from already-downloaded
// files on disk when --skip-fetch is used.
// Directory layout: submissions/<courseID>/<assignmentName>/<studentID>/<timestamp>/
func loadSubmissionsFromDisk(workspace, className, assignmentName string) ([]classroom.Submission, string, error) {
	// Try to find the assignment dir — className may be a course ID or a name
	// that was already sanitized on a previous fetch.
	assignmentDir, courseID, err := findAssignmentDir(workspace, className, assignmentName)
	if err != nil {
		return nil, "", err
	}

	entries, err := os.ReadDir(assignmentDir)
	if err != nil {
		return nil, "", fmt.Errorf("reading assignment dir %s: %w", assignmentDir, err)
	}

	var submissions []classroom.Submission
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		studentID := e.Name()
		studentDir := filepath.Join(assignmentDir, studentID)

		latestDir, err := latestSubdir(studentDir)
		if err != nil {
			log.Printf("[main] WARN: skipping %s: %v", studentID, err)
			continue
		}

		profile, _ := readStudentJSON(filepath.Join(latestDir, "student.json"))
		if profile.ID == "" {
			profile.ID = studentID
		}

		var files []classroom.DownloadedFile
		fileEntries, err := os.ReadDir(latestDir)
		if err != nil {
			return nil, "", err
		}
		for _, fe := range fileEntries {
			if fe.IsDir() {
				continue
			}
			files = append(files, classroom.DownloadedFile{
				Name: fe.Name(),
				Path: filepath.Join(latestDir, fe.Name()),
			})
		}

		submissions = append(submissions, classroom.Submission{
			StudentID:    profile.ID,
			StudentName:  profile.FullName,
			StudentEmail: profile.Email,
			Files:        files,
		})
	}
	return submissions, courseID, nil
}

// findAssignmentDir locates submissions/<courseID>/<assignmentName> by trying
// several path variants — the className might be a raw course ID or a sanitized name.
func findAssignmentDir(workspace, className, assignmentName string) (string, string, error) {
	submissionsRoot := filepath.Join(workspace, "submissions")

	// Walk the top-level entries of submissions/ to find a matching course dir
	topEntries, err := os.ReadDir(submissionsRoot)
	if err != nil {
		return "", "", fmt.Errorf("reading submissions dir %s: %w", submissionsRoot, err)
	}

	candidates := []string{
		className,
		classroom.Sanitize(className),
	}

	for _, entry := range topEntries {
		if !entry.IsDir() {
			continue
		}
		for _, cand := range candidates {
			if entry.Name() == cand {
				// Found course dir — now find assignment subdir
				courseDir := filepath.Join(submissionsRoot, entry.Name())
				assignCandidates := []string{
					assignmentName,
					classroom.Sanitize(assignmentName),
					strings.ToUpper(assignmentName),
					strings.ToLower(assignmentName),
				}
				for _, ac := range assignCandidates {
					p := filepath.Join(courseDir, ac)
					if info, err := os.Stat(p); err == nil && info.IsDir() {
						return p, entry.Name(), nil
					}
				}
				// Try case-insensitive scan of course dir
				subEntries, _ := os.ReadDir(courseDir)
				for _, se := range subEntries {
					if se.IsDir() && strings.EqualFold(se.Name(), assignmentName) {
						return filepath.Join(courseDir, se.Name()), entry.Name(), nil
					}
				}
			}
		}
	}
	return "", "", fmt.Errorf("could not find submissions dir for class=%q assignment=%q under %s", className, assignmentName, submissionsRoot)
}

// latestSubdir returns the path to the lexicographically last subdirectory
// (timestamp dirs like "2026-05-26T21-21-41" sort correctly).
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
	if err := json.Unmarshal(data, &p); err != nil {
		return classroom.StudentProfile{}, err
	}
	return p, nil
}

// ── small helpers ─────────────────────────────────────────────────────────────

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

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

// findByName does a Unicode-normalized, case-insensitive contains-match.
// This mirrors the MCP server's normalizeStr logic so Cyrillic names, mixed
// case, and unusual Unicode decompositions all match correctly.
func findByName[T any](items []T, name string, nameOf func(T) string) (T, error) {
	needle := normalizeStr(name)
	for _, item := range items {
		if strings.Contains(normalizeStr(nameOf(item)), needle) {
			return item, nil
		}
	}
	var zero T
	return zero, fmt.Errorf("no match for %q", name)
}

// normalizeStr applies Unicode NFC normalization and Unicode-aware lowercasing
// so that visually identical strings with different encodings compare equal.
// Matches the MCP server's normalizeStr exactly.
func normalizeStr(s string) string {
	s = strings.TrimSpace(s)
	s = norm.NFC.String(s)
	s = cases.Lower(language.Und).String(s)
	return s
}
