# Runbook — what a full agent run looks like

Read this before your first unattended run so you know what is normal and what is a
real fault.

## Start it

Terminal 1 — the daemon (leave it open; this is the whole discovery+gate engine):

    cd ~/agent && ./jobd-bin daemon -db jobd.db

Browser — the dashboard, refreshes itself every 2 min:

    open http://127.0.0.1:8383

Terminal 2 (or a Claude session) — the apply stage, when the queue has jobs:

    claude "/jobd-apply — work the queue, max 5 applications"

## Stage 1 — SWEEP (~4-8 min, no LLM, no cost)

Console prints one line per newly seen posting:

    NEW   Backend Engineer, Payments   Bengaluru   https://job-boards.greenhouse.io/...
    sweep done: 181 boards ok (46 unchanged), 3 failed, 812 new postings

Normal:
- "unchanged" grows over time — those are free ETag 304s, a sign caching works.
- 2-8 board failures per sweep. Dead tokens get one strike, then removed on the second.
- Thousands of NEW lines on the first sweeps, dropping to tens once caught up.

Watch for: >30 failed boards (network/DNS problem, not the boards).

## Stage 2 — PREFILTER (instant, no LLM, no cost)

    prefilter: 4421 dropped, 71 kept (LLM-gating up to 25 now)

Normal: 95-99% dropped. That is the design — it kills senior/non-engineering/wrong-
stack titles for free so the LLM only sees plausible roles.

Watch for: a *low* drop rate (<90%) means the regexes stopped matching — check for a
title pattern the filter doesn't know.

## Stage 3 — LLM GATE (~3-6 s/job, ~$0.0001/job on your GCP credits)

One line per job, with the model's verbatim evidence:

    REJE  SDE 2 - Fullstack        score=0   YoE gate: min 3.0 > 2.0 ("3+ years of full stack development experience.")
    SHOR  Backend Engineer         score=45  YoE ideal: 1.0 ("1+ years building backend services")
    SHOR  Platform Engineer        score=35  YoE stretch: 2.0 vs his ~1yr ("2+ years of backend experience")
    DEAD  Platform Engineer        posting 404 (pulled from board)
    ESC   Data Engineer            gate failed validation twice: yoe_evidence not verbatim

Normal:
- Mostly REJE. India's junior market is genuinely thin — v1 saw the same.
- Occasional DEAD (postings vanish between sweep and gate).
- Rare ESC (deferred for you to eyeball on the dashboard).

Watch for:
- **Every** job ESC'ing → Vertex auth or quota problem. Check `curl -s
  http://127.0.0.1:8383/api/stats` and the console for a 429/403 that survived backoff.
- A shortlisted job whose evidence quote looks wrong → tell me, that's a prompt bug.
- 429s are retried automatically (2s/6s/15s). Seeing one logged then succeeding is fine.

## Stage 4 — APPLY (Claude session + Chrome, you watch or leave it)

Per job, in order: **claim → context → tailor → fill → report.**

    claim   → 200 = mine, 409 = someone already took it (skips — this is the
                     double-apply guard; a 409 in the log is the system working)
    context → JD text + experience bank
    resume  → 200 = tailored PDF, 422 = selection broke a bank rule (retries once,
                     then falls back to the untailored baseline)
    result  → applied | blocked | deferred | dead | rejected_hard

Normal:
- 2-4 applications out of 5 attempts. Blocked/dead ones are honest outcomes, not bugs.
- SmartRecruiters jobs with required resume upload → `blocked` (structural shadow-DOM
  issue, known since v1). Don't expect these to ever work.
- A `deferred` for an email security code or a Google consent screen means it needs
  *you*; it shows in "Pending for Harshil" on the dashboard.

Watch for:
- **Any application you didn't authorize the facts for.** The agent may never fabricate
  a fact, a skill, or a document. If a report mentions inventing anything, stop and
  tell me.
- The same company applied twice → should be impossible (claim + ledger + job-code
  dedupe). If you ever see it, that's a P0 bug — send me the two URLs.
- Claude reporting "applied" without an on-screen confirmation → it's instructed not
  to; flag it if you see it.

## Dedupe — why you won't double-apply

Four independent layers, each sufficient on its own:
1. **Claim**: `UPDATE ... WHERE status='shortlisted'` is atomic — exactly one winner.
2. **Ledger**: `applied`/`dead`/`rejected_hard` writes a permanent URL row; sweeps skip
   any URL in it forever (seeded with all 162 of v1's entries).
3. **Job code**: dedupe also matches by req ID fragment, catching aggregators that
   re-serve the same job under a different URL path (this caught a real v1 duplicate).
4. **Unique index**: `(ats, token, req_id)` — the same posting can only ever be one row.

Stale claims (crashed session) auto-return to the queue after 1 hour.

## After a run

Dashboard: apply queue, "Pending for Harshil", and the run log (boards/new/gated/
shortlisted per cycle).

    sqlite3 ~/agent/jobd.db "SELECT applied_at, title, token, resume_tailored, note FROM jobs WHERE status='applied' ORDER BY applied_at DESC LIMIT 20"

Cost check (should be pennies):

    open "https://console.cloud.google.com/billing?project=jobd-agent-hs"

## Known-blocked, don't be surprised

- **Outreach/Gmail sending**: not built yet (next step).
- **Apollo**: not used at all — costs money, replaced by web-search contact discovery
  when outreach lands.
- **SmartRecruiters resume uploads**: structurally blocked, permanent skip.
- **bank.yaml**: still needs your fact validation, especially the FRD-doc date range
  (Oct 2024-Jul 2025) vs your resume (intern Jul-Sep 2025, SE1 Oct 2025-present). Until
  you resolve that, treat tailored resumes as draft-quality.
