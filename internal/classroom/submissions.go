package classroom

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prokhorind/classroom-grader/internal/vision"
	googleclassroom "google.golang.org/api/classroom/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// plainTextExtensions lists file extensions that can be read as plain text
// and passed directly to the grader without any conversion.
var plainTextExtensions = map[string]struct{}{
	".py": {}, ".js": {}, ".ts": {}, ".json": {}, ".txt": {},
	".md": {}, ".csv": {}, ".java": {}, ".c": {}, ".cpp": {},
	".go": {}, ".rb": {}, ".php": {}, ".sh": {}, ".yaml": {},
	".yml": {}, ".xml": {}, ".html": {}, ".css": {}, ".sql": {},
	".r": {}, ".kt": {}, ".swift": {}, ".rs": {}, ".cs": {},
	".h": {}, ".hpp": {},
}

// imageMIMETypes lists Drive MIME types that should be sent to the vision
// client for OCR instead of downloaded as raw binary.
var imageMIMETypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"image/bmp":  ".bmp",
	"image/tiff": ".tiff",
	"image/heic": ".heic",
	"image/heif": ".heif",
}

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
// saves them under baseDir/<courseFolderName>/<assignmentTitle>/<studentID>/<timestamp>/
//
// visionClient is optional (pass nil to disable). When provided, PDFs and
// images are sent to the vision model for text extraction instead of being
// stored as raw binary files.
func DownloadSubmissions(ctx context.Context, svc *googleclassroom.Service, httpClient *http.Client, courseID, courseFolderName, courseWorkID, assignmentTitle, baseDir string, filter StudentFilter, visionClient vision.Client) ([]Submission, error) {
	driveSvc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("creating drive service: %w", err)
	}

	timestamp := time.Now().Local().Format("2006-01-02T15-04-05")
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

				studentDir := sub.UserId
				if profile.FullName != "" {
					studentDir = Sanitize(profile.FullName)
				}
				versionDir := filepath.Join(baseDir, Sanitize(courseFolderName), Sanitize(assignmentTitle), studentDir, timestamp)
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
							df, err = handleDriveAttachment(ctx, driveSvc, att.DriveFile, versionDir, visionClient)
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

func handleDriveAttachment(ctx context.Context, svc *drive.Service, df *googleclassroom.DriveFile, destDir string, visionClient vision.Client) (*DownloadedFile, error) {
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
		return downloadAndExtractPDF(ctx, svc, df, destDir, visionClient)
	}

	// .docx — extract plain text from the Word XML.
	if meta.MimeType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" ||
		strings.ToLower(filepath.Ext(df.Title)) == ".docx" {
		return downloadAndExtractDocx(ctx, svc, df, destDir)
	}

	// Plain-text source files — download and keep as-is.
	if _, isText := plainTextExtensions[strings.ToLower(filepath.Ext(df.Title))]; isText {
		destPath := uniquePath(destDir, df.Title)
		if err := downloadDriveFile(ctx, svc, df.Id, destPath); err != nil {
			return nil, fmt.Errorf("downloading %s: %w", df.Title, err)
		}
		return &DownloadedFile{Name: filepath.Base(destPath), Path: destPath}, nil
	}

	// Image types — use the vision client for OCR when available, otherwise
	// download the raw file and let the grader deal with it.
	if ext, isImage := imageMIMETypes[meta.MimeType]; isImage {
		if visionClient != nil {
			return downloadAndExtractImage(ctx, svc, df, destDir, ext, visionClient)
		}
		// No vision client: fall through and store the raw binary.
		log.Printf("[get_submissions]   WARN: image %q downloaded as raw binary (no vision client configured)", df.Title)
	}

	destPath := uniquePath(destDir, df.Title)
	if err := downloadDriveFile(ctx, svc, df.Id, destPath); err != nil {
		return nil, fmt.Errorf("downloading %s: %w", df.Title, err)
	}
	return &DownloadedFile{Name: filepath.Base(destPath), Path: destPath}, nil
}

// downloadAndExtractPDF downloads a PDF from Drive, saves the original PDF to
// destDir for reference, and uses the vision client to transcribe its text.
// If no vision client is configured the raw PDF is the only output saved.
func downloadAndExtractPDF(ctx context.Context, svc *drive.Service, df *googleclassroom.DriveFile, destDir string, visionClient vision.Client) (*DownloadedFile, error) {
	// Always save the original PDF so it can be inspected later.
	pdfPath := filepath.Join(destDir, df.Title)

	var resp *http.Response
	err := withRetry(ctx, fmt.Sprintf("download pdf %s", df.Title), func() error {
		var e error
		resp, e = svc.Files.Get(df.Id).Context(ctx).Download()
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("downloading PDF %s: %w", df.Title, err)
	}
	defer resp.Body.Close()

	pdfFile, err := os.Create(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("creating PDF file %s: %w", df.Title, err)
	}
	if _, err := io.Copy(pdfFile, resp.Body); err != nil {
		pdfFile.Close()
		return nil, fmt.Errorf("writing PDF %s: %w", df.Title, err)
	}
	pdfFile.Close()

	if visionClient == nil {
		log.Printf("[get_submissions]   WARN: no vision client — saved raw PDF %s", df.Title)
		return &DownloadedFile{Name: df.Title, Path: pdfPath}, nil
	}

	text, err := visionClient.ExtractText(ctx, pdfPath)
	if err != nil {
		return nil, fmt.Errorf("vision extraction for PDF %s: %w", df.Title, err)
	}

	name := df.Title + ".txt"
	txtPath := filepath.Join(destDir, name)
	if err := os.WriteFile(txtPath, []byte(text), 0644); err != nil {
		return nil, fmt.Errorf("writing extracted PDF text for %s: %w", df.Title, err)
	}

	log.Printf("[get_submissions]   vision extracted %d bytes from PDF %s → %s", len(text), df.Title, name)
	return &DownloadedFile{Name: name, Path: txtPath}, nil
}

// downloadAndExtractImage downloads an image from Drive, calls the vision
// client to transcribe its text, and saves the result as a .txt file.
func downloadAndExtractImage(ctx context.Context, svc *drive.Service, df *googleclassroom.DriveFile, destDir, ext string, visionClient vision.Client) (*DownloadedFile, error) {
	tmp, err := os.CreateTemp("", "classroom-img-*"+ext)
	if err != nil {
		return nil, fmt.Errorf("creating temp file for %s: %w", df.Title, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	var resp *http.Response
	err = withRetry(ctx, fmt.Sprintf("download image %s", df.Title), func() error {
		var e error
		resp, e = svc.Files.Get(df.Id).Context(ctx).Download()
		return e
	})
	if err != nil {
		tmp.Close()
		return nil, fmt.Errorf("downloading image %s: %w", df.Title, err)
	}
	defer resp.Body.Close()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("writing temp image %s: %w", df.Title, err)
	}
	tmp.Close()

	text, err := visionClient.ExtractText(ctx, tmpPath)
	if err != nil {
		return nil, fmt.Errorf("vision extraction for image %s: %w", df.Title, err)
	}

	name := df.Title + ".txt"
	destPath := filepath.Join(destDir, name)
	if err := os.WriteFile(destPath, []byte(text), 0644); err != nil {
		return nil, fmt.Errorf("writing extracted image text for %s: %w", df.Title, err)
	}

	log.Printf("[get_submissions]   vision extracted %d bytes from image %s", len(text), df.Title)
	return &DownloadedFile{Name: name, Path: destPath}, nil
}

func exportGoogleDoc(ctx context.Context, svc *drive.Service, df *googleclassroom.DriveFile, destDir string, mimeType string) (*DownloadedFile, error) {
	exportMime := "text/plain"
	if mimeType == "application/vnd.google-apps.spreadsheet" {
		exportMime = "text/csv"
	}

	// Use uniquePath so that multiple Google Docs submitted by the same student
	// (e.g. one per task) get distinct filenames instead of overwriting each other.
	name := df.Title + ".txt"
	destPath := uniquePath(destDir, name)

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
	return &DownloadedFile{Name: filepath.Base(destPath), Path: destPath}, nil
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

// uniquePath returns a destination path that doesn't already exist on disk.
// If <destDir>/<name> is free it is returned as-is; otherwise it appends a
// numeric suffix before the extension: "Task 1.txt", "Task 1_2.txt", etc.
func uniquePath(destDir, name string) string {
	candidate := filepath.Join(destDir, name)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate = filepath.Join(destDir, fmt.Sprintf("%s_%d%s", base, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// downloadAndExtractDocx downloads a .docx file from Drive and extracts its
// plain text by reading the word/document.xml entry inside the ZIP container.
func downloadAndExtractDocx(ctx context.Context, svc *drive.Service, df *googleclassroom.DriveFile, destDir string) (*DownloadedFile, error) {
	// Download the raw .docx bytes into memory.
	var buf bytes.Buffer
	err := withRetry(ctx, fmt.Sprintf("download docx %s", df.Title), func() error {
		resp, e := svc.Files.Get(df.Id).Context(ctx).Download()
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		buf.Reset()
		_, e = io.Copy(&buf, resp.Body)
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("downloading docx %s: %w", df.Title, err)
	}

	// Open the ZIP archive that is the .docx container.
	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return nil, fmt.Errorf("opening docx zip for %s: %w", df.Title, err)
	}

	// Find and parse word/document.xml.
	var text string
	for _, f := range r.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("opening word/document.xml in %s: %w", df.Title, err)
		}
		text, err = extractDocxText(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("extracting text from %s: %w", df.Title, err)
		}
		break
	}

	if text == "" {
		log.Printf("[get_submissions]   WARN: no text extracted from docx %q", df.Title)
	}

	name := df.Title
	if !strings.HasSuffix(strings.ToLower(name), ".docx") {
		name += ".docx"
	}
	name += ".txt"
	destPath := uniquePath(destDir, name)
	if err := os.WriteFile(destPath, []byte(text), 0644); err != nil {
		return nil, fmt.Errorf("writing docx text for %s: %w", df.Title, err)
	}

	log.Printf("[get_submissions]   extracted %d bytes from docx %s → %s", len(text), df.Title, filepath.Base(destPath))
	return &DownloadedFile{Name: filepath.Base(destPath), Path: destPath}, nil
}

// extractDocxText walks the XML token stream of word/document.xml and
// collects all character data, inserting newlines at paragraph boundaries.
func extractDocxText(r io.Reader) (string, error) {
	var sb strings.Builder
	dec := xml.NewDecoder(r)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			// w:p marks a paragraph — add a newline to separate them.
			if t.Name.Local == "p" && t.Name.Space != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
			}
		case xml.CharData:
			sb.Write(t)
		}
	}
	return strings.TrimSpace(sb.String()), nil
}
