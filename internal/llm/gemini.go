// Package llm is the swappable LLM boundary (DESIGN.md): jobd depends only
// on the Client interface. The Vertex AI Gemini backend bills Harshil's GCP
// credits; future backends (ollama on the laptop, any OpenAI-compatible
// endpoint) implement the same interface — flipping providers is config,
// not code.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Verdict is the structured JD-gate output. Facts only — scoring policy
// lives in jobd, not in the model.
type Verdict struct {
	YoeMin        *float64 `json:"yoe_min"`         // stated minimum years; null if none stated
	YoeEvidence   string   `json:"yoe_evidence"`    // verbatim quote from the JD; "" if none
	LocationIndia *bool    `json:"location_india"`  // null if JD doesn't say
	Stack         []string `json:"stack"`           // technologies the JD asks for
	SalaryMaxLPA  *float64 `json:"salary_max_lpa"`  // null unless a band is stated in INR
}

type Client interface {
	GateJD(ctx context.Context, title, jdText string) (Verdict, string, error)
}

// Gemini calls Vertex AI generateContent with a service-account token.
type Gemini struct {
	Project string
	Model   string
	ts      oauth2.TokenSource
	http    *http.Client
}

// LoadEnvFile reads KEY=VALUE lines from ~/.config/jobd/env into the process
// environment (existing vars win).
func LoadEnvFile() {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(home + "/.config/jobd/env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func NewGeminiFromEnv(ctx context.Context) (*Gemini, error) {
	LoadEnvFile()
	project, model := os.Getenv("GCP_PROJECT"), os.Getenv("GEMINI_MODEL")
	if project == "" || model == "" {
		return nil, fmt.Errorf("GCP_PROJECT / GEMINI_MODEL not set (see ~/.config/jobd/env)")
	}
	saPath := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	b, err := os.ReadFile(saPath)
	if err != nil {
		return nil, fmt.Errorf("read service account key: %w", err)
	}
	creds, err := google.CredentialsFromJSON(ctx, b, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("parse service account key: %w", err)
	}
	return &Gemini{Project: project, Model: model, ts: creds.TokenSource,
		http: &http.Client{Timeout: 60 * time.Second}}, nil
}

const gatePrompt = `You extract facts from a job description. Respond with ONLY a JSON object, no prose:
{"yoe_min": number|null, "yoe_evidence": string, "location_india": boolean|null, "stack": string[], "salary_max_lpa": number|null}

Rules:
- yoe_min: the minimum years of experience the JD REQUIRES (not "preferred"). "2-5 years" -> 2. "1+" -> 1. null if no requirement is stated.
- yoe_evidence: the verbatim sentence fragment from the JD stating that requirement. MUST be copied exactly. "" if yoe_min is null.
- location_india: true only if the JD says the role is in India or remote-India; false if another country; null if unstated.
- stack: the main technologies/languages the JD asks for (max 12).
- salary_max_lpa: top of the stated salary band in INR lakhs/year; null if not stated in INR.

JOB TITLE: %s

JOB DESCRIPTION:
%s`

var wsRe = regexp.MustCompile(`\s+`)

func squash(s string) string { return wsRe.ReplaceAllString(strings.ToLower(s), " ") }

// GateJD extracts a Verdict, validating that yoe_evidence appears verbatim in
// the JD. One retry with the validation error appended; then the caller
// escalates (Claude session queue) per DESIGN.md.
func (g *Gemini) GateJD(ctx context.Context, title, jdText string) (Verdict, string, error) {
	if len(jdText) > 20000 {
		jdText = jdText[:20000]
	}
	prompt := fmt.Sprintf(gatePrompt, title, jdText)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			prompt += fmt.Sprintf("\n\nYour previous answer was rejected: %v. Fix it.", lastErr)
		}
		raw, err := g.generate(ctx, prompt)
		if err != nil {
			return Verdict{}, "", err // transport errors aren't fixed by re-prompting
		}
		var v Verdict
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			lastErr = fmt.Errorf("invalid JSON: %w", err)
			continue
		}
		if v.YoeMin != nil && v.YoeEvidence == "" {
			lastErr = fmt.Errorf("yoe_min set but yoe_evidence empty")
			continue
		}
		if v.YoeEvidence != "" && !strings.Contains(squash(jdText), squash(v.YoeEvidence)) {
			lastErr = fmt.Errorf("yoe_evidence %q is not a verbatim quote from the JD", v.YoeEvidence)
			continue
		}
		return v, raw, nil
	}
	return Verdict{}, "", fmt.Errorf("gate failed validation twice: %w", lastErr)
}

// generate calls Vertex with backoff on 429/5xx — rate limits are transient
// and must never surface as gate escalations.
func (g *Gemini) generate(ctx context.Context, prompt string) (string, error) {
	backoff := []time.Duration{2 * time.Second, 6 * time.Second, 15 * time.Second}
	var lastErr error
	for attempt := 0; ; attempt++ {
		out, retryable, err := g.generateOnce(ctx, prompt)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retryable || attempt >= len(backoff) {
			return "", lastErr
		}
		select {
		case <-time.After(backoff[attempt] + time.Duration(rand.Intn(1500))*time.Millisecond):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func (g *Gemini) generateOnce(ctx context.Context, prompt string) (string, bool, error) {
	body, _ := json.Marshal(map[string]any{
		"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": prompt}}}},
		"generationConfig": map[string]any{
			"temperature":      0,
			"responseMimeType": "application/json",
		},
	})
	url := fmt.Sprintf(
		"https://aiplatform.googleapis.com/v1/projects/%s/locations/global/publishers/google/models/%s:generateContent",
		g.Project, g.Model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	tok, err := g.ts.Token()
	if err != nil {
		return "", false, fmt.Errorf("token: %w", err)
	}
	tok.SetAuthHeader(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", true, err // network blips are retryable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", retryable, fmt.Errorf("vertex status %d: %s", resp.StatusCode, oneline(string(b)))
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, err
	}
	if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
		return "", false, fmt.Errorf("empty response")
	}
	return out.Candidates[0].Content.Parts[0].Text, false, nil
}

func oneline(s string) string { return wsRe.ReplaceAllString(s, " ") }
