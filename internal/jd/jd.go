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
	"time"
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

// Posting is one job as the board describes it: the JD body plus the header
// fields the sweep would otherwise have supplied from the board listing.
type Posting struct {
	Title    string `json:"title"`
	Location string `json:"location"`
	PostedAt string `json:"posted_at"`
	Text     string `json:"text"`
}

// Fetch returns the plain-text JD body for one posting.
func Fetch(ctx context.Context, c *http.Client, ats, token, reqID string) (string, error) {
	p, err := FetchPosting(ctx, c, ats, token, reqID)
	return p.Text, err
}

// FetchPosting returns the JD body along with the title/location/date the board
// API carries. The intake path needs those: a link pasted by hand arrives
// without the listing row the sweep would have recorded.
func FetchPosting(ctx context.Context, c *http.Client, ats, token, reqID string) (Posting, error) {
	switch ats {
	case "greenhouse":
		var body struct {
			Title    string `json:"title"`
			Content  string `json:"content"`
			Updated  string `json:"updated_at"`
			Location struct {
				Name string `json:"name"`
			} `json:"location"`
		}
		url := fmt.Sprintf("https://boards-api.greenhouse.io/v1/boards/%s/jobs/%s", token, reqID)
		if err := getJSON(ctx, c, url, &body); err != nil {
			return Posting{}, err
		}
		return Posting{Title: body.Title, Location: body.Location.Name,
			PostedAt: body.Updated, Text: stripHTML(body.Content)}, nil

	case "lever":
		var body struct {
			Text             string `json:"text"`
			DescriptionPlain string `json:"descriptionPlain"`
			Lists            []struct {
				Text    string `json:"text"`
				Content string `json:"content"`
			} `json:"lists"`
			AdditionalPlain string `json:"additionalPlain"`
			CreatedAt       int64  `json:"createdAt"`
			Categories      struct {
				Location string `json:"location"`
			} `json:"categories"`
		}
		url := fmt.Sprintf("https://api.lever.co/v0/postings/%s/%s", token, reqID)
		if err := getJSON(ctx, c, url, &body); err != nil {
			return Posting{}, err
		}
		var b strings.Builder
		b.WriteString(body.DescriptionPlain)
		for _, l := range body.Lists {
			b.WriteString("\n\n" + l.Text + "\n" + stripHTML(l.Content))
		}
		b.WriteString("\n" + body.AdditionalPlain)
		p := Posting{Title: body.Text, Location: body.Categories.Location,
			Text: strings.TrimSpace(b.String())}
		if body.CreatedAt > 0 {
			p.PostedAt = time.UnixMilli(body.CreatedAt).UTC().Format(time.RFC3339)
		}
		return p, nil

	case "ashby":
		// board endpoint carries per-job descriptions; find ours
		var body struct {
			Jobs []struct {
				ID              string `json:"id"`
				Title           string `json:"title"`
				Location        string `json:"location"`
				PublishedAt     string `json:"publishedAt"`
				DescriptionHTML string `json:"descriptionHtml"`
			} `json:"jobs"`
		}
		url := "https://api.ashbyhq.com/posting-api/job-board/" + token + "?includeCompensation=true"
		if err := getJSON(ctx, c, url, &body); err != nil {
			return Posting{}, err
		}
		for _, j := range body.Jobs {
			if j.ID == reqID {
				return Posting{Title: j.Title, Location: j.Location,
					PostedAt: j.PublishedAt, Text: stripHTML(j.DescriptionHTML)}, nil
			}
		}
		return Posting{}, fmt.Errorf("posting %s no longer on board (dead)", reqID)

	case "workable":
		var body struct {
			Title        string `json:"title"`
			City         string `json:"city"`
			Country      string `json:"country"`
			PublishedOn  string `json:"published_on"`
			Description  string `json:"description"`
			Requirements string `json:"requirements"`
			Benefits     string `json:"benefits"`
		}
		url := fmt.Sprintf("https://apply.workable.com/api/v1/widget/accounts/%s/jobs/%s", token, reqID)
		if err := getJSON(ctx, c, url, &body); err != nil {
			return Posting{}, err
		}
		return Posting{Title: body.Title, Location: joinLoc(body.City, body.Country),
			PostedAt: body.PublishedOn,
			Text: strings.TrimSpace(stripHTML(body.Description) + "\n\nRequirements:\n" +
				stripHTML(body.Requirements) + "\n\nBenefits:\n" + stripHTML(body.Benefits))}, nil

	case "smartrecruiters":
		var body struct {
			Name         string `json:"name"`
			ReleasedDate string `json:"releasedDate"`
			Location     struct {
				City    string `json:"city"`
				Country string `json:"country"`
			} `json:"location"`
			JobAd struct {
				Sections map[string]struct {
					Title string `json:"title"`
					Text  string `json:"text"`
				} `json:"sections"`
			} `json:"jobAd"`
		}
		url := fmt.Sprintf("https://api.smartrecruiters.com/v1/companies/%s/postings/%s", token, reqID)
		if err := getJSON(ctx, c, url, &body); err != nil {
			return Posting{}, err
		}
		var b strings.Builder
		for _, key := range []string{"companyDescription", "jobDescription", "qualifications", "additionalInformation"} {
			if s, ok := body.JobAd.Sections[key]; ok {
				b.WriteString(s.Title + "\n" + stripHTML(s.Text) + "\n\n")
			}
		}
		return Posting{Title: body.Name, Location: joinLoc(body.Location.City, body.Location.Country),
			PostedAt: body.ReleasedDate, Text: strings.TrimSpace(b.String())}, nil
	}
	return Posting{}, fmt.Errorf("unknown ats %q", ats)
}

func joinLoc(city, country string) string {
	if city != "" && country != "" {
		return city + ", " + country
	}
	return city + country
}
