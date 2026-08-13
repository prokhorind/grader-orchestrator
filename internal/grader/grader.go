// Package grader orchestrates grading a batch of student submissions against
// a teacher solution using a local LM Studio model.
package grader

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/prokhorind/classroom-grader/internal/classroom"
	"github.com/prokhorind/classroom-grader/internal/lmstudio"
)

// Mark is one graded student record — mirrors the marks.json schema.
type Mark struct {
	StudentName string `json:"student_name"`
	StudentID   string `json:"student_id"`
	Mark        int    `json:"mark"`
	Deductions  string `json:"deductions"`
	Comment     string `json:"comment"`
}

// Config holds everything the grader needs.
type Config struct {
	// WorkspaceRoot is the steering project root (where submissions/ lives).
	WorkspaceRoot string
	// TeacherSolution is the path to the reference solution file.
	TeacherSolution string
	// SystemPrompt is the full text of prompts/grader.md.
	SystemPrompt string
}

// Grader grades a set of already-downloaded submissions.
type Grader struct {
	cfg    Config
	client *lmstudio.Client
}

// New creates a Grader.
func New(cfg Config, client *lmstudio.Client) *Grader {
	return &Grader{cfg: cfg, client: client}
}

// GradeAll grades every student in submissions and returns the mark list.
// submissions comes directly from classroom.DownloadSubmissions.
func (g *Grader) GradeAll(ctx context.Context, submissions []classroom.Submission) ([]Mark, error) {
	teacherCode, err := os.ReadFile(g.cfg.TeacherSolution)
	if err != nil {
		return nil, fmt.Errorf("reading teacher solution %s: %w", g.cfg.TeacherSolution, err)
	}

	var marks []Mark
	for _, sub := range submissions {
		log.Printf("[grader] grading %s (%s)", sub.StudentName, sub.StudentID)

		studentCode, err := readStudentCode(sub)
		if err != nil {
			log.Printf("[grader] WARN: could not read files for %s: %v — scoring 1", sub.StudentName, err)
			marks = append(marks, Mark{
				StudentName: sub.StudentName,
				StudentID:   sub.StudentID,
				Mark:        1,
				Deductions:  "could not read submission files",
				Comment:     "Не вдалося прочитати файли роботи.",
			})
			continue
		}

		mark, err := g.gradeOne(ctx, sub, string(teacherCode), studentCode)
		if err != nil {
			return nil, fmt.Errorf("grading %s: %w", sub.StudentName, err)
		}
		marks = append(marks, mark)
		log.Printf("[grader] %s → %d", sub.StudentName, mark.Mark)
	}
	return marks, nil
}

// gradeOne sends a single student's submission to LM Studio and parses the result.
func (g *Grader) gradeOne(ctx context.Context, sub classroom.Submission, teacherCode, studentCode string) (Mark, error) {
	userPrompt := buildUserPrompt(sub.StudentName, sub.StudentID, teacherCode, studentCode)

	// Append /no_think to disable the thinking/reasoning preamble on Qwen3
	// and similar models that support this token. Has no effect on models that
	// don't recognise it.
	systemPrompt := strings.TrimSpace(g.cfg.SystemPrompt) + "\n/no_think"

	messages := []lmstudio.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	reply, err := g.client.Complete(ctx, messages)
	if err != nil {
		return Mark{}, fmt.Errorf("LM Studio call failed: %w", err)
	}

	mark, err := parseMark(reply, sub.StudentName, sub.StudentID)
	if err != nil {
		// Log the raw reply so the user can debug prompt/model issues
		log.Printf("[grader] WARN: could not parse LM Studio reply for %s:\n%s", sub.StudentName, reply)
		return Mark{}, fmt.Errorf("parsing mark for %s: %w", sub.StudentName, err)
	}
	return mark, nil
}

// readStudentCode reads all non-student.json files from the latest submission
// version and concatenates them separated by headers.
func readStudentCode(sub classroom.Submission) (string, error) {
	// sub.Files already contains only the latest version's files (as returned
	// by DownloadSubmissions — each run uses a fresh timestamp dir).
	var parts []string
	for _, f := range sub.Files {
		if filepath.Base(f.Path) == "student.json" {
			continue
		}
		if strings.HasSuffix(f.Name, ".skipped") {
			continue
		}
		data, err := os.ReadFile(f.Path)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", f.Path, err)
		}
		parts = append(parts, fmt.Sprintf("=== %s ===\n%s", f.Name, string(data)))
	}
	if len(parts) == 0 {
		return "", nil // empty submission — grader will score 1
	}
	return strings.Join(parts, "\n\n"), nil
}

// buildUserPrompt constructs the per-student grading message.
// The system prompt (grader.md) already has the full rubric and workflow;
// we just need to supply the concrete inputs.
func buildUserPrompt(name, id, teacherCode, studentCode string) string {
	var sb strings.Builder
	sb.WriteString("Grade the following student submission.\n\n")
	sb.WriteString("## Student\n")
	sb.WriteString(fmt.Sprintf("- Name: %s\n", name))
	sb.WriteString(fmt.Sprintf("- ID: %s\n\n", id))
	sb.WriteString("## Teacher Solution\n```\n")
	sb.WriteString(teacherCode)
	sb.WriteString("\n```\n\n")
	sb.WriteString("## Student Submission\n```\n")
	if studentCode == "" {
		sb.WriteString("(no submission)\n")
	} else {
		sb.WriteString(studentCode)
	}
	sb.WriteString("\n```\n\n")
	sb.WriteString("Respond with a JSON object (not an array) for this single student:\n")
	sb.WriteString("{\n")
	sb.WriteString(`  "student_name": "string",` + "\n")
	sb.WriteString(`  "student_id": "string",` + "\n")
	sb.WriteString(`  "mark": <integer 1-12>,` + "\n")
	sb.WriteString(`  "deductions": "short factual notes in English",` + "\n")
	sb.WriteString(`  "comment": "1-2 sentences in Ukrainian, friendly tone"` + "\n")
	sb.WriteString("}\n")
	sb.WriteString("Output ONLY the JSON object — no markdown fences, no extra text.\n")
	return sb.String()
}

// parseMark extracts a Mark from the LLM reply.
// Handles three output styles models may produce:
//   - bare JSON object
//   - JSON wrapped in markdown fences
//   - Qwen3 thinking models: <think>...</think> block followed by JSON
func parseMark(reply, fallbackName, fallbackID string) (Mark, error) {
	clean := strings.TrimSpace(reply)

	// Strip <think>...</think> block emitted by Qwen3 thinking models
	// when /no_think is not honoured or the model ignores it.
	if start := strings.Index(clean, "<think>"); start != -1 {
		if end := strings.Index(clean, "</think>"); end != -1 && end > start {
			clean = strings.TrimSpace(clean[end+len("</think>"):])
		}
	}

	// Strip markdown code fences if present
	if idx := strings.Index(clean, "```"); idx != -1 {
		start := strings.Index(clean[idx:], "\n")
		if start == -1 {
			start = idx + 3
		} else {
			start = idx + start + 1 // skip the opening fence line
		}
		end := strings.LastIndex(clean, "```")
		if end > start {
			clean = strings.TrimSpace(clean[start:end])
		}
	}

	// Find the outermost JSON object
	start := strings.Index(clean, "{")
	end := strings.LastIndex(clean, "}")
	if start == -1 || end == -1 || end < start {
		return Mark{}, fmt.Errorf("no JSON object found in reply")
	}
	clean = clean[start : end+1]

	var m Mark
	if err := json.Unmarshal([]byte(clean), &m); err != nil {
		return Mark{}, fmt.Errorf("json.Unmarshal: %w", err)
	}

	// Fill in student identity from what we know if the model left them blank
	if m.StudentName == "" {
		m.StudentName = fallbackName
	}
	if m.StudentID == "" {
		m.StudentID = fallbackID
	}

	// Clamp mark to valid range
	if m.Mark < 1 {
		m.Mark = 1
	}
	if m.Mark > 12 {
		m.Mark = 12
	}

	return m, nil
}

// WriteMarks serialises marks to marks.json inside the timestamp directory.
// Path: <workspaceRoot>/submissions/<courseID>/<assignmentTitle>/<timestamp>/marks.json
//
// Merge behaviour: if marks.json already exists for that timestamp (e.g. a partial
// re-grade), the incoming marks replace matching students (by StudentName) and all
// other existing entries are kept. This lets you re-grade a subset without wiping
// the rest.
func WriteMarks(workspaceRoot, courseID, assignmentTitle, timestamp string, marks []Mark) (string, error) {
	dir := filepath.Join(workspaceRoot, "submissions",
		classroom.Sanitize(courseID),
		classroom.Sanitize(assignmentTitle),
		timestamp)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating output dir: %w", err)
	}

	outPath := filepath.Join(dir, "marks.json")

	// Merge with any existing marks for this timestamp.
	merged := mergeMarks(outPath, marks)

	sort.Slice(merged, func(i, j int) bool {
		return merged[i].StudentName < merged[j].StudentName
	})

	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshalling marks: %w", err)
	}
	if err := os.WriteFile(outPath, data, 0644); err != nil {
		return "", fmt.Errorf("writing marks.json: %w", err)
	}
	return outPath, nil
}

// mergeMarks reads existing marks from path (if it exists) and overlays incoming
// marks on top, matching by StudentName. Students not present in incoming are kept.
func mergeMarks(path string, incoming []Mark) []Mark {
	existing := readMarksFile(path)

	// Index existing by name for O(1) replacement.
	byName := make(map[string]Mark, len(existing))
	for _, m := range existing {
		byName[m.StudentName] = m
	}
	// Overlay incoming.
	for _, m := range incoming {
		byName[m.StudentName] = m
	}

	merged := make([]Mark, 0, len(byName))
	for _, m := range byName {
		merged = append(merged, m)
	}
	return merged
}

// ReadMarksFile reads a marks.json file and returns its contents, or nil on error.
func ReadMarksFile(path string) []Mark {
	return readMarksFile(path)
}

// readMarksFile is the internal version used by WriteMarks.
func readMarksFile(path string) []Mark {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var marks []Mark
	if err := json.Unmarshal(data, &marks); err != nil {
		return nil
	}
	return marks
}
