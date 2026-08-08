---
name: jobd-add
description: Put a job Harshil found himself into the jobd pipeline. Use when he shares a job link, a LinkedIn post, a forwarded JD, or a referral and wants it screened and queued — "add this job", "can you check this posting", "/jobd-add <url>". Runs the same dedupe, prefilter and LLM gate the daemon runs, then adds survivors to the apply queue.
---

# jobd add — the manual intake stage

Harshil hands you a posting. Your job is to get it **through the filters** and into the
apply queue if it passes — not to judge it yourself. The filters are code, they are the
same ones the daemon runs on the 178 swept boards, and they are the reason a job in the
queue can be applied to without a second thought.

**API base**: `http://127.0.0.1:8383`

### Step 0 — make sure jobd is reachable

```
curl -sf http://127.0.0.1:8383/api/stats || (cd ~/projects/agent && (./jobd-bin daemon -db jobd.db -backend off >/tmp/jobd.log 2>&1 &) && sleep 4 && curl -sf http://127.0.0.1:8383/api/stats)
```

If the daemon will not come up, fall back to the CLI, which does exactly the same thing
against the database directly:

```
cd ~/projects/agent && ./jobd-bin add -db jobd.db -url '<url>' [-title ... -company ... -jd file.txt] [-force]
```

## 1. Find the real application URL

What Harshil shares is often not the posting. Resolve it first:

- A **LinkedIn/X/Slack post** about a role → find the actual apply link. Follow the
  "Apply" link out to the company's board; that Greenhouse/Lever/Ashby/Workable/
  SmartRecruiters URL is what you submit, not the post.
- A **LinkedIn job page** (`linkedin.com/jobs/view/...`) → check whether it says "Apply
  on company website". If it does, submit that URL instead — jobd can read board URLs
  itself and dedupe them against everything it has already seen.
- Only when there is genuinely no board URL do you submit the LinkedIn/careers-page link.

This matters: a tracked-board URL gets full dedupe against the ledger, an auto-fetched
JD, and the company added to the sweep registry so every future opening there arrives on
its own. A LinkedIn URL gets none of that.

## 2. Submit it

**Tracked board** (greenhouse / lever / ashby / workable / smartrecruiters) — jobd
fetches the title, location and JD itself. Nothing else is needed:

```
curl -s -X POST http://127.0.0.1:8383/api/jobs/submit \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://job-boards.greenhouse.io/acme/jobs/1234567","note":"referral from Ankit"}'
```

**Anything else** — you must supply the JD text and title, because jobd cannot read the
page. Open the posting (Chrome, or WebFetch) and copy the description:

```
curl -s -X POST http://127.0.0.1:8383/api/jobs/submit \
  -H 'Content-Type: application/json' \
  -d @payload.json
```
```json
{
  "url": "https://www.linkedin.com/jobs/view/4123456789/",
  "title": "Software Engineer 1",
  "company": "Netomi",
  "location": "Gurugram, India",
  "jd_text": "<the full job description, verbatim>",
  "note": "seen on LinkedIn"
}
```

Write the JD to a file and use `-d @file` — quoting a long JD inline will break.

**Copy the JD in full, never a summary.** The gate reads it for the YoE requirement and
the apply stage scores keyword coverage against it. A précis under-reports coverage
badly — the full Crunchyroll posting matched 17 terms, a summary of the same posting
only 8.

A `422` means exactly one thing: the URL is not a tracked board and you did not supply
`jd_text` + `title`. Fetch the page and re-post. It is not a failure.

## 3. Report the verdict — do not argue with it

The response tells you everything:

| `decision` | what happened | what you say |
|---|---|---|
| `queued` | passed every filter, `status: shortlisted` | it's in the queue at score N |
| `rejected` | a filter rejected it — `stage` says which, `note` says why | quote the reason verbatim |
| `duplicate` | already known — `stage: ledger` (applied/rejected before) or `jobs` (already queued) | say which, do not re-add |
| `deferred` | the gate could not run (JD too short, or no LLM) | needs Harshil's eye |

Report the `note` as-is. If it was rejected, **tell him the reason and stop** — do not
resubmit with `force` on your own initiative. `board_added: true` is worth mentioning:
that company's whole board is now swept automatically.

### force — only when Harshil says so

`{"force": true}` queues the job even though a filter rejected it. Use it **only** when
he explicitly asks after hearing the reason ("add it anyway"). It overrides the
decision, not the facts: the gate still runs and the note records what was overridden.

It cannot reopen a job already applied to — the ledger wins over force, always. If you
see `duplicate` at `stage: ledger`, that is final.

## 4. Then what

The job is now indistinguishable from one the daemon found. It sits in
`/api/apply-queue` and the next apply run picks it up. If Harshil wants it done now:

> Read `skills/jobd-apply/SKILL.md` and follow it exactly.

and afterwards, for the outreach draft, `skills/jobd-outreach/SKILL.md`.

Several links at once: submit them one at a time and report a line per job. Do not batch
them into one verdict — each has its own reason.
