// Package intake is the front door for a job Harshil found himself — a link he
// was sent, a posting from a LinkedIn feed, a referral.
//
// The point is that a hand-carried job is NOT a special case downstream. It
// runs the same dedupe, the same prefilter, the same LLM gate and the same
// scoring policy the sweep runs, and lands in the same `jobs` table with the
// same `shortlisted` status. From the apply and outreach stages' point of view
// there is no difference: they see one queue.
//
// What intake adds over the sweep is only what a pasted link is missing:
//   - resolving the URL back to an (ats, token, req_id) so dedupe still works
//   - fetching the JD (or accepting text, for boards jobd cannot read)
//   - a `force` escape hatch, because Harshil choosing a job by hand is itself
//     evidence the gate does not have
package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/harshil/agent/internal/ats"
	"github.com/harshil/agent/internal/jd"
	"github.com/harshil/agent/internal/llm"
	"github.com/harshil/agent/internal/policy"
	"github.com/harshil/agent/internal/prefilter"
	"github.com/harshil/agent/internal/store"
)

// Request is one job being handed to the pipeline.
type Request struct {
	URL string `json:"url"`
	// JDText is required only when the URL is not one of the five tracked
	// boards — jobd cannot read a LinkedIn or careers-page posting itself, so
	// the caller (Claude, with a browser) supplies the text.
	JDText string `json:"jd_text"`
	// Title/Company/Location fill in what the board API would have given us.
	// Title is required for a non-board URL: the prefilter reads it.
	Title    string `json:"title"`
	Company  string `json:"company"`
	Location string `json:"location"`
	// Force queues the job even if a filter rejects it. The gate still runs and
	// its verdict is still recorded — force overrides the decision, not the
	// facts. It can never resurrect a job already applied to.
	Force bool `json:"force"`
	// Note is free text from Harshil ("referral from Ankit") kept on the row.
	Note string `json:"note"`
}

// Result is the verdict, shaped so a caller can report it without a second query.
type Result struct {
	Decision string `json:"decision"` // queued | rejected | duplicate | deferred
	Stage    string `json:"stage"`    // where the decision was made
	JobID    int64  `json:"job_id"`
	Status   string `json:"status"` // the jobs.status now on the row
	Score    int    `json:"score"`
	Note     string `json:"note"`

	ATS      string `json:"ats"`
	Company  string `json:"company"`
	ReqID    string `json:"req_id"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Location string `json:"location"`

	JDChars    int          `json:"jd_chars"`
	Gate       *llm.Verdict `json:"gate,omitempty"`
	Forced     bool         `json:"forced"`
	BoardAdded bool         `json:"board_added"` // company added to the sweep registry
}

// ErrNeedJDText means the URL is not a board jobd can read: re-post with jd_text
// (and a title). This is the one error a caller is expected to recover from.
var ErrNeedJDText = errors.New(
	"not a tracked job board (greenhouse/lever/ashby/workable/smartrecruiters): " +
		"re-submit with jd_text and title — open the posting and copy the description")

// ErrNoURL / ErrNoTitle are plain input faults.
var (
	ErrNoURL   = errors.New("url is required")
	ErrNoTitle = errors.New("title is required when the JD text is supplied by the caller")
)

// Deps are what Submit needs from the process around it.
type Deps struct {
	Store  *store.Store
	Gate   llm.Client   // may be nil: the gate is then skipped and the job deferred
	Client *http.Client // for fetching the JD from a board API
}

// minJDChars — below this the gate has nothing to read. Same threshold the
// sweep's gate stage uses.
const minJDChars = 200

// Submit runs one job through the full pipeline and records the outcome.
func Submit(ctx context.Context, d Deps, req Request) (Result, error) {
	if strings.TrimSpace(req.URL) == "" {
		return Result{}, ErrNoURL
	}
	if d.Client == nil {
		d.Client = http.DefaultClient
	}
	url := ats.CanonicalURL(req.URL)
	res := Result{URL: url, Forced: req.Force}

	// --- identity: resolve the link to the same key the sweep would record ---
	atsType, token, reqID, known := ats.Identify(url)
	if !known {
		if strings.TrimSpace(req.JDText) == "" {
			return res, ErrNeedJDText
		}
		if strings.TrimSpace(req.Title) == "" {
			return res, ErrNoTitle
		}
		atsType = "manual"
		token = ats.Slug(req.Company)
		if token == "" {
			token = ats.HostSlug(url)
		}
		sum := sha256.Sum256([]byte(url))
		reqID = hex.EncodeToString(sum[:8])
	}
	res.ATS, res.Company, res.ReqID = atsType, token, reqID

	// --- dedupe layer 1: the permanent ledger ---
	// Applied/dead/rejected jobs live here forever; a re-shared link must not
	// reopen one. Force does not override this — double-applying is the one
	// failure the whole system is built to prevent.
	if verdict, hit, err := d.Store.InLedger(url); err != nil {
		return res, err
	} else if hit {
		res.Decision, res.Stage, res.Note = "duplicate", "ledger",
			"already in the permanent ledger as "+verdict+" — not re-queued"
		return res, nil
	}
	if known {
		if verdict, hit, err := d.Store.InLedgerByFragment("/" + reqID); err != nil {
			return res, err
		} else if hit {
			res.Decision, res.Stage, res.Note = "duplicate", "ledger",
				"same job code already in the ledger as "+verdict+" — not re-queued"
			return res, nil
		}
	}

	// --- dedupe layer 2: a row that already exists ---
	existing, found, err := d.Store.JobByKey(atsType, token, reqID)
	if err != nil {
		return res, err
	}
	if found {
		res.Title, res.Location = existing.Title, existing.Location
		switch existing.Status {
		case "applied", "applying":
			res.Decision, res.Stage, res.JobID = "duplicate", "jobs", existing.ID
			res.Status, res.Note = existing.Status, "already "+existing.Status+" — never re-queued"
			return res, nil
		case "shortlisted":
			res.Decision, res.Stage, res.JobID = "duplicate", "jobs", existing.ID
			res.Status, res.Score = existing.Status, existing.Score
			res.Note = "already in the apply queue"
			return res, nil
		default:
			// new / rejected_hard / dead / deferred: re-run the pipeline on it.
			// Without force the verdict will simply come out the same way; with
			// force this is how Harshil overrides an earlier rejection.
		}
	}

	// --- the posting itself ---
	title, location, postedAt := strings.TrimSpace(req.Title), strings.TrimSpace(req.Location), ""
	jdText := strings.TrimSpace(req.JDText)
	if known {
		p, err := jd.FetchPosting(ctx, d.Client, atsType, token, reqID)
		switch {
		case err != nil && jdText == "":
			// the board says this posting is gone — record it as dead so a
			// future sweep skips it, and say so
			res.Decision, res.Stage = "rejected", "fetch"
			res.Note = "posting could not be fetched (likely pulled): " + err.Error()
			res.Title = title
			return d.finish(req, res, store.NewJob{ATS: atsType, Token: token, ReqID: reqID,
				URL: url, Title: title, Location: location, Source: "manual"}, "dead", 0, "")
		case err != nil:
			res.Note = "board fetch failed (" + err.Error() + "); used the supplied JD text"
		default:
			jdText = p.Text
			postedAt = p.PostedAt
			if p.Title != "" {
				title = p.Title
			}
			if p.Location != "" {
				location = p.Location
			}
		}
	}
	res.Title, res.Location, res.JDChars = title, location, len(jdText)
	if title == "" {
		return res, ErrNoTitle
	}

	row := store.NewJob{ATS: atsType, Token: token, ReqID: reqID, URL: url,
		Title: title, Location: location, PostedAt: postedAt, Source: "manual"}
	// Only stash the text when jobd cannot re-read the posting later; for a
	// tracked board the live fetch at apply time is the fresher source.
	if !known {
		row.JDText = jdText
	}

	// --- filter 1: the $0 prefilter ---
	if keep, note := prefilter.Check(title, location); !keep {
		if !req.Force {
			res.Decision, res.Stage, res.Note = "rejected", "prefilter", note
			return d.finish(req, res, row, "rejected_hard", 0, "")
		}
		res.Note = "forced past " + note + "; "
	}

	// --- filter 2: the LLM gate ---
	if len(jdText) < minJDChars {
		note := fmt.Sprintf("JD too short to gate (%d chars) — needs a human look", len(jdText))
		if !req.Force {
			res.Decision, res.Stage, res.Note = "deferred", "gate", note
			return d.finish(req, res, row, "deferred", 0, "")
		}
		res.Note += "forced with no gate: " + note + "; "
		res.Decision, res.Stage = "queued", "forced"
		return d.finish(req, res, row, "shortlisted", 0, "")
	}
	if d.Gate == nil {
		// Park it as 'new', not 'deferred'. Missing credentials is a property of
		// THIS machine, not of the job — someone submitting from a laptop with no
		// Google login has told us about a real posting, and the next gate run on
		// a machine that does have credentials should pick it up by itself.
		// 'deferred' would strand it: gate() only ever reads status='new'.
		note := "awaiting gate — no Gemini credentials on the machine that submitted it"
		if !req.Force {
			res.Decision, res.Stage, res.Note = "pending_gate", "gate", note
			return d.finish(req, res, row, "new", 0, "")
		}
		res.Note += "forced past ungated JD: " + note + "; "
		res.Decision, res.Stage = "queued", "forced"
		return d.finish(req, res, row, "shortlisted", 0, "")
	}
	verdict, raw, err := d.Gate.GateJD(ctx, title, jdText)
	if err != nil {
		note := "gate escalation: " + err.Error()
		if !req.Force {
			// Only schema drift is the job's own fault and needs a human. A 403
			// waiting on an IAM grant, an expired login, a quota wall — those are
			// this machine at this moment, so park the job as 'new' and let the
			// next gate run have it. Anything else strands a real posting a
			// collaborator just contributed.
			if !errors.Is(err, llm.ErrValidation) {
				res.Decision, res.Stage, res.Note = "pending_gate", "gate",
					"awaiting gate — "+err.Error()
				return d.finish(req, res, row, "new", 0, "")
			}
			res.Decision, res.Stage, res.Note = "deferred", "gate", note
			return d.finish(req, res, row, "deferred", 0, "")
		}
		res.Note += "forced past a failed gate: " + note + "; "
		res.Decision, res.Stage = "queued", "forced"
		return d.finish(req, res, row, "shortlisted", 0, "")
	}
	res.Gate = &verdict

	// --- the policy that the sweep's gate stage uses, unchanged ---
	status, note, score := policy.Apply(verdict)
	if status != "shortlisted" && req.Force {
		res.Note += "FORCED past the gate — " + note
		res.Decision, res.Stage, res.Score = "queued", "forced", score
		return d.finish(req, res, row, "shortlisted", score, raw)
	}
	res.Note += note
	res.Score = score
	if status == "shortlisted" {
		res.Decision, res.Stage = "queued", "gate"
	} else {
		res.Decision, res.Stage = "rejected", "gate"
	}
	return d.finish(req, res, row, status, score, raw)
}

// finish records the outcome and hands back the completed Result. It exists so
// the call sites can say `return d.finish(...)` — writing
// `return res, d.record(req, &res, ...)` would depend on Go's unspecified
// evaluation order between reading res and the call that fills it in.
func (d Deps) finish(req Request, res Result, row store.NewJob, status string, score int, gateJSON string) (Result, error) {
	err := d.record(req, &res, row, status, score, gateJSON)
	return res, err
}

// record writes the row and its verdict, and — for a posting on a tracked board
// jobd was not already watching — adds that company to the sweep registry, so
// one shared link puts the whole board under watch from the next cycle on.
func (d Deps) record(req Request, res *Result, row store.NewJob, status string, score int, gateJSON string) error {
	id, _, err := d.Store.UpsertJob(row)
	if err != nil {
		return err
	}
	note := "manual: " + res.Note
	if strings.TrimSpace(req.Note) != "" {
		note += " | " + strings.TrimSpace(req.Note)
	}
	if err := d.Store.SaveGate(id, status, note, gateJSON, gateYoE(res.Gate), score); err != nil {
		return err
	}
	res.JobID, res.Status, res.Note = id, status, note
	if row.ATS != "manual" {
		added, err := d.Store.EnsureCompany(row.ATS, row.Token)
		if err != nil {
			return err
		}
		res.BoardAdded = added
	}
	return nil
}

func gateYoE(v *llm.Verdict) *float64 {
	if v == nil {
		return nil
	}
	return v.YoeMin
}
