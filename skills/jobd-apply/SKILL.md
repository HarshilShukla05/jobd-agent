---
name: jobd-apply
description: Apply to jobs from the jobd queue. Use when Harshil says "run the apply stage", "apply to the queue", "/jobd-apply", or when the jobd dashboard has shortlisted jobs waiting. Claims each job atomically, tailors his resume from the experience bank, fills the ATS form in Chrome, and writes the outcome back to jobd.
---

# jobd apply stage

You are the apply worker for Harshil's job pipeline. `jobd` has already swept the
boards, prefiltered, and LLM-gated — every job in the queue passed the YoE/location/
salary gates. Your job: claim, tailor, apply, report. Never re-litigate the gate.

**API base**: `http://127.0.0.1:8383`

### Step 0 — make sure jobd is reachable (do this first, always)

```
curl -sf http://127.0.0.1:8383/api/stats || (cd ~/agent && (./jobd-bin daemon -db jobd.db -backend off >/tmp/jobd.log 2>&1 &) && sleep 4 && curl -sf http://127.0.0.1:8383/api/stats)
```

The daemon serves the API this whole skill depends on. If it isn't running, start it
in API-only mode (`-backend off`, so it does not recursively invoke another apply
stage) as shown above. If it still won't come up after that, stop and report — do not
attempt to apply without it.

## Loop (one job at a time, max 5 per run unless told otherwise)

### 1. Pull the queue
```
curl -s http://127.0.0.1:8383/api/apply-queue
```
Work highest `score` first. If empty, report "queue empty" and stop.

### 2. Claim it — THE DOUBLE-APPLY GUARD
```
curl -s -X POST http://127.0.0.1:8383/api/jobs/<id>/claim
```
- `200 {"claimed": true}` → it is yours, proceed.
- `409` → someone/something already took it. **Skip immediately.** Never apply to a
  job you did not win the claim for. This is the only thing standing between the
  system and a duplicate application.

### 3. Get context
```
curl -s http://127.0.0.1:8383/api/jobs/<id>/context
```
Returns `job` (url, title, company, gate facts), `jd_text` (full JD), `bank_yaml`
(the experience bank), and `jd_error`.

If `jd_error` is non-empty or `jd_text` is empty → the posting is gone. Report
`dead` (step 7) and move on. Do not open a browser for a dead posting.

**Re-verify the gate cheaply**: skim `jd_text` for a YoE minimum **above 2 years** that
the gate missed. If you find one, report `rejected_hard` with the verbatim quote and
move on. Harshil has ~1 year; 1-2 year postings are a deliberate stretch and SHOULD be
applied to (the note will say "YoE stretch") — do not skip those. This is the
human-level double-check; it costs one read.

### 4. Tailor the resume
Read `bank_yaml`. Choose the bullets whose `tags` best mirror this JD, respecting each
role's `min_bullets`/`max_bullets`, and never selecting two bullets that list each
other in `conflicts`. Order them so the most JD-relevant lead. Build the tech lines and
skills lines from the pools, leading with what the JD asks for.

**You never write resume prose.** You only choose IDs and ordering. Any term
substitution must be in the bank's `aliases` map.

POST the selection (same shape as `selections/baseline.json`):
```
curl -s -X POST http://127.0.0.1:8383/api/jobs/<id>/resume -d @selection.json
```
- `200 {"ok": true, "pdf": "..."}` → use that PDF; `resume_tailored = true`.
- `422` → your selection broke a rule (the error says which). Fix it and retry **once**.
  If it fails again, use the `fallback_pdf` from the response and set
  `resume_tailored = false`. Never let a resume failure block the application.

### 4b. Score it, then improve it — iterate up to twice

Save the JD to `jds/<company>.txt` and score the render:
```
./resumegen-bin -selection selections/<job>.json -out out -name <Name> -jd jds/<company>.txt -screen
```

Three layers, matching how screening actually works (see `RESEARCH.md`):
- **ATS MATCH** — searchability. Recruiters boolean-search the ATS; a JD term you
  lack means you never surface. Target **>=75%**.
- **7-SECOND SKIM** — the six eye-tracking fixation points and the layout rules.
  Target **8/8**; a FAIL here is a real defect.
- **AI RECRUITER SCREEN** — verdict `advance` / `maybe` / `reject`. Target `advance`.

If below target, revise the selection using the MISSING list — swap in bank bullets
that legitimately carry those terms — and re-render. **Stop after two revisions.**
A term with no truthful bullet behind it is a genuine gap: leave it missing and
note it. Never add a term the bank cannot back.

Ignore any screen comment claiming the employment dates are in the future — that is
a model knowledge-cutoff artifact, not a defect.

### 5. Apply — IN GOOGLE CHROME, ALWAYS

**Chrome is NOT Harshil's default browser.** Every application must happen in Google
Chrome, because that is the browser signed into his accounts (Google, job boards) and
the one his ATS workarounds are proven against. Never let the OS pick.

- **Claude Code**: use the Chrome browser tools (the connected-Chrome MCP). Before the
  first job, confirm you are actually driving Chrome — list the connected browsers /
  tab context and verify. If no Chrome connection is available, STOP and report
  `deferred` with "Chrome not connected"; do not fall back to any other browser.
- **Codex**: drive Chrome explicitly through the browser backend. If you must open a
  URL from a shell, use `open -a "Google Chrome" <url>` on macOS or
  `google-chrome <url>` on Linux — never a bare `open`/`xdg-open`, which would launch
  his default browser (Zen) where he is not signed in.
- If a form opens in the wrong browser, close it and redo the step in Chrome rather
  than filling it there.

**Gate 0 (before investing in any form)**: navigate to the job URL and read the page
back. If navigation reports success but the page content is stale or from a different
URL, that is the known Chrome-extension reversion fault — STOP the whole apply stage,
report the remaining jobs as `deferred`, and say so plainly. Do not fight it.

**Facts come from two files in this repo, and nowhere else:**
- `references/profile.md` — the ONLY source of factual answers (identity, education,
  employment, compensation, work authorization, form workarounds).
- `references/answers-bank.md` — pre-approved verbatim answers to common questions.

If a required field is covered by neither, **skip the job and flag it**. Never guess a
date, a number, a credential, or a preference. Anything marked `[TODO]` in profile.md
is unanswerable — skip jobs that require it.

Upload the PDF from step 4.

Carry over these hard-won rules:
- **Verify the posting is still live** before investing in a long form.
- **Ashby autofill** can silently wipe fields — re-verify every field before submit.
- **Ashby false validation**: a first submit may wrongly claim a required field is
  missing; re-set the value and submit once more before treating it as blocked.
- **SmartRecruiters** postings with a required resume upload are structurally blocked
  (shadow DOM). Spend ≤10 tool calls confirming, then report `blocked`.
- Prefer Quick Apply / MyGreenhouse when offered.

### 5b. KNOCKOUT QUESTIONS — the highest-risk step in the whole pipeline

Research finding (`RESEARCH.md`): on Greenhouse/Lever the resume is **not**
auto-scored — the only automatic, invisible rejection comes from **knockout
questions** on the form. These are usually: years of experience, work
authorization, willingness to relocate, and salary expectations.

So slow down on those specific fields:
- Answer **only** from `references/profile.md`. Never round up, never guess.
- Years of experience: answer truthfully (~1 year). Do not inflate to clear a bar —
  a false answer here is what gets caught at the recruiter screen anyway.
- Work authorization: India roles → authorized, no sponsorship needed. Outside
  India → sponsorship required.
- Salary: expected ₹16–20 LPA. If a form forces a single number, use the profile's
  expected range midpoint, never below the ₹16 LPA floor.
- If a knockout field has no truthful answer available, **skip the job and flag it**
  rather than guessing — a wrong answer is an instant silent reject.

### 6. HARD STOPS — report and move on, never work around
- CAPTCHA, password login, account creation, 2FA/OTP → `blocked`
- Email security code required → `deferred`, note "code needed in Gmail"
- Google OAuth **consent** screen (not just an account picker) → `deferred`
- A required field with no truthful answer, or a required upload of a document that
  does not exist (device specs, speed tests) → `blocked`. **Never fabricate a fact or
  a document.** A genuine "Other — not listed" option is fine to use.
- Any answer not covered by profile.md/answers-bank.md → answer from profile facts
  only; if impossible, `deferred`.

### 7. Report the outcome — ALWAYS, even on failure
```
curl -s -X POST http://127.0.0.1:8383/api/jobs/<id>/result \
  -H 'Content-Type: application/json' \
  -d '{"status":"applied","note":"Greenhouse quick apply, confirmation shown","resume_tailored":true}'
```
Valid `status`: `applied` · `blocked` · `deferred` · `dead` · `rejected_hard`.

`applied`, `dead`, and `rejected_hard` also write the permanent ledger entry, so the
job can never be re-discovered by a future sweep. **A claimed job you never report
stays stuck in `applying` for an hour, then returns to the queue** — so always report,
even when you failed.

## Final report to Harshil
- ✅ applied (n): company — role — tailored y/n
- ⏭️ skipped: company — reason (blocked/dead/gate)
- 🔴 needs Harshil: security codes, OAuth consents, anything deferred
- 📊 queue remaining

Report honestly. If you applied to 2 of 5, say 2 of 5 and why — never pad, never
claim an application you did not confirm on screen.
