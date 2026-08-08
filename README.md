# agent — job pipeline v2

Deterministic spine for Harshil's job-hunt system. Architecture: [DESIGN.md](DESIGN.md).

> **This repository is private and contains Harshil's personal data**, including his date
> of birth, home address, phone number and current compensation
> ([references/profile.md](references/profile.md), [references/answers-bank.md](references/answers-bank.md),
> [bank/bank.yaml](bank/bank.yaml)). Anyone with access to this repo can read all of it.
> Do not make it public, do not fork it to a public namespace, and do not add a
> collaborator you would not tell those things to in person.

## Running this on Harshil's behalf

The point of this repo is that someone else can work the queue while he is at his day
job. Read this whole section before your first run.

**What you can do without him:** discovery (sweep + gate), reading the dashboard, adding
a job you found ([skills/jobd-add](skills/jobd-add/SKILL.md)), and tailoring resumes.

**What needs his machine or his help:** actually submitting an application. The apply
stage drives **Google Chrome signed into his accounts** — his Google login, his existing
profiles on Greenhouse/Amazon/etc. That session lives on his machine and it is not
something to reproduce on yours. Never ask him for a password, and never enter one for
him: if a form needs a login he does not already have in that browser, stop and report
`deferred`. Same for OTPs, 2FA and email security codes — those are his to enter.

### ⚠️ Only one person runs the pipeline at a time

The guarantee that he never applies to the same job twice comes from **one SQLite file**:
`jobd.db` holds both the atomic claim (`UPDATE ... WHERE status='shortlisted'`, exactly
one winner) and the permanent ledger. That file is **not** in this repo — it is
gitignored, because it is state, not code.

So if two people clone this and each run their own `jobd.db`, **there is no shared claim
and no shared ledger, and you can both apply to the same job.** A duplicate application
is the one failure this system exists to prevent; it costs him the role, not just the
slot.

Until a shared instance is set up (see below), the rule is simply: **agree who is running
it before anyone starts**, and get the current `jobd.db` from him rather than starting a
fresh one. `./jobd-bin stats -db jobd.db` should show thousands of jobs and a non-empty
ledger — if it shows zeros you are on a fresh database and you must stop.

*Planned fix:* one daemon on his machine or a small VM, with everyone else pointing the
apply skill at that API over Tailscale. One database, and the atomic claim works exactly
as designed again. Not built yet.

### Setup

```bash
git clone <this repo> && cd agent
go build -o jobd-bin ./cmd/jobd && go build -o resumegen-bin ./cmd/resumegen
```

Two things are deliberately not in the repo and must be obtained from Harshil:

| what | where it goes | needed for |
|---|---|---|
| `jobd.db` | repo root | the dedupe ledger — **do not start without it** |
| GCP service-account key + `GCP_PROJECT`/`GEMINI_MODEL` | `~/.config/jobd/` | the LLM gate (`jobd gate`, and screening submissions) |

Also needed locally: Go, `python3` + `pypdf`, and tectonic (vendored under `tools/`,
fetched per machine — see [DEPLOY.md](DEPLOY.md)).

Then read [RUNBOOK.md](RUNBOOK.md), which is the operational guide: what a normal run
looks like, what is a real fault, and how to recover a wedged job.

### The rules that are not negotiable

These are enforced in code where possible and by the skills otherwise. If you find
yourself working around one, stop and ask him.

- **Never invent a fact.** Every claim on a resume comes from `bank/bank.yaml`; the
  renderer rejects anything else. Every form answer comes from `references/profile.md`
  or `references/answers-bank.md`. If a required field is covered by neither, skip the
  job and flag it — do not guess a date, a number or a preference.
- **Never report `applied` without an on-screen confirmation.**
- **Never skip the claim.** A `409` means someone else has the job; move on.
- **Always report an outcome**, including failures — an unreported claim strands the job
  for an hour.
- Outreach **drafts only**. Sending is his approval on the dashboard, never yours.

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

**Autonomous loop** — one command, no wrapper scripts:

```bash
./jobd-bin daemon -db jobd.db
```

Each cycle: release stale claims → sweep → prefilter → gate (unlimited) → and if the
apply queue is non-empty, invoke a headless coding agent for apply + outreach drafting
([internal/agent](internal/agent/agent.go)). Backends: `-backend auto` (default) tries
Claude Code and fails over to Codex **only on quota exhaustion**, so an exhausted
subscription never stalls the pipeline; `-primary codex` flips the order;
`-backend off` does discovery only. No caps anywhere. See [DEPLOY.md](DEPLOY.md).
