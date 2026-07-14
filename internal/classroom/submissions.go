package classroom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"
	googleclassroom "google.golang.org/api/classroom/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	retryMaxAttempts = 3
	retryBaseDelay   = 2 * time.Second
)

// Submission represents a single student's downloaded submission.
type Submission struct {
	StudentID    string           `json:"student_id"`
	StudentName  string           `json:"student_name"`
	StudentEmail string           `json:"student_email"`
	Version      string           `json:"version"`
	Files        []DownloadedFile `json:"files"`
}

// DownloadedFile is a file that was downloaded as part of a submission.
type DownloadedFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// StudentFilter holds a pre-processed set of name tokens used to match students.
// An empty filter means "match everyone".
type StudentFilter struct {
	tokens []string
}

// NewStudentFilter builds a filter from a slice of "Surname" or "Surname Name" strings.
// Pass nil or an empty slice to download all students.
func NewStudentFilter(names []string) StudentFilter {
	var tokens []string
	for _, n := range names {
		t := strings.ToLower(strings.TrimSpace(n))
		if t != "" {
			tokens = append(tokens, t)
		}
	}
	return StudentFilter{tokens: tokens}
}

// Match returns true when the profile should be included in the download.
func (f StudentFilter) Match(profile StudentProfile) bool {
	if len(f.tokens) == 0 {
		return true
	}
	lower := strings.ToLower(profile.FullName)
	for _, tok := range f.tokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// Google Docs MIME types — exported as plain text.
var googleDocsMimeTypes = map[string]struct{}{
	"application/vnd.google-apps.document":     {},
	"application/vnd.google-apps.spreadsheet":  {},
	"application/vnd.google-apps.presentation": {},
}

// isRetryable returns true for Google API 5xx server errors worth retrying.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	for err != nil {
		if e, ok := err.(*googleapi.Error); ok {
			return e.Code >= 500 && e.Code < 600
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
		} else {
			break
		}
	}
	return false
}

// withRetry calls fn up to retryMaxAttempts times, backing off on 5xx errors.
func withRetry(ctx context.Context, label string, fn func() error) error {
	var err error
	for attempt := 1; attempt <= retryMaxAttempts; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		if !isRetryable(err) {
			return err
		}
		delay := retryBaseDelay * time.Duration(attempt)
		log.Printf("[retry] %s: attempt %d/%d failed (%v) — retrying in %s", label, attempt, retryMaxAttempts, err, delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("%s failed after %d attempts: %w", label, retryMaxAttempts, err)
}

// DownloadSubmissions fetches all student submissions for an assignment and
// saves them under baseDir/<courseID>/<assignmentTitle>/<studentID>/<timestamp>/
func DownloadSubmissions(ctx context.Context, svc *googleclassroom.Service, httpClient *http.Client, courseID, courseWorkID, assignmentTitle, baseDir string, filter StudentFilter) ([]Submission, error) {
	driveSvc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("creating drive service: %w", err)
	}

	timestamp := time.Now().UTC().Format("2006-01-02T15-04-05")
	var submissions []Submission

	log.Printf("[get_submissions] starting download: course=%s assignment=%q dir=%s", courseID, assignmentTitle, baseDir)

	err = svc.Courses.CourseWork.StudentSubmissions.
		List(courseID, courseWorkID).
		Pages(ctx, func(page *googleclassroom.ListStudentSubmissionsResponse) error {
			for _, sub := range page.StudentSubmissions {
				log.Printf("[get_submissions] processing student=%s", sub.UserId)

				profile, err := GetStudentProfile(ctx, svc, courseID, sub.UserId)
				if err != nil {
					log.Printf("[get_submissions]   WARN: could not fetch profile for %s: %v", sub.UserId, err)
					profile = StudentProfile{ID: sub.UserId}
				}

				if !filter.Match(profile) {
					log.Printf("[get_submissions] skipping student=%s name=%q (not in filter)", sub.UserId, profile.FullName)
					continue
				}

				versionDir := filepath.Join(baseDir, courseID, Sanitize(assignmentTitle), sub.UserId, timestamp)
				if err := os.MkdirAll(versionDir, 0755); err != nil {
					return err
				}

				var downloaded []DownloadedFile

				if sub.AssignmentSubmission != nil {
					for _, att := range sub.AssignmentSubmission.Attachments {
						var df *DownloadedFile
						var err error
						switch {
						case att.DriveFile != nil:
							log.Printf("[get_submissions]   drive file: id=%s title=%q", att.DriveFile.Id, att.DriveFile.Title)
							df, err = handleDriveAttachment(ctx, driveSvc, att.DriveFile, versionDir)
						case att.Link != nil:
							log.Printf("[get_submissions]   link: %s", att.Link.Url)
							df, err = handleLinkAttachment(att.Link, versionDir)
						}
						if err != nil {
							log.Printf("[get_submissions]   ERROR: %v", err)
							skipped := saveSkippedFile(att, err, versionDir)
							if skipped != nil {
								downloaded = append(downloaded, *skipped)
							}
							continue
						}
						if df != nil {
							log.Printf("[get_submissions]   saved: %s", df.Path)
							downloaded = append(downloaded, *df)
						}
					}
				}

				if sub.ShortAnswerSubmission != nil && sub.ShortAnswerSubmission.Answer != "" {
					df, err := saveTextSubmission("short_answer.txt", sub.ShortAnswerSubmission.Answer, versionDir)
					if err != nil {
						return err
					}
					downloaded = append(downloaded, *df)
				}

				if err := writeStudentInfo(versionDir, profile); err != nil {
					return fmt.Errorf("writing student info: %w", err)
				}

				log.Printf("[get_submissions] done student=%s name=%q files=%d", sub.UserId, profile.FullName, len(downloaded))

				submissions = append(submissions, Submission{
					StudentID:    sub.UserId,
					StudentName:  profile.FullName,
					StudentEmail: profile.Email,
					Version:      timestamp,
					Files:        downloaded,
				})
			}
			return nil
		})

	log.Printf("[get_submissions] finished: total=%d err=%v", len(submissions), err)
	return submissions, err
}

func handleDriveAttachment(ctx context.Context, svc *drive.Service, df *googleclassroom.DriveFile, destDir string) (*DownloadedFile, error) {
	var meta *drive.File
	err := withRetry(ctx, fmt.Sprintf("fetch metadata %s", df.Title), func() error {
		var e error
		meta, e = svc.Files.Get(df.Id).Fields("mimeType", "name").Context(ctx).Do()
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("fetching metadata for %s: %w", df.Title, err)
	}

	if _, isGoogleDoc := googleDocsMimeTypes[meta.MimeType]; isGoogleDoc {
		return exportGoogleDoc(ctx, svc, df, destDir, meta.MimeType)
	}

	if meta.MimeType == "application/pdf" {
		return downloadAndExtractPDF(ctx, svc, df, destDir)
	}

	destPath := filepath.Join(destDir, df.Title)
	if err := downloadDriveFile(ctx, svc, df.Id, destPath); err != nil {
		return nil, fmt.Errorf("downloading %s: %w", df.Title, err)
	}
	return &DownloadedFile{Name: df.Title, Path: destPath}, nil
}

func downloadAndExtractPDF(ctx context.Context, svc *drive.Service, df *googleclassroom.DriveFile, destDir string) (*DownloadedFile, error) {
	tmp, err := os.CreateTemp("", "classroom-pdf-*.pdf")
	if err != nil {
		return nil, fmt.Errorf("creating temp file for %s: %w", df.Title, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	var resp *http.Response
	err = withRetry(ctx, fmt.Sprintf("download pdf %s", df.Title), func() error {
		var e error
		resp, e = svc.Files.Get(df.Id).Context(ctx).Download()
		return e
	})
	if err != nil {
		tmp.Close()
		return nil, fmt.Errorf("downloading PDF %s: %w", df.Title, err)
	}
	defer resp.Body.Close()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("writing temp PDF %s: %w", df.Title, err)
	}
	tmp.Close()

	text, err := extractPDFText(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("extracting text from PDF %s: %w", df.Title, err)
	}

	name := df.Title + ".txt"
	destPath := filepath.Join(destDir, name)
	if err := os.WriteFile(destPath, []byte(text), 0644); err != nil {
		return nil, fmt.Errorf("writing extracted PDF text for %s: %w", df.Title, err)
	}

	log.Printf("[get_submissions]   extracted %d bytes of text from PDF %s", len(text), df.Title)
	return &DownloadedFile{Name: name, Path: destPath}, nil
}

func exportGoogleDoc(ctx context.Context, svc *drive.Service, df *googleclassroom.DriveFile, destDir string, mimeType string) (*DownloadedFile, error) {
	exportMime := "text/plain"
	if mimeType == "application/vnd.google-apps.spreadsheet" {
		exportMime = "text/csv"
	}

	name := df.Title + ".txt"
	destPath := filepath.Join(destDir, name)

	var resp *http.Response
	err := withRetry(ctx, fmt.Sprintf("export %s", df.Title), func() error {
		var e error
		resp, e = svc.Files.Export(df.Id, exportMime).Context(ctx).Download()
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("exporting %q as plain text: %w", df.Title, err)
	}
	defer resp.Body.Close()

	f, err := os.Create(destPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return nil, err
	}
	return &DownloadedFile{Name: name, Path: destPath}, nil
}

func handleLinkAttachment(link *googleclassroom.Link, destDir string) (*DownloadedFile, error) {
	title := link.Title
	if title == "" {
		title = "link"
	}
	name := Sanitize(title) + ".txt"
	destPath := filepath.Join(destDir, name)
	content := fmt.Sprintf("Title: %s\nURL: %s\n", link.Title, link.Url)
	if err := os.WriteFile(destPath, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("saving link %s: %w", link.Url, err)
	}
	return &DownloadedFile{Name: name, Path: destPath}, nil
}

func extractPDFText(path string) (string, error) {
	text, err := extractPDFTextWithPdftotext(path)
	if err == nil {
		log.Printf("[extractPDFText] extracted %d bytes using pdftotext", len(text))
		return text, nil
	}
	log.Printf("[extractPDFText] pdftotext failed (%v), falling back to ledongthuc/pdf", err)
	return extractPDFTextFallback(path)
}

func extractPDFTextWithPdftotext(path string) (string, error) {
	cmd := exec.Command("pdftotext", "-layout", path, "-")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("pdftotext: %w", err)
	}
	return string(output), nil
}

func extractPDFTextFallback(path string) (string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var sb strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			log.Printf("[extractPDFText] WARN page %d: %v", i, err)
			continue
		}
		sb.WriteString(cleanPDFCode(text))
		sb.WriteString("\n\n")
	}
	return sb.String(), nil
}

func cleanPDFCode(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	lines := strings.Split(text, "\n")
	var rebuilt []string
	var currentLine strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimRight(line, " ")
		if strings.TrimSpace(trimmed) == "" {
			if currentLine.Len() > 0 {
				rebuilt = append(rebuilt, currentLine.String())
				currentLine.Reset()
			}
			rebuilt = append(rebuilt, "")
			continue
		}
		if len(trimmed) < 12 && !strings.HasSuffix(trimmed, ";") &&
			!strings.HasSuffix(trimmed, "{") && !strings.HasSuffix(trimmed, "}") {
			if currentLine.Len() > 0 {
				currentLine.WriteString(" ")
			}
			currentLine.WriteString(trimmed)
		} else {
			if currentLine.Len() > 0 {
				rebuilt = append(rebuilt, currentLine.String())
				currentLine.Reset()
			}
			rebuilt = append(rebuilt, trimmed)
		}
	}
	if currentLine.Len() > 0 {
		rebuilt = append(rebuilt, currentLine.String())
	}
	return strings.Join(rebuilt, "\n")
}

func saveTextSubmission(filename, content, destDir string) (*DownloadedFile, error) {
	destPath := filepath.Join(destDir, filename)
	if err := os.WriteFile(destPath, []byte(content), 0644); err != nil {
		return nil, err
	}
	return &DownloadedFile{Name: filename, Path: destPath}, nil
}

func writeStudentInfo(dir string, p StudentProfile) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "student.json"), data, 0644)
}

func downloadDriveFile(ctx context.Context, svc *drive.Service, fileID, destPath string) error {
	var resp *http.Response
	err := withRetry(ctx, fmt.Sprintf("download file %s", fileID), func() error {
		var e error
		resp, e = svc.Files.Get(fileID).Context(ctx).Download()
		return e
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

func saveSkippedFile(att *googleclassroom.Attachment, reason error, destDir string) *DownloadedFile {
	title := "unknown"
	if att.DriveFile != nil {
		title = att.DriveFile.Title
	} else if att.Link != nil {
		title = att.Link.Title
	}
	log.Printf("WARN: skipping attachment %q: %v", title, reason)
	name := Sanitize(title) + ".skipped"
	destPath := filepath.Join(destDir, name)
	content := fmt.Sprintf("File: %s\nError: %v\n", title, reason)
	if err := os.WriteFile(destPath, []byte(content), 0644); err != nil {
		log.Printf("WARN: could not write skipped marker for %q: %v", title, err)
		return nil
	}
	return &DownloadedFile{Name: name, Path: destPath}
}
