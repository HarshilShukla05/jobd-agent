// Package store owns jobd's SQLite state: the company registry (ATS board
// tokens), every job ever seen, and the applied/rejected ledger imported from
// the v1 pipeline. This replaces the self-editing prompt file that v1 used as
// its database.
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS companies (
    id            INTEGER PRIMARY KEY,
    ats           TEXT NOT NULL,
    token         TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'active',  -- active | unstable | dead
    fail_count    INTEGER NOT NULL DEFAULT 0,
    etag          TEXT NOT NULL DEFAULT '',
    last_swept_at TEXT,
    UNIQUE (ats, token)
);
CREATE TABLE IF NOT EXISTS jobs (
    id            INTEGER PRIMARY KEY,
    ats           TEXT NOT NULL,
    token         TEXT NOT NULL,
    req_id        TEXT NOT NULL,
    url           TEXT NOT NULL,
    title         TEXT NOT NULL DEFAULT '',
    location      TEXT NOT NULL DEFAULT '',
    posted_at     TEXT,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    -- new -> gated -> shortlisted -> applied | rejected_hard | dead | deferred
    status        TEXT NOT NULL DEFAULT 'new',
    note          TEXT NOT NULL DEFAULT '',
    UNIQUE (ats, token, req_id)
);
CREATE TABLE IF NOT EXISTS ledger (
    url      TEXT PRIMARY KEY,   -- v1 seen-jobs ledger: authoritative dedupe
    verdict  TEXT NOT NULL,
    noted_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs (status);
`

// additive migrations; "duplicate column" errors are expected and ignored
var migrations = []string{
	`ALTER TABLE jobs ADD COLUMN yoe_min REAL`,
	`ALTER TABLE jobs ADD COLUMN gate_json TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN score INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN claimed_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN applied_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN resume_tailored INTEGER NOT NULL DEFAULT 0`,
	// Outreach: one row per drafted micro-pitch. Confidence drives the send
	// policy (high = batch-approvable, lower = individual approval), and
	// bounces blacklist the domain pattern for every future run.
	`CREATE TABLE IF NOT EXISTS outreach (
	    id          INTEGER PRIMARY KEY,
	    job_id      INTEGER NOT NULL,
	    company     TEXT NOT NULL,
	    person_name TEXT NOT NULL DEFAULT '',
	    title       TEXT NOT NULL DEFAULT '',
	    email       TEXT NOT NULL,
	    confidence  TEXT NOT NULL DEFAULT 'low',
	    subject     TEXT NOT NULL,
	    body        TEXT NOT NULL,
	    -- draft -> approved -> sent | bounced | skipped
	    status      TEXT NOT NULL DEFAULT 'draft',
	    note        TEXT NOT NULL DEFAULT '',
	    created_at  TEXT NOT NULL,
	    sent_at     TEXT NOT NULL DEFAULT '',
	    UNIQUE (email, job_id)
	)`,
	`CREATE TABLE IF NOT EXISTS email_patterns (
	    domain     TEXT PRIMARY KEY,
	    pattern    TEXT NOT NULL,      -- first / first.last / firstlast / flast
	    verified   INTEGER NOT NULL DEFAULT 0,
	    bounced    INTEGER NOT NULL DEFAULT 0,
	    updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS runs (
	    id          INTEGER PRIMARY KEY,
	    started_at  TEXT NOT NULL,
	    finished_at TEXT NOT NULL,
	    boards_ok   INTEGER NOT NULL,
	    boards_fail INTEGER NOT NULL,
	    new_jobs    INTEGER NOT NULL,
	    gated       INTEGER NOT NULL,
	    shortlisted INTEGER NOT NULL
	)`,
}

type Store struct{ DB *sql.DB }

type Company struct {
	ID        int64
	ATS       string
	Token     string
	Status    string
	FailCount int
	ETag      string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	return &Store{DB: db}, nil
}

type Job struct {
	ID       int64
	ATS      string
	Token    string
	ReqID    string
	URL      string
	Title    string
	Location string
}

func (s *Store) JobsByStatus(status string, limit int) ([]Job, error) {
	rows, err := s.DB.Query(
		`SELECT id, ats, token, req_id, url, title, location FROM jobs
		 WHERE status = ? ORDER BY first_seen_at DESC LIMIT ?`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.ATS, &j.Token, &j.ReqID, &j.URL, &j.Title, &j.Location); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) SetJobStatus(id int64, status, note string) error {
	_, err := s.DB.Exec(`UPDATE jobs SET status = ?, note = ? WHERE id = ?`, status, note, id)
	return err
}

func (s *Store) SaveGate(id int64, status, note, gateJSON string, yoeMin *float64, score int) error {
	_, err := s.DB.Exec(
		`UPDATE jobs SET status = ?, note = ?, gate_json = ?, yoe_min = ?, score = ? WHERE id = ?`,
		status, note, gateJSON, yoeMin, score, id)
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (s *Store) AddCompany(ats, token, status string) error {
	_, err := s.DB.Exec(
		`INSERT INTO companies (ats, token, status) VALUES (?, ?, ?)
		 ON CONFLICT (ats, token) DO NOTHING`, ats, token, status)
	return err
}

func (s *Store) AddLedger(url, verdict string) error {
	_, err := s.DB.Exec(
		`INSERT INTO ledger (url, verdict, noted_at) VALUES (?, ?, ?)
		 ON CONFLICT (url) DO UPDATE SET verdict = excluded.verdict`, url, verdict, now())
	return err
}

func (s *Store) ActiveCompanies(limit int) ([]Company, error) {
	rows, err := s.DB.Query(
		`SELECT id, ats, token, status, fail_count, etag FROM companies
		 WHERE status != 'dead'
		 ORDER BY last_swept_at IS NOT NULL, last_swept_at ASC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Company
	for rows.Next() {
		var c Company
		if err := rows.Scan(&c.ID, &c.ATS, &c.Token, &c.Status, &c.FailCount, &c.ETag); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpsertJob records a sighting of a posting; reports whether it is new.
func (s *Store) UpsertJob(ats, token, reqID, url, title, location, postedAt string) (bool, error) {
	var exists bool
	err := s.DB.QueryRow(
		`SELECT 1 FROM jobs WHERE ats = ? AND token = ? AND req_id = ?`,
		ats, token, reqID).Scan(&exists)
	if err == sql.ErrNoRows {
		_, err = s.DB.Exec(
			`INSERT INTO jobs (ats, token, req_id, url, title, location, posted_at, first_seen_at, last_seen_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ats, token, reqID, url, title, location, postedAt, now(), now())
		return err == nil, err
	}
	if err != nil {
		return false, err
	}
	_, err = s.DB.Exec(
		`UPDATE jobs SET last_seen_at = ? WHERE ats = ? AND token = ? AND req_id = ?`,
		now(), ats, token, reqID)
	return false, err
}

func (s *Store) SweepResult(c Company, etag string, ok bool) error {
	if ok {
		_, err := s.DB.Exec(
			`UPDATE companies SET etag = ?, fail_count = 0, status = 'active', last_swept_at = ? WHERE id = ?`,
			etag, now(), c.ID)
		return err
	}
	// second consecutive failure kills the token, mirroring v1 registry rules
	_, err := s.DB.Exec(
		`UPDATE companies SET fail_count = fail_count + 1,
		    status = CASE WHEN fail_count + 1 >= 2 THEN 'dead' ELSE 'unstable' END,
		    last_swept_at = ? WHERE id = ?`, now(), c.ID)
	return err
}

func (s *Store) InLedger(url string) (string, bool, error) {
	var verdict string
	err := s.DB.QueryRow(`SELECT verdict FROM ledger WHERE url = ?`, url).Scan(&verdict)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return verdict, err == nil, err
}

// InLedgerByFragment matches ledger URLs containing a fragment — Workable
// aggregators re-serve the same job code under different URL paths, so
// dedupe must be by code, not exact URL (v1 finding).
func (s *Store) InLedgerByFragment(fragment string) (string, bool, error) {
	var verdict string
	err := s.DB.QueryRow(`SELECT verdict FROM ledger WHERE url LIKE ? LIMIT 1`,
		"%"+fragment+"%").Scan(&verdict)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return verdict, err == nil, err
}

// ClaimJob atomically moves a shortlisted job to 'applying' so no second
// worker (or later run) can touch it. Returns false if it was already
// claimed or is no longer shortlisted — this is the double-apply guard.
func (s *Store) ClaimJob(id int64) (bool, error) {
	res, err := s.DB.Exec(
		`UPDATE jobs SET status = 'applying', claimed_at = ?
		 WHERE id = ? AND status = 'shortlisted'`, now(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseStaleClaims returns jobs stuck in 'applying' (crashed session) to
// the queue after the timeout.
func (s *Store) ReleaseStaleClaims(olderThan time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	res, err := s.DB.Exec(
		`UPDATE jobs SET status = 'shortlisted', note = note || ' [claim expired]'
		 WHERE status = 'applying' AND claimed_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// FinishJob records the apply outcome and, for a real application, writes
// the ledger entry that permanently prevents re-discovery.
func (s *Store) FinishJob(id int64, status, note string, resumeTailored bool) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, note = ?, resume_tailored = ?, applied_at = ? WHERE id = ?`,
		status, note, resumeTailored, now(), id); err != nil {
		return err
	}
	if status == "applied" || status == "rejected_hard" || status == "dead" {
		var url string
		if err := tx.QueryRow(`SELECT url FROM jobs WHERE id = ?`, id).Scan(&url); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO ledger (url, verdict, noted_at) VALUES (?, ?, ?)
			 ON CONFLICT (url) DO UPDATE SET verdict = excluded.verdict`,
			url, status, now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// JobDetail is everything an apply session needs about one job.
type JobDetail struct {
	JobRow
	ReqID    string `json:"req_id"`
	GateJSON string `json:"gate_json"`
	Status   string `json:"status"`
}

func (s *Store) JobDetail(id int64) (JobDetail, error) {
	var d JobDetail
	err := s.DB.QueryRow(
		`SELECT id, score, title, token, ats, req_id, location, url, note, yoe_min,
		        first_seen_at, gate_json, status
		 FROM jobs WHERE id = ?`, id).
		Scan(&d.ID, &d.Score, &d.Title, &d.Token, &d.ATS, &d.ReqID, &d.Location, &d.URL,
			&d.Note, &d.YoeMin, &d.FirstSeen, &d.GateJSON, &d.Status)
	return d, err
}

// JobRow is the dashboard/API projection of a job.
type JobRow struct {
	ID        int64   `json:"id"`
	Score     int     `json:"score"`
	Title     string  `json:"title"`
	Token     string  `json:"company"`
	ATS       string  `json:"ats"`
	Location  string  `json:"location"`
	URL       string  `json:"url"`
	Note      string  `json:"note"`
	YoeMin    *float64 `json:"yoe_min"`
	FirstSeen string  `json:"first_seen_at"`
}

func (s *Store) JobRows(status string, limit int) ([]JobRow, error) {
	rows, err := s.DB.Query(
		`SELECT id, score, title, token, ats, location, url, note, yoe_min, first_seen_at
		 FROM jobs WHERE status = ? ORDER BY score DESC, first_seen_at DESC LIMIT ?`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var r JobRow
		if err := rows.Scan(&r.ID, &r.Score, &r.Title, &r.Token, &r.ATS, &r.Location,
			&r.URL, &r.Note, &r.YoeMin, &r.FirstSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) StatusCounts() (map[string]int, error) {
	rows, err := s.DB.Query(`SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

type Run struct {
	StartedAt, FinishedAt                          string
	BoardsOK, BoardsFail, NewJobs, Gated, Shortlisted int
}

func (s *Store) InsertRun(r Run) error {
	_, err := s.DB.Exec(
		`INSERT INTO runs (started_at, finished_at, boards_ok, boards_fail, new_jobs, gated, shortlisted)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.StartedAt, r.FinishedAt, r.BoardsOK, r.BoardsFail, r.NewJobs, r.Gated, r.Shortlisted)
	return err
}

func (s *Store) RecentRuns(limit int) ([]Run, error) {
	rows, err := s.DB.Query(
		`SELECT started_at, finished_at, boards_ok, boards_fail, new_jobs, gated, shortlisted
		 FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.StartedAt, &r.FinishedAt, &r.BoardsOK, &r.BoardsFail,
			&r.NewJobs, &r.Gated, &r.Shortlisted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Outreach struct {
	ID         int64  `json:"id"`
	JobID      int64  `json:"job_id"`
	Company    string `json:"company"`
	PersonName string `json:"person_name"`
	Title      string `json:"title"`
	Email      string `json:"email"`
	Confidence string `json:"confidence"`
	Subject    string `json:"subject"`
	Body       string `json:"body"`
	Status     string `json:"status"`
	Note       string `json:"note"`
	CreatedAt  string `json:"created_at"`
}

func (s *Store) AddOutreach(o Outreach) (int64, error) {
	// never draft to a domain whose guessed pattern has already bounced
	domain := o.Email
	if i := strings.LastIndex(domain, "@"); i >= 0 {
		domain = domain[i+1:]
	}
	var bounced int
	err := s.DB.QueryRow(`SELECT bounced FROM email_patterns WHERE domain = ?`, domain).Scan(&bounced)
	if err == nil && bounced > 0 {
		return 0, fmt.Errorf("domain %s is blacklisted after a prior bounce", domain)
	}
	res, err := s.DB.Exec(
		`INSERT INTO outreach (job_id, company, person_name, title, email, confidence,
		                       subject, body, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'draft', ?)
		 ON CONFLICT (email, job_id) DO NOTHING`,
		o.JobID, o.Company, o.PersonName, o.Title, o.Email, o.Confidence,
		o.Subject, o.Body, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) OutreachByStatus(status string, limit int) ([]Outreach, error) {
	rows, err := s.DB.Query(
		`SELECT id, job_id, company, person_name, title, email, confidence,
		        subject, body, status, note, created_at
		 FROM outreach WHERE status = ? ORDER BY created_at DESC LIMIT ?`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outreach
	for rows.Next() {
		var o Outreach
		if err := rows.Scan(&o.ID, &o.JobID, &o.Company, &o.PersonName, &o.Title, &o.Email,
			&o.Confidence, &o.Subject, &o.Body, &o.Status, &o.Note, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) SetOutreachStatus(id int64, status, note string) error {
	sentAt := ""
	if status == "sent" {
		sentAt = now()
	}
	_, err := s.DB.Exec(
		`UPDATE outreach SET status = ?, note = ?, sent_at = ? WHERE id = ?`,
		status, note, sentAt, id)
	return err
}

// MarkBounced blacklists the domain so no future run drafts to it again.
func (s *Store) MarkBounced(id int64, reason string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var email string
	if err := tx.QueryRow(`SELECT email FROM outreach WHERE id = ?`, id).Scan(&email); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE outreach SET status = 'bounced', note = ? WHERE id = ?`, reason, id); err != nil {
		return err
	}
	domain := email
	if i := strings.LastIndex(domain, "@"); i >= 0 {
		domain = domain[i+1:]
	}
	if _, err := tx.Exec(
		`INSERT INTO email_patterns (domain, pattern, bounced, updated_at) VALUES (?, '', 1, ?)
		 ON CONFLICT (domain) DO UPDATE SET bounced = bounced + 1, updated_at = excluded.updated_at`,
		domain, now()); err != nil {
		return err
	}
	return tx.Commit()
}

// SendStats powers the circuit breaker: pause all sending if the trailing
// bounce rate is too high, and enforce the daily volume cap.
func (s *Store) SendStats() (sentToday, bouncedRecent, sentRecent int, err error) {
	today := time.Now().UTC().Format("2006-01-02")
	if err = s.DB.QueryRow(
		`SELECT COUNT(*) FROM outreach WHERE status = 'sent' AND sent_at LIKE ?`,
		today+"%").Scan(&sentToday); err != nil {
		return
	}
	err = s.DB.QueryRow(
		`SELECT
		   COALESCE(SUM(CASE WHEN status = 'bounced' THEN 1 ELSE 0 END), 0),
		   COALESCE(COUNT(*), 0)
		 FROM (SELECT status FROM outreach WHERE status IN ('sent','bounced')
		       ORDER BY id DESC LIMIT 50)`).Scan(&bouncedRecent, &sentRecent)
	return
}

func (s *Store) Counts() (companies, jobs, ledger int, err error) {
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM companies WHERE status != 'dead'`).Scan(&companies); err != nil {
		return
	}
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobs); err != nil {
		return
	}
	err = s.DB.QueryRow(`SELECT COUNT(*) FROM ledger`).Scan(&ledger)
	return
}
