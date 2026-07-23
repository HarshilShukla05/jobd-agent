# agent — job pipeline v2

Deterministic spine for Harshil's job-hunt system. Architecture: [DESIGN.md](DESIGN.md).

## Step 1 (done): experience bank + resume compiler

```
bank/bank.yaml            master experience bank — facts frozen, LLM selects IDs only
resume/template.tex       ATS-safe single-column template (Go template, << >> delims,
                          f-ligatures disabled so extracted text matches ASCII keywords)
selections/baseline.json  selection mirroring the real resume (regression reference)
cmd/resumegen/            renderer: validate selection -> tex -> tectonic -> verify
scripts/verify_pdf.py     asserts 1 page + every bullet survives pypdf text extraction
tools/tectonic            vendored tectonic 0.15.0 (macOS arm64; fetch linux build on deploy)
```

Render a tailored resume:

```bash
go run ./cmd/resumegen -selection selections/<job>.json -out out
```

Guarantees enforced (hard errors, caller falls back to baseline): bullet IDs and skills
must exist in the bank, section budgets respected, only approved alias substitutions,
exactly 1 page, ATS text round-trip passes.

Requires: Go, python3 + pypdf, tectonic (vendored).

## Step 2 (in progress): `jobd`

Built: SQLite store ([internal/store](internal/store/store.go)), 5-ATS clients with
ETag conditional GETs ([internal/ats](internal/ats/ats.go)), seed/sweep/stats commands
([cmd/jobd](cmd/jobd/main.go)). State seeded from the v1 scheduled task: 181 boards,
162 ledger entries; live sweep validated (4.4K postings ingested, idempotent re-sweeps,
two-strikes token death).

```bash
go run ./cmd/jobd seed  -db jobd.db -skill <v1 SKILL.md>
go run ./cmd/jobd sweep -db jobd.db [-max N] [-only ats:token]
go run ./cmd/jobd stats -db jobd.db
```

Also built (2026-07-22 evening): [prefilter](internal/prefilter/prefilter.go) ($0
title/location gate), [jd](internal/jd/jd.go) (lazy full-JD fetch per ATS),
[llm](internal/llm/gemini.go) (`Client` interface + Vertex AI Gemini backend, service
account auth from `~/.config/jobd/env`, evidence-quote validation with one retry then
escalation), and `jobd gate` (policy in code: YoE ≤ 1.5 hard gate, India location,
16 LPA salary floor, stack-overlap scoring). Live run: 4,492 postings → prefilter
dropped 4,421 → Gemini gated survivors with verbatim YoE evidence quotes.

GCP: project `jobd-agent-hs`, Vertex AI enabled, SA `jobd-daemon` (aiplatform.user),
key at `~/.config/jobd/sa.json`. Note: the consumer Gemini API is prepay-only now —
Vertex AI is the path that bills the $300 credits.

Also built: `jobd daemon` — sweep+gate cycles on a jittered 30-60m interval plus the
master dashboard/API ([internal/web](internal/web/server.go)) on `127.0.0.1:8383`:
`GET /` (dashboard: apply queue, pending-for-Harshil, run log), `GET /api/apply-queue`,
`GET /api/stats`, `POST /api/jobs/{id}/result` (apply-stage writeback). Hardening from
live runs: Vertex 429/5xx exponential backoff (never escalates rate limits), prefilter
excludes for support/QA/customer-facing and mismatched-stack titles (.NET/PHP/frontend/
mobile/etc.), and ledger dedupe by job CODE fragment (catches Workable aggregator
URL-path variants — found a real antarcticaglobal dup on day one).

**Apply stage** ([skills/jobd-apply](skills/jobd-apply/SKILL.md)): a Claude session
drives claim → context → tailor → Chrome form → result. API: `POST /api/jobs/{id}/claim`
(atomic, 409 = already taken — the double-apply guard), `GET .../context` (JD + bank),
`POST .../resume` (renders tailored PDF, 422 + baseline fallback on rule violation),
`POST .../result` (writes ledger on applied/dead/rejected_hard). Stale claims return to
the queue after 1h. Full lifecycle smoke-tested.

See [RUNBOOK.md](RUNBOOK.md) for what a full run looks like and what to watch for.

**Outreach** ([skills/jobd-outreach](skills/jobd-outreach/SKILL.md) +
[internal/gmail](internal/gmail/gmail.go)): a Claude session researches contacts,
infers emails with a confidence tier, and stages drafts — never sends. Harshil approves
on the dashboard, then `jobd outreach-send` delivers via the Gmail API (send-only
scope, his own one-time OAuth). Rails: 20/day cap, 30-120s spacing, 8% trailing-bounce
circuit breaker, and a bounce permanently blacklists that domain.

**Autonomous loop**: `jobd daemon -apply-cmd deploy/run-apply.sh` runs sweep → prefilter
→ gate (unlimited) → and then invokes headless Claude sessions for apply + outreach
drafting. No caps anywhere: every survivor gets gated, every shortlisted job gets
applied to. See [DEPLOY.md](DEPLOY.md) for the laptop install.
