// Package web serves the master dashboard (what's shortlisted, what's
// pending for Harshil, what each run did) and the JSON API that Claude
// apply-sessions consume:
//
//	GET  /                      dashboard (HTML)
//	GET  /api/apply-queue       shortlisted jobs, best first
//	GET  /api/stats             pipeline counts
//	POST /api/jobs/{id}/result  {"status": "...", "note": "..."} from the apply stage
package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/harshil/agent/internal/jd"
	"github.com/harshil/agent/internal/store"
)

type Server struct {
	St           *store.Store
	BankPath     string
	TemplatePath string
	ResumegenBin string
	WorkDir      string
	BaselinePDF  string
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
	jdText, jdErr := jd.Fetch(r.Context(), &http.Client{Timeout: 30 * time.Second}, d.ATS, d.Token, d.ReqID)
	if jdErr != nil {
		jdText = "" // dead postings are the apply session's call to report
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
</style>
<h1>jobd — pipeline dashboard</h1>
<p>
{{range $k, $v := .Counts}}<span class="pill">{{$k}}: <b>{{$v}}</b></span>{{end}}
</p>

<h2>Shortlisted — apply queue ({{len .Shortlist}})</h2>
<table>
<tr><th class="num">score</th><th>title</th><th>company</th><th>location</th><th>note</th></tr>
{{range .Shortlist}}
<tr><td class="num">{{.Score}}</td><td><a href="{{.URL}}">{{.Title}}</a></td>
<td>{{.Token}}</td><td>{{.Location}}</td><td class="note">{{.Note}}</td></tr>
{{else}}<tr><td colspan="5" class="note">empty — run a sweep + gate</td></tr>{{end}}
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
		"Drafts": drafts, "Approved": approved,
	}); err != nil {
		fmt.Println("dashboard render:", err)
	}
}
