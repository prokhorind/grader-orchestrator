// Package classroom provides a minimal Google Classroom API client.
// This is inlined from github.com/prokhorind/google-classroom-mcp/classroom
// so the grader has no external module dependency on the MCP project.
package classroom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	googleclassroom "google.golang.org/api/classroom/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

var scopes = []string{
	googleclassroom.ClassroomCoursesReadonlyScope,
	googleclassroom.ClassroomCourseworkStudentsScope,
	googleclassroom.ClassroomCourseworkStudentsReadonlyScope,
	googleclassroom.ClassroomStudentSubmissionsStudentsReadonlyScope,
	googleclassroom.ClassroomCourseworkMeScope,
	googleclassroom.ClassroomRostersReadonlyScope,
	googleclassroom.ClassroomProfileEmailsScope,
	googleclassroom.ClassroomProfilePhotosScope,
	drive.DriveReadonlyScope,
}

// oauthConfigFromFile reads OAuth2 client credentials from a Google credentials JSON file.
func oauthConfigFromFile(credentialsFile string) (*oauth2.Config, error) {
	data, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("reading credentials file %s: %w", credentialsFile, err)
	}
	config, err := google.ConfigFromJSON(data, scopes...)
	if err != nil {
		return nil, fmt.Errorf("parsing credentials file: %w", err)
	}
	config.RedirectURL = "urn:ietf:wg:oauth:2.0:oob"
	return config, nil
}

// NewService creates an authenticated Google Classroom service.
// Fails immediately if no cached token exists — run `go run ./cmd/auth` in the
// MCP server repo first to generate the token.
func NewService(ctx context.Context, credentialsFile, tokenFile string) (*googleclassroom.Service, *http.Client, error) {
	config, err := oauthConfigFromFile(credentialsFile)
	if err != nil {
		return nil, nil, err
	}

	tok, err := loadToken(tokenFile)
	if err != nil {
		return nil, nil, fmt.Errorf("no token found at %s — run `go run ./cmd/auth` first: %w", tokenFile, err)
	}

	httpClient := config.Client(ctx, tok)
	svc, err := googleclassroom.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, nil, fmt.Errorf("creating classroom service: %w", err)
	}
	return svc, httpClient, nil
}

func loadToken(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	return tok, json.NewDecoder(f).Decode(tok)
}
