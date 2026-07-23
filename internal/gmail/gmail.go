// Package gmail sends outreach from Harshil's own Gmail account via the
// Gmail API with a gmail.send-only scope. He authorizes once in his browser
// (`jobd auth-gmail`); the refresh token is stored locally at
// ~/.config/jobd/gmail-token.json and never leaves the machine.
package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const SendScope = "https://www.googleapis.com/auth/gmail.send"

func configPath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "jobd", name)
}

// Config builds the OAuth config from the desktop client credentials JSON
// Harshil downloads once from the GCP console.
func Config() (*oauth2.Config, error) {
	b, err := os.ReadFile(configPath("gmail-client.json"))
	if err != nil {
		return nil, fmt.Errorf("gmail client credentials missing: %w\n"+
			"Create an OAuth 2.0 Client ID (type: Desktop app) in the GCP console for\n"+
			"project jobd-agent-hs, download the JSON, and save it to %s",
			err, configPath("gmail-client.json"))
	}
	return google.ConfigFromJSON(b, SendScope)
}

func SaveToken(t *oauth2.Token) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath("gmail-token.json"), b, 0o600)
}

func loadToken() (*oauth2.Token, error) {
	b, err := os.ReadFile(configPath("gmail-token.json"))
	if err != nil {
		return nil, fmt.Errorf("not authorized yet: run `jobd auth-gmail` (%w)", err)
	}
	var t oauth2.Token
	return &t, json.Unmarshal(b, &t)
}

// APISender is the OAuth path (gmail.send scope). Only used if Harshil has
// created an OAuth client by hand; SMTP + App Password is the default.
type APISender struct {
	client *http.Client
	from   string
}

func NewAPISender(ctx context.Context, from string) (*APISender, error) {
	cfg, err := Config()
	if err != nil {
		return nil, err
	}
	tok, err := loadToken()
	if err != nil {
		return nil, err
	}
	return &APISender{client: cfg.Client(ctx, tok), from: from}, nil
}

// Send delivers one plain-text message. Returns the Gmail message id.
func (s *APISender) Send(ctx context.Context, to, subject, body string) (string, error) {
	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\n", s.from)
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n")
	msg.WriteString(body)

	payload, _ := json.Marshal(map[string]string{
		"raw": base64.URLEncoding.EncodeToString([]byte(msg.String())),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://gmail.googleapis.com/gmail/v1/users/me/messages/send",
		strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ID    string `json:"id"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gmail send failed (%d): %s", resp.StatusCode, out.Error.Message)
	}
	return out.ID, nil
}
