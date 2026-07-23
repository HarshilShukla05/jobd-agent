// Package ats fetches public job-board JSON APIs for the five tracked ATS
// types. Pure HTTP — no LLM, no scraping of HTML. Includes the anti-bot
// hardening from DESIGN.md: browser UA, conditional requests via ETag, and
// the SmartRecruiters Accept-header fix learned in v1.
package ats

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

type Posting struct {
	ReqID    string
	URL      string
	Title    string
	Location string
	PostedAt string // RFC3339 when the board provides it, else ""
}

type Result struct {
	Postings    []Posting
	NotModified bool
	ETag        string
}

var Types = []string{"greenhouse", "lever", "ashby", "workable", "smartrecruiters"}

// Fetch sweeps one board. etag is the value cached from the previous sweep
// ("" for none); on a 304 the result has NotModified set and no postings.
func Fetch(ctx context.Context, client *http.Client, atsType, token, etag string) (Result, error) {
	switch atsType {
	case "greenhouse":
		return fetchGreenhouse(ctx, client, token, etag)
	case "lever":
		return fetchLever(ctx, client, token, etag)
	case "ashby":
		return fetchAshby(ctx, client, token, etag)
	case "workable":
		return fetchWorkable(ctx, client, token, etag)
	case "smartrecruiters":
		return fetchSmartRecruiters(ctx, client, token, etag)
	}
	return Result{}, fmt.Errorf("unknown ats type %q", atsType)
}

func get(ctx context.Context, client *http.Client, url, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	return client.Do(req)
}

// decode drains and parses a response, handling 304/non-200 uniformly.
func decode(resp *http.Response, v any) (notModified bool, newETag string, err error) {
	defer resp.Body.Close()
	newETag = resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified {
		return true, newETag, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, "", fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return false, newETag, json.NewDecoder(resp.Body).Decode(v)
}

func fetchGreenhouse(ctx context.Context, c *http.Client, token, etag string) (Result, error) {
	resp, err := get(ctx, c, "https://boards-api.greenhouse.io/v1/boards/"+token+"/jobs", etag)
	if err != nil {
		return Result{}, err
	}
	var body struct {
		Jobs []struct {
			ID          int64  `json:"id"`
			AbsoluteURL string `json:"absolute_url"`
			Title       string `json:"title"`
			UpdatedAt   string `json:"updated_at"`
			Location    struct {
				Name string `json:"name"`
			} `json:"location"`
		} `json:"jobs"`
	}
	nm, et, err := decode(resp, &body)
	if nm || err != nil {
		return Result{NotModified: nm, ETag: et}, err
	}
	r := Result{ETag: et}
	for _, j := range body.Jobs {
		r.Postings = append(r.Postings, Posting{
			ReqID: fmt.Sprint(j.ID), URL: j.AbsoluteURL, Title: j.Title,
			Location: j.Location.Name, PostedAt: j.UpdatedAt,
		})
	}
	return r, nil
}

func fetchLever(ctx context.Context, c *http.Client, token, etag string) (Result, error) {
	resp, err := get(ctx, c, "https://api.lever.co/v0/postings/"+token+"?mode=json", etag)
	if err != nil {
		return Result{}, err
	}
	var body []struct {
		ID         string `json:"id"`
		HostedURL  string `json:"hostedUrl"`
		Text       string `json:"text"`
		CreatedAt  int64  `json:"createdAt"`
		Categories struct {
			Location string `json:"location"`
		} `json:"categories"`
	}
	nm, et, err := decode(resp, &body)
	if nm || err != nil {
		return Result{NotModified: nm, ETag: et}, err
	}
	r := Result{ETag: et}
	for _, j := range body {
		r.Postings = append(r.Postings, Posting{
			ReqID: j.ID, URL: j.HostedURL, Title: j.Text, Location: j.Categories.Location,
			PostedAt: time.UnixMilli(j.CreatedAt).UTC().Format(time.RFC3339),
		})
	}
	return r, nil
}

func fetchAshby(ctx context.Context, c *http.Client, token, etag string) (Result, error) {
	resp, err := get(ctx, c, "https://api.ashbyhq.com/posting-api/job-board/"+token, etag)
	if err != nil {
		return Result{}, err
	}
	var body struct {
		Jobs []struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Location    string `json:"location"`
			JobURL      string `json:"jobUrl"`
			PublishedAt string `json:"publishedAt"`
		} `json:"jobs"`
	}
	nm, et, err := decode(resp, &body)
	if nm || err != nil {
		return Result{NotModified: nm, ETag: et}, err
	}
	r := Result{ETag: et}
	for _, j := range body.Jobs {
		r.Postings = append(r.Postings, Posting{
			ReqID: j.ID, URL: j.JobURL, Title: j.Title, Location: j.Location, PostedAt: j.PublishedAt,
		})
	}
	return r, nil
}

func fetchWorkable(ctx context.Context, c *http.Client, token, etag string) (Result, error) {
	resp, err := get(ctx, c, "https://apply.workable.com/api/v1/widget/accounts/"+token, etag)
	if err != nil {
		return Result{}, err
	}
	var body struct {
		Jobs []struct {
			Title       string `json:"title"`
			Shortcode   string `json:"shortcode"`
			City        string `json:"city"`
			Country     string `json:"country"`
			URL         string `json:"url"`
			PublishedOn string `json:"published_on"`
		} `json:"jobs"`
	}
	nm, et, err := decode(resp, &body)
	if nm || err != nil {
		return Result{NotModified: nm, ETag: et}, err
	}
	r := Result{ETag: et}
	for _, j := range body.Jobs {
		loc := j.City
		if j.Country != "" {
			if loc != "" {
				loc += ", "
			}
			loc += j.Country
		}
		// dedupe by shortcode: v1 learned aggregators re-serve the same job
		// code under different URL paths
		r.Postings = append(r.Postings, Posting{
			ReqID: j.Shortcode, URL: j.URL, Title: j.Title, Location: loc, PostedAt: j.PublishedOn,
		})
	}
	return r, nil
}

func fetchSmartRecruiters(ctx context.Context, c *http.Client, token, etag string) (Result, error) {
	// needs explicit Accept + pagination params or it silently returns empty (v1 finding)
	url := fmt.Sprintf("https://api.smartrecruiters.com/v1/companies/%s/postings?limit=100", token)
	resp, err := get(ctx, c, url, etag)
	if err != nil {
		return Result{}, err
	}
	var body struct {
		Content []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			ReleasedDate string `json:"releasedDate"`
			Location    struct {
				City    string `json:"city"`
				Country string `json:"country"`
			} `json:"location"`
		} `json:"content"`
	}
	nm, et, err := decode(resp, &body)
	if nm || err != nil {
		return Result{NotModified: nm, ETag: et}, err
	}
	r := Result{ETag: et}
	for _, j := range body.Content {
		loc := j.Location.City
		if j.Location.Country != "" {
			if loc != "" {
				loc += ", "
			}
			loc += j.Location.Country
		}
		r.Postings = append(r.Postings, Posting{
			ReqID: j.ID, Title: j.Name, Location: loc, PostedAt: j.ReleasedDate,
			URL: "https://jobs.smartrecruiters.com/" + token + "/" + j.ID,
		})
	}
	return r, nil
}
