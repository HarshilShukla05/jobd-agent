// jobd is the deterministic spine of the job pipeline. Commands:
//
//	jobd seed  -db jobd.db -skill <v1 SKILL.md>   import registry tokens + ledger
//	jobd sweep -db jobd.db [-max N]               sweep boards, record new postings
//	jobd gate  -db jobd.db [-max N]               prefilter 'new' jobs, LLM-gate survivors
//	jobd add   -db jobd.db -url <posting url>     run ONE hand-found job through the filters
//	jobd stats -db jobd.db                        print table counts
//
// The sweep is politeness-hardened per DESIGN.md: sequential per-host requests
// with 1-3s jittered spacing, conditional GETs, and two-strikes token death.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/harshil/agent/internal/agent"
	"github.com/harshil/agent/internal/ats"
	"github.com/harshil/agent/internal/intake"
	"github.com/harshil/agent/internal/jd"
	"github.com/harshil/agent/internal/llm"
	"github.com/harshil/agent/internal/policy"
	"github.com/harshil/agent/internal/prefilter"
	"github.com/harshil/agent/internal/store"
	"github.com/harshil/agent/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: jobd <seed|sweep|stats> [flags]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dbPath := fs.String("db", "jobd.db", "sqlite database path")
	skill := fs.String("skill", "", "seed: path to v1 pipeline SKILL.md")
	max := fs.Int("max", 0, "sweep: max companies this pass (0 = all)")
	only := fs.String("only", "", "sweep: restrict to one board, as ats:token")
	listen := fs.String("listen", "127.0.0.1:8383", "daemon: dashboard/API listen address")
	gateMax := fs.Int("gate-max", 0, "daemon: max LLM gates per cycle (0 = unlimited)")
	backend := fs.String("backend", "auto", "daemon: apply/outreach agent — auto | claude | codex | off")
	primary := fs.String("primary", "claude", "daemon: which backend auto tries first (claude | codex)")
	from := fs.String("from", "harshilshukla0502@gmail.com", "outreach-send: From address")
	dryRun := fs.Bool("dry-run", false, "outreach-send: print what would be sent, send nothing")
	intervalMin := fs.Int("interval-min", 30, "daemon: min minutes between cycles")
	intervalMax := fs.Int("interval-max", 60, "daemon: max minutes between cycles")
	addURL := fs.String("url", "", "add: the job posting URL")
	addTitle := fs.String("title", "", "add: job title (required when -jd supplies the text)")
	addCompany := fs.String("company", "", "add: company name (non-board URLs)")
	addLocation := fs.String("location", "", "add: location, if the URL is not a tracked board")
	addJD := fs.String("jd", "", "add: file holding the JD text, for boards jobd cannot read")
	addNote := fs.String("note", "", "add: free-text note kept on the job (e.g. 'referral')")
	addForce := fs.Bool("force", false, "add: queue it even if a filter rejects it")
	fs.Parse(args)

	st, err := store.Open(*dbPath)
	if err != nil {
		die("open db: %v", err)
	}

	switch cmd {
	case "seed":
		if *skill == "" {
			die("seed requires -skill")
		}
		seed(st, *skill)
	case "sweep":
		sweep(st, *max, *only)
	case "gate":
		gate(st, *max)
	case "add":
		if *addURL == "" {
			die("add requires -url")
		}
		add(st, *addURL, *addTitle, *addCompany, *addLocation, *addJD, *addNote, *addForce)
	case "daemon":
		var runner *agent.Runner
		if *backend != "off" {
			cwd, _ := os.Getwd()
			runner = &agent.Runner{
				Backend: agent.Backend(*backend),
				Primary: agent.Backend(*primary),
				Dir:     cwd,
			}
			if !agent.Available(agent.Claude) && !agent.Available(agent.Codex) {
				die("no apply agent found: install Claude Code or Codex, " +
					"or run with -backend off to do discovery only")
			}
		}
		daemon(st, *listen, *gateMax, *intervalMin, *intervalMax, runner)
	case "auth-gmail":
		authGmail()
	case "outreach-send":
		outreachSend(st, *from, *dryRun)
	case "stats":
		c, j, l, err := st.Counts()
		if err != nil {
			die("stats: %v", err)
		}
		fmt.Printf("companies(active): %d  jobs: %d  ledger: %d\n", c, j, l)
	default:
		die("unknown command %q", cmd)
	}
}

var (
	tokenRe  = regexp.MustCompile(`\b(greenhouse|lever|ashby|workable|smartrecruiters):([A-Za-z0-9][A-Za-z0-9._-]*)(\??)`)
	ledgerRe = regexp.MustCompile(`^(https?://\S+)\s*\|\s*([a-z-]+)`)
)

// seed imports the v1 scheduled task's COMPANY REGISTRY tokens and SEEN JOBS
// LEDGER lines. Tokens suffixed "?" (failed last v1 run) import as 'unstable'.
func seed(st *store.Store, skillPath string) {
	f, err := os.Open(skillPath)
	if err != nil {
		die("open skill: %v", err)
	}
	defer f.Close()

	var nTok, nLed int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if m := ledgerRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			if err := st.AddLedger(m[1], m[2]); err != nil {
				die("ledger insert: %v", err)
			}
			nLed++
			continue
		}
		for _, m := range tokenRe.FindAllStringSubmatch(line, -1) {
			status := "active"
			if m[3] == "?" {
				status = "unstable"
			}
			if err := st.AddCompany(m[1], m[2], status); err != nil {
				die("company insert: %v", err)
			}
			nTok++
		}
	}
	if err := sc.Err(); err != nil {
		die("scan: %v", err)
	}
	fmt.Printf("seeded: %d token refs, %d ledger entries\n", nTok, nLed)
}

func sweep(st *store.Store, max int, only string) (okBoards, failedBoards, newJobs int) {
	if max <= 0 {
		max = 1 << 30
	}
	companies, err := st.ActiveCompanies(max)
	if err != nil {
		die("companies: %v", err)
	}
	if only != "" {
		ats, token, ok := strings.Cut(only, ":")
		if !ok {
			die("-only must be ats:token")
		}
		all, err := st.ActiveCompanies(1 << 30)
		if err != nil {
			die("companies: %v", err)
		}
		companies = nil
		for _, c := range all {
			if c.ATS == ats && c.Token == token {
				companies = []store.Company{c}
			}
		}
		if companies == nil {
			die("board %s not found or dead", only)
		}
	}
	// randomize order every pass (anti-uniformity, per DESIGN.md)
	rand.Shuffle(len(companies), func(i, j int) { companies[i], companies[j] = companies[j], companies[i] })

	client := &http.Client{Timeout: 30 * time.Second}
	ctx := context.Background()
	var unchanged int

	for i, c := range companies {
		if i > 0 {
			time.Sleep(time.Second + time.Duration(rand.Intn(2000))*time.Millisecond)
		}
		res, err := ats.Fetch(ctx, client, c.ATS, c.Token, c.ETag)
		if err != nil {
			failedBoards++
			fmt.Printf("FAIL  %s:%s  %v\n", c.ATS, c.Token, err)
			if err := st.SweepResult(c, "", false); err != nil {
				die("record fail: %v", err)
			}
			continue
		}
		okBoards++
		if res.NotModified {
			unchanged++
			if err := st.SweepResult(c, c.ETag, true); err != nil {
				die("record 304: %v", err)
			}
			continue
		}
		for _, p := range res.Postings {
			if _, hit, err := st.InLedger(p.URL); err != nil {
				die("ledger check: %v", err)
			} else if hit {
				continue // already applied/rejected in v1 — never resurface
			}
			// same job code under a different URL path (Workable aggregators)
			if _, hit, err := st.InLedgerByFragment("/" + p.ReqID); err != nil {
				die("ledger fragment check: %v", err)
			} else if hit {
				continue
			}
			_, isNew, err := st.UpsertJob(store.NewJob{
				ATS: c.ATS, Token: c.Token, ReqID: p.ReqID, URL: p.URL,
				Title: p.Title, Location: p.Location, PostedAt: p.PostedAt,
			})
			if err != nil {
				die("upsert: %v", err)
			}
			if isNew {
				newJobs++
				fmt.Printf("NEW   %-60.60s  %-25.25s  %s\n", p.Title, p.Location, p.URL)
			}
		}
		if err := st.SweepResult(c, res.ETag, true); err != nil {
			die("record ok: %v", err)
		}
	}
	fmt.Printf("\nsweep done: %d boards ok (%d unchanged), %d failed, %d new postings\n",
		okBoards, unchanged, failedBoards, newJobs)
	return okBoards, failedBoards, newJobs
}

// daemon runs sweep+gate cycles forever on a jittered interval and serves the
// dashboard/API. This is the process the laptop runs under systemd.
func daemon(st *store.Store, listen string, gateMax, intervalMin, intervalMax int, runner *agent.Runner) {
	srv := &http.Server{Addr: listen, Handler: web.New(st).Routes()}
	go func() {
		fmt.Printf("dashboard on http://%s\n", listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			die("http: %v", err)
		}
	}()
	for {
		started := time.Now().UTC()
		// an apply session that crashed mid-job shouldn't strand it forever
		if n, err := st.ReleaseStaleClaims(time.Hour); err != nil {
			die("release claims: %v", err)
		} else if n > 0 {
			fmt.Printf("released %d stale claim(s) back to the queue\n", n)
		}
		ok, failedB, newJ := sweep(st, 0, "")
		gatedN, shortN := gate(st, gateMax)
		if err := st.InsertRun(store.Run{
			StartedAt: started.Format(time.RFC3339), FinishedAt: time.Now().UTC().Format(time.RFC3339),
			BoardsOK: ok, BoardsFail: failedB, NewJobs: newJ, Gated: gatedN, Shortlisted: shortN,
		}); err != nil {
			die("record run: %v", err)
		}
		// Hand the shortlist downstream. Trigger on QUEUE DEPTH, not this
		// cycle's new shortlists — a cycle that shortlists nothing must still
		// work the jobs already waiting.
		if runner != nil {
			queued, err := st.JobRows("shortlisted", 1000)
			if err != nil {
				die("queue depth: %v", err)
			}
			if len(queued) > 0 {
				runDownstream(st, runner, len(queued))
			} else {
				fmt.Println("downstream skipped: apply queue empty")
			}
		}

		mins := intervalMin
		if intervalMax > intervalMin {
			mins += rand.Intn(intervalMax - intervalMin)
		}
		fmt.Printf("cycle done %s — next in %dm\n\n", time.Now().UTC().Format(time.RFC3339), mins)
		time.Sleep(time.Duration(mins) * time.Minute)
	}
}

// gate runs the $0 prefilter over ALL 'new' jobs, then the LLM gate over up
// to max survivors (JD fetch -> Gemini extraction -> YoE/salary policy).
func gate(st *store.Store, max int) (gatedN, shortlistedN int) {
	if max <= 0 {
		max = 1 << 30 // unlimited: gate every prefilter survivor this cycle
	}
	jobs, err := st.JobsByStatus("new", 1<<30)
	if err != nil {
		die("jobs: %v", err)
	}
	var kept []store.Job
	var dropped int
	for _, j := range jobs {
		if keep, note := prefilter.Check(j.Title, j.Location); !keep {
			if err := st.SetJobStatus(j.ID, "rejected_hard", note); err != nil {
				die("reject: %v", err)
			}
			dropped++
		} else {
			kept = append(kept, j)
		}
	}
	cap := fmt.Sprint(max)
	if max >= 1<<30 {
		cap = "unlimited"
	}
	fmt.Printf("prefilter: %d dropped, %d kept (LLM gate cap: %s)\n\n", dropped, len(kept), cap)

	ctx := context.Background()
	gem, err := llm.NewGeminiFromEnv(ctx)
	if err != nil {
		die("gemini: %v", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}

	for _, j := range kept {
		if gatedN >= max {
			break
		}
		time.Sleep(500*time.Millisecond + time.Duration(rand.Intn(1000))*time.Millisecond)
		text, err := jd.Fetch(ctx, client, j.ATS, j.Token, j.ReqID)
		if err != nil {
			if err := st.SetJobStatus(j.ID, "dead", "jd fetch: "+err.Error()); err != nil {
				die("mark dead: %v", err)
			}
			fmt.Printf("DEAD  %-55.55s %v\n", j.Title, err)
			continue
		}
		if len(text) < 200 {
			if err := st.SetJobStatus(j.ID, "deferred", "jd too short for gate"); err != nil {
				die("defer: %v", err)
			}
			continue
		}
		v, raw, err := gem.GateJD(ctx, j.Title, text)
		gatedN++
		if err != nil {
			// schema drift twice -> Claude session queue per DESIGN.md
			if err := st.SetJobStatus(j.ID, "deferred", "gate escalation: "+err.Error()); err != nil {
				die("defer: %v", err)
			}
			fmt.Printf("ESC   %-55.55s %v\n", j.Title, err)
			continue
		}
		status, note, score := policy.Apply(v)
		if err := st.SaveGate(j.ID, status, note, raw, v.YoeMin, score); err != nil {
			die("save gate: %v", err)
		}
		if status == "shortlisted" {
			shortlistedN++
		}
		fmt.Printf("%-5s %-55.55s score=%-3d %s\n", strings.ToUpper(status[:4]), j.Title, score, note)
	}
	fmt.Printf("\ngate done: %d LLM-gated this pass, %d prefilter-kept remain for next pass\n",
		gatedN, len(kept)-gatedN)
	return gatedN, shortlistedN
}

// add is the manual intake path: one job Harshil found himself, pushed through
// the same dedupe/prefilter/gate/policy the sweep uses. The HTTP endpoint
// (POST /api/jobs/submit) is the same code; this exists for when the daemon
// isn't up, and for piping a JD in from a file.
func add(st *store.Store, url, title, company, location, jdFile, note string, force bool) {
	req := intake.Request{URL: url, Title: title, Company: company,
		Location: location, Note: note, Force: force}
	if jdFile != "" {
		b, err := os.ReadFile(jdFile)
		if err != nil {
			die("read -jd: %v", err)
		}
		req.JDText = string(b)
	}
	ctx := context.Background()
	deps := intake.Deps{Store: st, Client: &http.Client{Timeout: 30 * time.Second}}
	if gem, err := llm.NewGeminiFromEnv(ctx); err == nil {
		deps.Gate = gem
	} else {
		fmt.Fprintf(os.Stderr, "warning: LLM gate unavailable (%v)\n", err)
	}
	res, err := intake.Submit(ctx, deps, req)
	if err != nil {
		die("submit: %v", err)
	}
	label := res.Title
	if label == "" {
		label = res.URL
	}
	fmt.Printf("%-9s %s\n", strings.ToUpper(res.Decision), label)
	fmt.Printf("  job %d  %s:%s  status=%s score=%d  (decided at: %s)\n",
		res.JobID, res.ATS, res.Company, res.Status, res.Score, res.Stage)
	fmt.Printf("  %s\n", res.Note)
	if res.BoardAdded {
		fmt.Printf("  + %s:%s added to the sweep registry — future postings there come in automatically\n",
			res.ATS, res.Company)
	}
}

// runDownstream drives the apply and outreach stages via a coding-agent CLI
// (Claude Code or Codex, with quota failover). Never fatal: a downstream
// failure must not kill the discovery daemon.
//
// Progress is measured from the database, not the agent's words: if the
// queue depth didn't move, the backend did nothing, and the failover in
// agent.Run is allowed to try the other CLI.
func runDownstream(st *store.Store, r *agent.Runner, queued int) {
	fmt.Printf("\n=== downstream: %d job(s) in the apply queue ===\n", queued)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()

	countShortlisted := func() int {
		rows, err := st.JobRows("shortlisted", 10000)
		if err != nil {
			return -1
		}
		return len(rows)
	}
	before := countShortlisted()
	applyProgressed := func() bool {
		n := countShortlisted()
		return n >= 0 && n < before // jobs left the queue = real work happened
	}
	if err := r.Run(ctx, "apply stage", agent.ApplyPrompt, applyProgressed); err != nil {
		fmt.Printf("apply stage failed (continuing): %v\n", err)
	}
	if applied := before - countShortlisted(); applied <= 0 {
		fmt.Println("no jobs left the queue — skipping outreach drafting this cycle")
		return
	}

	draftsBefore, _ := st.OutreachByStatus("draft", 1000)
	outreachProgressed := func() bool {
		d, err := st.OutreachByStatus("draft", 1000)
		return err == nil && len(d) > len(draftsBefore)
	}
	if err := r.Run(ctx, "outreach drafting", agent.OutreachPrompt, outreachProgressed); err != nil {
		fmt.Printf("outreach drafting failed (continuing): %v\n", err)
	}
	fmt.Println("=== downstream done ===")
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "jobd: "+format+"\n", a...)
	os.Exit(1)
}

func randInt(n int) int { return rand.Intn(n) }
