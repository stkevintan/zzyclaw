package copilot

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// GitHub Copilot VS Code extension OAuth app client ID
	copilotClientID = "Iv1.b507a08c87ecfe98"
	deviceCodeURL   = "https://github.com/login/device/code"
	accessTokenURL  = "https://github.com/login/oauth/access_token"
	copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
	credentialFile  = ".copilot-token.json"
)

// savedCredential is persisted to disk for reuse across restarts.
type savedCredential struct {
	GithubToken string `json:"github_token"`
	CreatedAt   int64  `json:"created_at"`
}

type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type accessTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
}

// Login performs the GitHub Device Code flow using the Copilot OAuth app
// and returns a GitHub token that works with the Copilot API.
// The token is cached to disk for reuse.
// credentialDir overrides the default credential directory (~/) when non-empty.
func Login(credentialDir string) (string, error) {
	credPath := resolveCredentialPath(credentialDir)
	// Check for cached credential first, and verify it still works.
	if token, err := loadCredential(credPath); err == nil && token != "" {
		if verr := validateCredential(token); verr == nil {
			slog.Info("using cached copilot credential")
			return token, nil
		} else {
			// Stale or revoked credential (e.g. 401 bad credentials):
			// discard it and fall through to a fresh login.
			slog.Warn("cached copilot credential invalid, re-authenticating", "error", verr)
			if rmErr := os.Remove(credPath); rmErr != nil && !os.IsNotExist(rmErr) {
				slog.Warn("failed to remove invalid copilot credential", "error", rmErr)
			}
		}
	}

	// Step 1: Request device code
	body, err := postFormJSON(deviceCodeURL, url.Values{
		"client_id": {copilotClientID},
		"scope":     {"read:user"},
	})
	if err != nil {
		return "", fmt.Errorf("copilot login: request device code: %w", err)
	}

	var device deviceCodeResponse
	if err := json.Unmarshal(body, &device); err != nil {
		return "", fmt.Errorf("copilot login: parse device code: %w", err)
	}

	// Step 2: Show user the code
	fmt.Println()
	fmt.Println("=== GitHub Copilot Login ===")
	fmt.Printf("Visit:  %s\n", device.VerificationURI)
	fmt.Printf("Code:   %s\n", device.UserCode)
	fmt.Println("Waiting for authorization...")
	fmt.Println()

	// Step 3: Poll for access token
	interval := time.Duration(device.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		token, done, err := pollAccessToken(device.DeviceCode)
		if err != nil {
			return "", err
		}
		if done {
			// Save credential
			if err := saveCredential(credPath, token); err != nil {
				slog.Warn("failed to save copilot credential", "error", err)
			}
			fmt.Println("GitHub Copilot authorized successfully!")
			return token, nil
		}
	}

	return "", fmt.Errorf("copilot login: device code expired")
}

func pollAccessToken(deviceCode string) (token string, done bool, err error) {
	body, err := postFormJSON(accessTokenURL, url.Values{
		"client_id":   {copilotClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	})
	if err != nil {
		return "", false, fmt.Errorf("copilot login: poll token: %w", err)
	}

	var tokenResp accessTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", false, fmt.Errorf("copilot login: parse token response: %w", err)
	}

	if tokenResp.AccessToken != "" {
		return tokenResp.AccessToken, true, nil
	}

	switch tokenResp.Error {
	case "authorization_pending", "slow_down":
		return "", false, nil
	case "expired_token":
		return "", false, fmt.Errorf("copilot login: device code expired")
	case "access_denied":
		return "", false, fmt.Errorf("copilot login: access denied by user")
	default:
		return "", false, fmt.Errorf("copilot login: unexpected error: %s", tokenResp.Error)
	}
}

// postFormJSON posts form data with Accept: application/json header.
func postFormJSON(endpoint string, data url.Values) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close device code response body", "error", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

// validateCredential verifies a GitHub token still works with the Copilot API
// by performing the short-lived token exchange. It returns an error when the
// token is rejected (e.g. HTTP 401 bad credentials), signalling that the cached
// credential should be discarded and re-authenticated.
func validateCredential(githubToken string) error {
	req, err := http.NewRequest(http.MethodGet, copilotTokenURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", "vscode/1.96.2")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("X-Github-Api-Version", "2025-04-01")

	// Use an explicit timeout so a hung/slow Copilot endpoint can't block startup
	// indefinitely (http.DefaultClient has no timeout).
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close credential validation response body", "error", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("copilot credential rejected (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func resolveCredentialPath(dir string) string {
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	return filepath.Join(dir, credentialFile)
}

func saveCredential(credPath, githubToken string) error {
	data, err := json.Marshal(savedCredential{
		GithubToken: githubToken,
		CreatedAt:   time.Now().Unix(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(credPath, data, 0o600)
}

func loadCredential(credPath string) (string, error) {
	data, err := os.ReadFile(credPath)
	if err != nil {
		return "", err
	}
	var cred savedCredential
	if err := json.Unmarshal(data, &cred); err != nil {
		return "", err
	}
	if strings.TrimSpace(cred.GithubToken) == "" {
		return "", fmt.Errorf("empty token")
	}
	return cred.GithubToken, nil
}
