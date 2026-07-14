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
	"path/filepath"

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

// OAuthConfigFromFile reads OAuth2 client credentials from a Google credentials JSON file.
func OAuthConfigFromFile(credentialsFile string) (*oauth2.Config, error) {
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

// DefaultConfigPaths returns the default paths for credentials.json and token.json
// in the user's config directory (e.g. ~/.config/classroom-grader/ or Library/Application Support/classroom-grader/).
func DefaultConfigPaths() (string, string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		// Fallback to home directory if UserConfigDir fails
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("getting user home dir: %w", err)
		}
		configDir = filepath.Join(home, ".config")
	}
	appDir := filepath.Join(configDir, "classroom-grader")
	return filepath.Join(appDir, "credentials.json"), filepath.Join(appDir, "token.json"), nil
}

// ResolveCredentialsAndTokenPaths resolves paths for credentials and token files based on overrides and defaults.
func ResolveCredentialsAndTokenPaths(credsOverride, tokenOverride, mcpRootOverride string) (string, string, error) {
	credsFile := credsOverride
	if credsFile == "" {
		credsFile = os.Getenv("GOOGLE_CREDENTIALS_FILE")
	}

	tokenFile := tokenOverride
	if tokenFile == "" {
		tokenFile = os.Getenv("GOOGLE_TOKEN_FILE")
	}

	// If mcpRoot is provided, it serves as an override for directory where secrets are loaded from.
	if mcpRootOverride != "" {
		mcpRoot, err := filepath.Abs(mcpRootOverride)
		if err != nil {
			return "", "", fmt.Errorf("resolving mcp-root path: %w", err)
		}
		if credsOverride == "" && os.Getenv("GOOGLE_CREDENTIALS_FILE") == "" {
			credsFile = filepath.Join(mcpRoot, ".secrets", "credentials.json")
		}
		if tokenOverride == "" && os.Getenv("GOOGLE_TOKEN_FILE") == "" {
			tokenFile = filepath.Join(mcpRoot, ".secrets", "token.json")
		}
	}

	// If paths are still empty, fall back to default OS config folder.
	defaultCreds, defaultToken, err := DefaultConfigPaths()
	if err != nil {
		return "", "", fmt.Errorf("determining default config paths: %w", err)
	}

	if credsFile == "" {
		credsFile = defaultCreds
	}
	if tokenFile == "" {
		tokenFile = defaultToken
	}

	return credsFile, tokenFile, nil
}

// SaveToken saves a token to a file path, creating parent directories if necessary.
func SaveToken(path string, token *oauth2.Token) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("creating token file: %w", err)
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(token)
}

// RunAuthFlow performs the OAuth2 browser flow and saves the token to tokenFile.
func RunAuthFlow(ctx context.Context, credentialsFile, tokenFile string) error {
	config, err := OAuthConfigFromFile(credentialsFile)
	if err != nil {
		return err
	}

	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("\nOpen this URL in your browser:\n\n%s\n\nPaste the authorization code: ", authURL)

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		return fmt.Errorf("reading auth code: %w", err)
	}

	tok, err := config.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("exchanging auth code: %w", err)
	}

	if err := SaveToken(tokenFile, tok); err != nil {
		return fmt.Errorf("saving token: %w", err)
	}

	fmt.Printf("\nToken saved to %s — you can now run the classroom grader.\n", tokenFile)
	return nil
}

// NewService creates an authenticated Google Classroom service.
func NewService(ctx context.Context, credentialsFile, tokenFile string) (*googleclassroom.Service, *http.Client, error) {
	config, err := OAuthConfigFromFile(credentialsFile)
	if err != nil {
		return nil, nil, err
	}

	tok, err := loadToken(tokenFile)
	if err != nil {
		return nil, nil, fmt.Errorf("no token found at %s — run standard authorization flow first: %w", tokenFile, err)
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
