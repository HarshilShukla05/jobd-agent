package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harshil/agent/internal/llm"
	"github.com/harshil/agent/internal/store"
)

type stubGate struct{}

func (stubGate) GateJD(context.Context, string, string) (llm.Verdict, string, error) {
	yoe := 1.0
	india := true
	return llm.Verdict{YoeMin: &yoe, YoeEvidence: "1+ years", LocationIndia: &india,
		Stack: []string{"Go", "Postgres"}}, "{}", nil
}

func testServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	s := New(st)
	s.Gate = stubGate{}
	return s, s.Routes()
}

const jdBody = `Backend Engineer in Bengaluru, India building services in Go and Postgres
on GCP. Requirements: 1+ years of backend experience, solid fundamentals in distributed
systems, and working knowledge of Kubernetes, Redis and gRPC. Hybrid from our Bengaluru
office. You will own services end to end, from design through production support.`

func TestSubmitEndpointQueuesAJob(t *testing.T) {
	_, h := testServer(t)
	body := map[string]any{
		"url": "https://www.linkedin.com/jobs/view/4123456789/", "title": "Backend Engineer",
		"company": "Acme", "location": "Bengaluru, India", "jd_text": jdBody,
	}
	buf, _ := json.Marshal(body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/jobs/submit", strings.NewReader(string(buf))))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var res struct {
		Decision string `json:"decision"`
		JobID    int64  `json:"job_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Decision != "queued" {
		t.Fatalf("decision = %q, want queued: %s", res.Decision, rr.Body)
	}

	// it must show up in the queue the apply stage polls
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/apply-queue", nil))
	if !strings.Contains(rr.Body.String(), `"title":"Backend Engineer"`) {
		t.Fatalf("job did not reach /api/apply-queue: %s", rr.Body)
	}

	// and /context must serve the stored JD rather than reporting a dead posting
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/jobs/1/context", nil))
	var ctxResp struct {
		JDText string `json:"jd_text"`
		JDErr  string `json:"jd_error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ctxResp); err != nil {
		t.Fatalf("decode context: %v", err)
	}
	if ctxResp.JDErr != "" || !strings.Contains(ctxResp.JDText, "Bengaluru") {
		t.Fatalf("context jd_error=%q jd_text=%.60q — the apply stage would call this dead",
			ctxResp.JDErr, ctxResp.JDText)
	}
}

func TestSubmitEndpointAsksForJDText(t *testing.T) {
	_, h := testServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/jobs/submit",
		strings.NewReader(`{"url":"https://careers.acme.com/jobs/1"}`)))
	if rr.Code != 422 {
		t.Fatalf("status %d, want 422: %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "jd_text") {
		t.Fatalf("422 does not say what is missing: %s", rr.Body)
	}
}

// The submit route sits next to /api/jobs/{id}/claim; make sure neither shadows
// the other.
func TestSubmitRouteDoesNotShadowClaim(t *testing.T) {
	_, h := testServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/jobs/12/claim", nil))
	if rr.Code != 409 { // no such job to claim, but it reached the claim handler
		t.Fatalf("claim status %d, want 409: %s", rr.Code, rr.Body)
	}
}

func TestDashboardRenders(t *testing.T) {
	_, h := testServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/?msg=hello", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	for _, want := range []string{`action="/submit"`, `name="jd_text"`, `name="force"`, "hello"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("dashboard is missing %q", want)
		}
	}
}
