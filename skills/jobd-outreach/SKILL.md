---
name: jobd-outreach
description: Draft cold outreach micro-pitches for companies Harshil just applied to. Use after the apply stage, when he says "draft outreach", "/jobd-outreach", or asks to contact recruiters/EMs at applied companies. Researches contacts, infers emails with a confidence tier, and stages drafts for his approval — never sends.
---

# jobd outreach drafter

For each company Harshil applied to recently, find one or two people worth a
3-sentence cold email and stage a draft. **You never send.** Sending happens only
after Harshil approves on the dashboard.

**API base**: `http://127.0.0.1:8383`

## 1. Find the companies

```
sqlite3 ~/agent/jobd.db "SELECT id, token, title, url FROM jobs WHERE status='applied' AND applied_at > date('now','-2 days')"
```

Skip staffing agencies and any posting where the end client is unnamed.

## 2. Find a person (max 2 per company)

Web search for the engineering manager, tech recruiter, or talent acquisition lead:
`"<company>" engineering manager India site:linkedin.com/in`, the company's team/about
page, their engineering blog, GitHub org members. Prefer someone connected to the role
(the hiring team) over a generic HR mailbox.

Record: name, title, and the source URL you found them on.

## 3. Infer the email + set a confidence tier

Find the company's email pattern from public sources — press/contact pages, GitHub
commit emails on the org's public repos, published papers, conference listings.

- **high** — you found this person's actual address published somewhere, OR you
  confirmed the domain's pattern from ≥2 real addresses and applied it.
- **medium** — the pattern is confirmed but you're applying it to a name you inferred.
- **low** — the pattern is a guess (default to `first@` / `first.last@`).

Be honest about the tier. High-confidence drafts are batch-approvable by Harshil;
lower tiers require an individual click. Overstating confidence is how a Gmail account
gets flagged.

## 4. Write the pitch — the closer voice

Three sentences. Confidence comes from specificity, not adjectives.

1. **A specific signal about their team/role** — something you actually read in the JD
   or on their site. Not "I'm excited about your company."
2. **One proof line from the experience bank** (`~/agent/bank/bank.yaml`) that mirrors
   their problem, with the real metric.
3. **Assume the meeting**: "Worth 15 minutes this week?"

Rules:
- Facts only from the bank and `profile.md`. **Never invent a metric, a system, or a
  claim.** Same no-fabrication rule as the resume.
- Present tense, no hedging ("I was wondering if maybe…" is banned), no flattery, no
  exclamation marks, no emoji.
- Never quote or reference fictional characters, films, or TV in the email.
- Subject line: 4-6 words, concrete. "Feed infra — 10K users, sub-20ms p99" beats
  "Application for Backend Engineer".
- Mention he's already applied, once, briefly. It's context, not a request for a favor.

Good example:

> Subject: Backend SDE-1 application — feed infra
>
> Saw you're hiring an SDE-1 for the feed team at Acme. I currently run a sorted home
> feed serving 10K+ users at sub-20ms p99 on Pub/Sub, Dataflow and Redis, and I
> migrated our message broker from Node to Go to sustain 1M+ RPS. I applied through
> the portal this morning — worth 15 minutes this week?
>
> Harshil

## 5. Stage the draft

```
curl -s -X POST http://127.0.0.1:8383/api/outreach \
  -H 'Content-Type: application/json' \
  -d '{"job_id": 123, "company": "Acme", "person_name": "…", "title": "Engineering Manager",
       "email": "…", "confidence": "medium", "subject": "…", "body": "…"}'
```

A `409` means the domain is blacklisted after a previous bounce — skip it, do not
retry with a different guess at the same domain.

## 6. Report

List what you staged: company, person, confidence, and where you found them. Tell
Harshil to review at `http://127.0.0.1:8383` and that nothing sends until he approves.

Never call `outreach-send` yourself.
