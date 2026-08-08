package intake

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harshil/agent/internal/llm"
	"github.com/harshil/agent/internal/store"
)

// fakeGate stands in for Gemini: no network, no credentials, and it reports
// exactly the facts a test wants the policy to judge.
type fakeGate struct {
	v   llm.Verdict
	err error
}

func (f fakeGate) GateJD(context.Context, string, string) (llm.Verdict, string, error) {
	return f.v, `{"fake":true}`, f.err
}

func f64(v float64) *float64 { return &v }
func b(v bool) *bool         { return &v }

func testDeps(t *testing.T, gate llm.Client) Deps {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	return Deps{Store: st, Gate: gate}
}

// A non-board posting with the text supplied: the whole point of the intake
// path. It must be gated, scored and queued like anything the sweep found.
const jdBody = `We are hiring a Backend Engineer in Bengaluru, India. You will build
services in Go and Postgres on GCP. Requirements: 1+ years of backend experience,
strong fundamentals in distributed systems, and comfort with Kubernetes and Redis.
This role is based in our Bengaluru office with hybrid working.`

func goodGate() fakeGate {
	return fakeGate{v: llm.Verdict{
		YoeMin:        f64(1),
		YoeEvidence:   "1+ years of backend experience",
		LocationIndia: b(true),
		Stack:         []string{"Go", "Postgres", "GCP", "Kubernetes", "Redis"},
	}}
}

func TestSubmitQueuesAGatedJob(t *testing.T) {
	d := testDeps(t, goodGate())
	res, err := Submit(context.Background(), d, Request{
		URL:      "https://www.linkedin.com/jobs/view/4123456789/?trk=feed",
		Title:    "Backend Engineer",
		Company:  "Acme Corp",
		Location: "Bengaluru, India",
		JDText:   jdBody,
		Note:     "referral from Ankit",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Decision != "queued" || res.Status != "shortlisted" {
		t.Fatalf("decision=%q status=%q, want queued/shortlisted (note: %s)", res.Decision, res.Status, res.Note)
	}
	if res.ATS != "manual" || res.Company != "acme-corp" {
		t.Errorf("identity = %s:%s, want manual:acme-corp", res.ATS, res.Company)
	}
	if res.Score == 0 {
		t.Error("score is 0 — the stack match did not run")
	}
	if !strings.Contains(res.Note, "referral from Ankit") {
		t.Errorf("note lost Harshil's comment: %q", res.Note)
	}

	// it must actually be in the queue the apply stage reads
	queue, err := d.Store.JobRows("shortlisted", 10)
	if err != nil || len(queue) != 1 {
		t.Fatalf("apply queue = %d rows, %v; want 1", len(queue), err)
	}
	if queue[0].Source != "manual" {
		t.Errorf("source = %q, want manual", queue[0].Source)
	}
	// and the JD text must survive for the apply stage, which cannot re-fetch it
	if text, err := d.Store.JobJD(res.JobID); err != nil || text == "" {
		t.Errorf("stored JD text = %q, %v; want the body kept", text, err)
	}
}

func TestSubmitNeedsJDTextForUnknownBoards(t *testing.T) {
	d := testDeps(t, goodGate())
	_, err := Submit(context.Background(), d, Request{URL: "https://careers.acme.com/jobs/42"})
	if !errors.Is(err, ErrNeedJDText) {
		t.Fatalf("err = %v, want ErrNeedJDText", err)
	}
	if _, err := Submit(context.Background(), d, Request{
		URL: "https://careers.acme.com/jobs/42", JDText: jdBody,
	}); !errors.Is(err, ErrNoTitle) {
		t.Fatalf("err = %v, want ErrNoTitle", err)
	}
}

func TestSubmitRejectsAtThePrefilter(t *testing.T) {
	d := testDeps(t, goodGate())
	req := Request{
		URL: "https://careers.acme.com/jobs/99", Title: "Senior Staff iOS Engineer",
		Company: "Acme", Location: "Bengaluru, India", JDText: jdBody,
	}
	res, err := Submit(context.Background(), d, req)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Decision != "rejected" || res.Stage != "prefilter" {
		t.Fatalf("decision=%q stage=%q, want rejected/prefilter", res.Decision, res.Stage)
	}
	if q, _ := d.Store.JobRows("shortlisted", 10); len(q) != 0 {
		t.Fatalf("a prefilter-rejected job reached the apply queue")
	}

	// force is the override: same job, queued anyway, and the reason is on record
	req.Force = true
	res, err = Submit(context.Background(), d, req)
	if err != nil {
		t.Fatalf("forced submit: %v", err)
	}
	if res.Decision != "queued" || res.Status != "shortlisted" {
		t.Fatalf("forced: decision=%q status=%q, want queued/shortlisted", res.Decision, res.Status)
	}
	if !strings.Contains(res.Note, "forced past") {
		t.Errorf("forced note does not say what was overridden: %q", res.Note)
	}
}

func TestSubmitRejectsOnYoEAndForceOverrides(t *testing.T) {
	gate := fakeGate{v: llm.Verdict{
		YoeMin: f64(5), YoeEvidence: "5+ years", LocationIndia: b(true), Stack: []string{"Go"},
	}}
	d := testDeps(t, gate)
	req := Request{URL: "https://careers.acme.com/jobs/7", Title: "Backend Engineer",
		Company: "Acme", Location: "Remote", JDText: jdBody}

	res, err := Submit(context.Background(), d, req)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Decision != "rejected" || res.Stage != "gate" || res.Status != "rejected_hard" {
		t.Fatalf("decision=%q stage=%q status=%q, want rejected/gate/rejected_hard", res.Decision, res.Stage, res.Status)
	}
	if res.Gate == nil || res.Gate.YoeMin == nil || *res.Gate.YoeMin != 5 {
		t.Error("the gate facts were not returned to the caller")
	}

	req.Force = true
	res, err = Submit(context.Background(), d, req)
	if err != nil {
		t.Fatalf("forced submit: %v", err)
	}
	if res.Status != "shortlisted" || !strings.Contains(res.Note, "FORCED") {
		t.Fatalf("force did not override the gate: status=%q note=%q", res.Status, res.Note)
	}
}

func TestSubmitNeverReopensAnAppliedJob(t *testing.T) {
	d := testDeps(t, goodGate())
	req := Request{URL: "https://careers.acme.com/jobs/1", Title: "Backend Engineer",
		Company: "Acme", Location: "Bengaluru, India", JDText: jdBody}
	res, err := Submit(context.Background(), d, req)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := d.Store.ClaimJob(res.JobID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := d.Store.FinishJob(res.JobID, "applied", "applied by hand", true); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// re-sharing the same link — even with force — must not queue it again
	req.Force = true
	again, err := Submit(context.Background(), d, req)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if again.Decision != "duplicate" {
		t.Fatalf("decision=%q, want duplicate (note: %s)", again.Decision, again.Note)
	}
	if q, _ := d.Store.JobRows("shortlisted", 10); len(q) != 0 {
		t.Fatal("an already-applied job was put back in the queue")
	}
}

func TestSubmitDedupesAQueuedJob(t *testing.T) {
	d := testDeps(t, goodGate())
	req := Request{URL: "https://careers.acme.com/jobs/2?utm_source=x", Title: "Backend Engineer",
		Company: "Acme", Location: "Bengaluru, India", JDText: jdBody}
	first, err := Submit(context.Background(), d, req)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// the same posting shared again, with different tracking junk on the URL
	req.URL = "https://careers.acme.com/jobs/2?trk=linkedin_feed"
	second, err := Submit(context.Background(), d, req)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if second.Decision != "duplicate" || second.JobID != first.JobID {
		t.Fatalf("second submit = %q job %d, want duplicate of job %d",
			second.Decision, second.JobID, first.JobID)
	}
}

func TestSubmitDefersWhenTheGateIsUnavailable(t *testing.T) {
	d := testDeps(t, nil) // no LLM configured
	res, err := Submit(context.Background(), d, Request{
		URL: "https://careers.acme.com/jobs/3", Title: "Backend Engineer",
		Company: "Acme", Location: "Bengaluru, India", JDText: jdBody,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Decision != "deferred" || res.Status != "deferred" {
		t.Fatalf("decision=%q status=%q, want deferred — an ungated job must never queue itself",
			res.Decision, res.Status)
	}
}
