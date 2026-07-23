// Package jd fetches the full job-description body for a single posting —
// lazily, only for postings that survive the prefilter, because v1 proved
// the JD body is the only trustworthy source for real YOE minimums.
package jd

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

var (
	tagRe   = regexp.MustCompile(`<[^>]*>`)
	spaceRe = regexp.MustCompile(`[ \t]+`)
	nlRe    = regexp.MustCompile(`\n{3,}`)
)

func stripHTML(s string) string {
	s = html.UnescapeString(s)
	s = strings.NewReplacer("</p>", "\n", "<br>", "\n", "<br/>", "\n", "</li>", "\n", "</div>", "\n").Replace(s)
	s = tagRe.ReplaceAllString(s, " ")
	s = spaceRe.ReplaceAllString(s, " ")
	s = nlRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func getJSON(ctx context.Context, c *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Fetch returns the plain-text JD body for one posting.
func Fetch(ctx context.Context, c *http.Client, ats, token, reqID string) (string, error) {
	switch ats {
	case "greenhouse":
		var body struct {
			Content string `json:"content"`
		}
		url := fmt.Sprintf("https://boards-api.greenhouse.io/v1/boards/%s/jobs/%s", token, reqID)
		if err := getJSON(ctx, c, url, &body); err != nil {
			return "", err
		}
		return stripHTML(body.Content), nil

	case "lever":
		var body struct {
			DescriptionPlain string `json:"descriptionPlain"`
			Lists            []struct {
				Text    string `json:"text"`
				Content string `json:"content"`
			} `json:"lists"`
			AdditionalPlain string `json:"additionalPlain"`
		}
		url := fmt.Sprintf("https://api.lever.co/v0/postings/%s/%s", token, reqID)
		if err := getJSON(ctx, c, url, &body); err != nil {
			return "", err
		}
		var b strings.Builder
		b.WriteString(body.DescriptionPlain)
		for _, l := range body.Lists {
			b.WriteString("\n\n" + l.Text + "\n" + stripHTML(l.Content))
		}
		b.WriteString("\n" + body.AdditionalPlain)
		return strings.TrimSpace(b.String()), nil

	case "ashby":
		// board endpoint carries per-job descriptions; find ours
		var body struct {
			Jobs []struct {
				ID              string `json:"id"`
				DescriptionHTML string `json:"descriptionHtml"`
			} `json:"jobs"`
		}
		url := "https://api.ashbyhq.com/posting-api/job-board/" + token + "?includeCompensation=true"
		if err := getJSON(ctx, c, url, &body); err != nil {
			return "", err
		}
		for _, j := range body.Jobs {
			if j.ID == reqID {
				return stripHTML(j.DescriptionHTML), nil
			}
		}
		return "", fmt.Errorf("posting %s no longer on board (dead)", reqID)

	case "workable":
		var body struct {
			Description  string `json:"description"`
			Requirements string `json:"requirements"`
			Benefits     string `json:"benefits"`
		}
		url := fmt.Sprintf("https://apply.workable.com/api/v1/widget/accounts/%s/jobs/%s", token, reqID)
		if err := getJSON(ctx, c, url, &body); err != nil {
			return "", err
		}
		return strings.TrimSpace(stripHTML(body.Description) + "\n\nRequirements:\n" +
			stripHTML(body.Requirements) + "\n\nBenefits:\n" + stripHTML(body.Benefits)), nil

	case "smartrecruiters":
		var body struct {
			JobAd struct {
				Sections map[string]struct {
					Title string `json:"title"`
					Text  string `json:"text"`
				} `json:"sections"`
			} `json:"jobAd"`
		}
		url := fmt.Sprintf("https://api.smartrecruiters.com/v1/companies/%s/postings/%s", token, reqID)
		if err := getJSON(ctx, c, url, &body); err != nil {
			return "", err
		}
		var b strings.Builder
		for _, key := range []string{"companyDescription", "jobDescription", "qualifications", "additionalInformation"} {
			if s, ok := body.JobAd.Sections[key]; ok {
				b.WriteString(s.Title + "\n" + stripHTML(s.Text) + "\n\n")
			}
		}
		return strings.TrimSpace(b.String()), nil
	}
	return "", fmt.Errorf("unknown ats %q", ats)
}
