// Package web serves the master dashboard (what's shortlisted, what's
// pending for Harshil, what each run did) and the JSON API that Claude
// apply-sessions consume:
//
//	GET  /                      dashboard (HTML)
//	GET  /api/apply-queue       shortlisted jobs, best first
//	GET  /api/stats             pipeline counts
//	POST /api/jobs/submit       hand one job to the pipeline (link or JD text)
//	POST /api/jobs/{id}/result  {"status": "...", "note": "..."} from the apply stage
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/harshil/agent/internal/intake"
	"github.com/harshil/agent/internal/jd"
	"github.com/harshil/agent/internal/llm"
	"github.com/harshil/agent/internal/store"
)

type Server struct {
	St           *store.Store
	BankPath     string
	TemplatePath string
	ResumegenBin string
	WorkDir      string
	BaselinePDF  string

	// Gate is the LLM the intake endpoint screens submissions with. Left nil
	// it is built on first use from the environment, so submissions still get
	// gated when the daemon was started in API-only mode (-backend off) — which
	// is exactly the mode an apply session starts it in.
	Gate     llm.Client
	gateOnce sync.Once
	gateErr  error
}

func New(st *store.Store) *Server {
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	return &Server{
		St:           st,
		BankPath:     abs("bank/bank.yaml"),
		TemplatePath: abs("resume/template.tex"),
		ResumegenBin: abs("resumegen-bin"),
		WorkDir:      abs("out"),
		BaselinePDF:  abs("out/Harshil_Shukla_Resume.pdf"),
	}
}

var validResult = map[string]bool{
	"applied": true, "dead": true, "deferred": true, "blocked": true,
	"rejected_hard": true, "shortlisted": true,
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/apply-queue", s.applyQueue)
	mux.HandleFunc("POST /api/jobs/submit", s.jobSubmit)
	mux.HandleFunc("POST /submit", s.dashboardSubmit)
	mux.HandleFunc("POST /api/jobs/{id}/claim", s.jobClaim)
	mux.HandleFunc("GET /api/jobs/{id}/context", s.jobContext)
	mux.HandleFunc("POST /api/jobs/{id}/resume", s.jobResume)
	mux.HandleFunc("POST /api/jobs/{id}/result", s.jobResult)
	mux.HandleFunc("POST /api/outreach", s.outreachCreate)
	mux.HandleFunc("GET /api/outreach", s.outreachList)
	mux.HandleFunc("POST /outreach/{id}/{action}", s.outreachAction)
	return mux
}

// outreachCreate stages a drafted micro-pitch (called by the Claude session
// that researched the contact). Never sends — staging only.
func (s *Server) outreachCreate(w http.ResponseWriter, r *http.Request) {
	var o store.Outreach
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if o.Email == "" || o.Subject == "" || o.Body == "" {
		http.Error(w, "email, subject and body are required", 400)
		return
	}
	if o.Confidence == "" {
		o.Confidence = "low"
	}
	id, err := s.St.AddOutreach(o)
	if err != nil {
		writeJSONStatus(w, 409, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id, "status": "draft"})
}

func (s *Server) outreachList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "draft"
	}
	rows, err := s.St.OutreachByStatus(status, 100)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, rows)
}

// outreachAction handles the dashboard buttons: approve / skip / bounced.
func (s *Server) outreachAction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	switch r.PathValue("action") {
	case "approve":
		err = s.St.SetOutreachStatus(id, "approved", "approved by Harshil on dashboard")
	case "skip":
		err = s.St.SetOutreachStatus(id, "skipped", "skipped by Harshil on dashboard")
	case "bounced":
		err = s.St.MarkBounced(id, "marked bounced by Harshil")
	default:
		http.Error(w, "unknown action", 400)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// gateClient lazily builds the JD-gate LLM. A submission must never be queued
// ungated just because the process was started without one.
func (s *Server) gateClient() (llm.Client, error) {
	s.gateOnce.Do(func() {
		if s.Gate != nil {
			return
		}
		gem, err := llm.NewGeminiFromEnv(context.Background())
		if err != nil {
			s.gateErr = err
			return
		}
		s.Gate = gem
	})
	return s.Gate, s.gateErr
}

func (s *Server) intakeDeps() intake.Deps {
	gate, err := s.gateClient()
	if err != nil {
		fmt.Println("intake: LLM gate unavailable:", err)
	}
	return intake.Deps{Store: s.St, Gate: gate, Client: &http.Client{Timeout: 30 * time.Second}}
}

// jobSubmit is the manual intake endpoint: hand it a posting and it runs the
// same dedupe, prefilter, LLM gate and scoring policy the sweep runs, then adds
// the survivors to the apply queue. Nothing bypasses a filter except an
// explicit {"force": true}.
//
//	{"url": "...", "jd_text": "...", "title": "...", "company": "...",
//	 "location": "...", "note": "...", "force": false}
//
// 200 with a Result; 422 when the URL is not a board jobd can read and no
// jd_text was supplied (the caller should fetch the page and re-post).
func (s *Server) jobSubmit(w http.ResponseWriter, r *http.Request) {
	var req intake.Request
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "body must be a JSON object with at least {\"url\": \"...\"}: "+err.Error(), 400)
		return
	}
	res, err := intake.Submit(r.Context(), s.intakeDeps(), req)
	switch {
	case errors.Is(err, intake.ErrNeedJDText), errors.Is(err, intake.ErrNoTitle):
		writeJSONStatus(w, 422, map[string]any{"ok": false, "error": err.Error(),
			"needs": []string{"jd_text", "title"}})
	case errors.Is(err, intake.ErrNoURL):
		writeJSONStatus(w, 400, map[string]any{"ok": false, "error": err.Error()})
	case err != nil:
		writeJSONStatus(w, 500, map[string]any{"ok": false, "error": err.Error()})
	default:
		writeJSON(w, res)
	}
}

// dashboardSubmit is the same intake from the dashboard's paste-a-link box.
func (s *Server) dashboardSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	res, err := intake.Submit(r.Context(), s.intakeDeps(), intake.Request{
		URL:      r.FormValue("url"),
		JDText:   r.FormValue("jd_text"),
		Title:    r.FormValue("title"),
		Company:  r.FormValue("company"),
		Location: r.FormValue("location"),
		Note:     r.FormValue("note"),
		Force:    r.FormValue("force") != "",
	})
	msg := ""
	if err != nil {
		msg = "error: " + err.Error()
	} else {
		msg = fmt.Sprintf("%s (%s) — %s: %s", strings.ToUpper(res.Decision), res.Stage, res.Title, res.Note)
	}
	http.Redirect(w, r, "/?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// jobClaim is the double-apply guard: exactly one caller can win a job.
func (s *Server) jobClaim(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	ok, err := s.St.ClaimJob(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if !ok {
		http.Error(w, "job is not shortlisted or already claimed — skip it", 409)
		return
	}
	writeJSON(w, map[string]any{"claimed": true, "id": id})
}

// jobContext gives an apply session everything it needs in one call: the job,
// the gate facts, the full JD text, and the experience bank for tailoring.
func (s *Server) jobContext(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	d, err := s.St.JobDetail(id)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	// Manually-added postings off a board jobd cannot read carry their JD text
	// in the row — there is nothing to re-fetch, so serve what intake stored.
	var jdText string
	var jdErr error
	if d.ATS == "manual" {
		jdText, jdErr = s.St.JobJD(d.ID)
		if jdErr == nil && jdText == "" {
			jdErr = fmt.Errorf("manual posting has no stored JD text")
		}
	} else {
		jdText, jdErr = jd.Fetch(r.Context(), &http.Client{Timeout: 30 * time.Second}, d.ATS, d.Token, d.ReqID)
		if jdErr != nil {
			jdText = "" // dead postings are the apply session's call to report
		}
	}
	bank, _ := os.ReadFile(s.BankPath)
	writeJSON(w, map[string]any{
		"job":       d,
		"jd_text":   jdText,
		"jd_error":  errString(jdErr),
		"bank_yaml": string(bank),
	})
}

// jobResume renders a tailored PDF from a selection. resumegen is the single
// source of render truth — it rejects any fabricated bullet or alias.
func (s *Server) jobResume(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	sel, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	selPath := filepath.Join(s.WorkDir, fmt.Sprintf("selection-%d.json", id))
	if err := os.WriteFile(selPath, sel, 0o644); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	out, err := exec.CommandContext(r.Context(), s.ResumegenBin,
		"-bank", s.BankPath, "-template", s.TemplatePath,
		"-selection", selPath, "-out", s.WorkDir,
		"-name", "Harshil_Shukla_Resume").CombinedOutput()
	if err != nil {
		// validation failure -> caller falls back to the baseline resume
		writeJSONStatus(w, 422, map[string]any{
			"ok": false, "error": strings.TrimSpace(string(out)),
			"fallback_pdf": s.BaselinePDF,
		})
		return
	}
	writeJSON(w, map[string]any{
		"ok": true, "pdf": filepath.Join(s.WorkDir, "Harshil_Shukla_Resume.pdf"),
		"detail": strings.TrimSpace(string(out)),
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	counts, err := s.St.StatusCounts()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, counts)
}

func (s *Server) applyQueue(w http.ResponseWriter, r *http.Request) {
	rows, err := s.St.JobRows("shortlisted", 50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, rows)
}

// jobResult records the apply outcome. 'applied' also writes the ledger
// entry, so the job can never be re-discovered by a future sweep.
func (s *Server) jobResult(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	var body struct {
		Status         string `json:"status"`
		Note           string `json:"note"`
		ResumeTailored bool   `json:"resume_tailored"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validResult[body.Status] {
		http.Error(w, "body must be {status, note, resume_tailored} with a valid status", 400)
		return
	}
	if err := s.St.FinishJob(id, body.Status, body.Note, body.ResumeTailored); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id, "status": body.Status})
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, 200, v) }

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

var dashTmpl = template.Must(template.New("dash").Parse(`<!doctype html>
<meta charset="utf-8">
<meta http-equiv="refresh" content="120">
<title>jobd</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 14px/1.5 -apple-system, system-ui, sans-serif; max-width: 1080px; margin: 2rem auto; padding: 0 1rem; }
  h1 { font-size: 1.3rem; } h2 { font-size: 1.05rem; margin-top: 2rem; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: .35rem .6rem; border-bottom: 1px solid rgba(127,127,127,.25); vertical-align: top; }
  th { font-weight: 600; opacity: .7; }
  .num { text-align: right; font-variant-numeric: tabular-nums; }
  .pill { display: inline-block; padding: 0 .5rem; border-radius: 999px; background: rgba(127,127,127,.15); margin-right: .5rem; }
  .note { opacity: .65; font-size: .85em; }
  a { color: inherit; }
  form.intake { border: 1px solid rgba(127,127,127,.3); border-radius: 8px; padding: .8rem 1rem; }
  form.intake input[type=url], form.intake textarea { width: 100%; box-sizing: border-box; font: inherit; }
  form.intake .row { display: flex; gap: .5rem; margin-top: .5rem; }
  form.intake .row input { flex: 1; font: inherit; min-width: 0; }
  .msg { background: rgba(127,127,127,.15); padding: .5rem .8rem; border-radius: 6px; }
  .tag { font-size: .75em; border: 1px solid rgba(127,127,127,.4); border-radius: 4px; padding: 0 .3rem; }
</style>
<h1>jobd — pipeline dashboard</h1>
<p>
{{range $k, $v := .Counts}}<span class="pill">{{$k}}: <b>{{$v}}</b></span>{{end}}
</p>
{{if .Msg}}<p class="msg">{{.Msg}}</p>{{end}}

<h2>Found a job yourself? Put it through the filters</h2>
<form class="intake" method="post" action="/submit">
  <input type="url" name="url" placeholder="https://job-boards.greenhouse.io/acme/jobs/123 — or any posting URL" required>
  <div class="row">
    <input name="title" placeholder="title (only needed if it is not a tracked board)">
    <input name="company" placeholder="company">
    <input name="location" placeholder="location">
  </div>
  <div class="row"><input name="note" placeholder="note — e.g. referral from Ankit"></div>
  <details style="margin-top:.5rem">
    <summary class="note">JD text — paste it for LinkedIn / careers pages jobd cannot read</summary>
    <textarea name="jd_text" rows="6" placeholder="paste the job description"></textarea>
  </details>
  <div class="row" style="align-items:center">
    <button>run the filters</button>
    <label class="note" style="flex:0 0 auto"><input type="checkbox" name="force" style="flex:0"> queue it even if a filter rejects it</label>
  </div>
</form>

<h2>Shortlisted — apply queue ({{len .Shortlist}})</h2>
<table>
<tr><th class="num">score</th><th>title</th><th>company</th><th>location</th><th>note</th></tr>
{{range .Shortlist}}
<tr><td class="num">{{.Score}}</td>
<td><a href="{{.URL}}">{{.Title}}</a>{{if eq .Source "manual"}} <span class="tag">manual</span>{{end}}</td>
<td>{{.Token}}</td><td>{{.Location}}</td><td class="note">{{.Note}}</td></tr>
{{else}}<tr><td colspan="5" class="note">empty — run a sweep + gate, or paste a link above</td></tr>{{end}}
</table>

<h2>Outreach drafts — approve to queue for sending ({{len .Drafts}})</h2>
<table>
<tr><th>to</th><th>company</th><th>pitch</th><th>conf</th><th>action</th></tr>
{{range .Drafts}}
<tr>
  <td>{{.PersonName}}<br><span class="note">{{.Email}}</span></td>
  <td>{{.Company}}<br><span class="note">{{.Title}}</span></td>
  <td><b>{{.Subject}}</b><br><span class="note">{{.Body}}</span></td>
  <td>{{.Confidence}}</td>
  <td>
    <form method="post" action="/outreach/{{.ID}}/approve" style="display:inline"><button>approve</button></form>
    <form method="post" action="/outreach/{{.ID}}/skip" style="display:inline"><button>skip</button></form>
  </td>
</tr>
{{else}}<tr><td colspan="5" class="note">no drafts</td></tr>{{end}}
</table>
{{if .Approved}}<p class="note">{{len .Approved}} approved and waiting — send with
<code>./jobd-bin outreach-send -db jobd.db</code></p>{{end}}

<h2>Pending for Harshil / escalated ({{len .Deferred}})</h2>
<table>
<tr><th>title</th><th>company</th><th>note</th></tr>
{{range .Deferred}}
<tr><td><a href="{{.URL}}">{{.Title}}</a></td><td>{{.Token}}</td><td class="note">{{.Note}}</td></tr>
{{else}}<tr><td colspan="3" class="note">nothing pending</td></tr>{{end}}
</table>

<h2>Recent runs</h2>
<table>
<tr><th>started</th><th class="num">boards ok</th><th class="num">fail</th><th class="num">new</th><th class="num">gated</th><th class="num">shortlisted</th></tr>
{{range .Runs}}
<tr><td>{{.StartedAt}}</td><td class="num">{{.BoardsOK}}</td><td class="num">{{.BoardsFail}}</td>
<td class="num">{{.NewJobs}}</td><td class="num">{{.Gated}}</td><td class="num">{{.Shortlisted}}</td></tr>
{{else}}<tr><td colspan="6" class="note">no runs recorded yet</td></tr>{{end}}
</table>
`))

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	counts, err := s.St.StatusCounts()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	shortlist, err := s.St.JobRows("shortlisted", 100)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	deferred, err := s.St.JobRows("deferred", 50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	runs, err := s.St.RecentRuns(20)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	drafts, err := s.St.OutreachByStatus("draft", 50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	approved, err := s.St.OutreachByStatus("approved", 50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashTmpl.Execute(w, map[string]any{
		"Counts": counts, "Shortlist": shortlist, "Deferred": deferred, "Runs": runs,
		"Drafts": drafts, "Approved": approved, "Msg": r.URL.Query().Get("msg"),
	}); err != nil {
		fmt.Println("dashboard render:", err)
	}
}
