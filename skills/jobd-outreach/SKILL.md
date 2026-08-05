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

## 4. Write the pitch

### The principle: SELECT relevance, never manufacture it

The evidence (see `RESEARCH.md` §5) settles a question worth understanding, because it
decides every sentence you write.

Generic outreach gets under 1% replies; genuinely relevant outreach gets 15-18%. So
"here is why I am a good engineer, I would do well anywhere" loses — even though it is
true. But the fix is NOT researching the company harder. Superficial personalisation
("loved your blog post") now reads as automated and performs worse than saying nothing,
because AI-written flattery has flooded every inbox.

The resolution: **the proof is company-agnostic, the selection is company-specific.**

Harshil's competence is portable — that is the honest fact. He also has ~80 real
bullets. So the personalised part is not "I studied you", it is "of everything I have
built, THIS is the piece that matters to you." Nothing is invented; something is chosen.

Two consequences, and both matter:

- **You are never blocked by a thin JD.** The material comes from his side, not theirs.
  A vague posting still tells you the domain, and the domain tells you which of his
  bullets to lead with.
- **It is a signal that cannot be faked.** Anyone can praise a blog post. Only someone
  who has actually solved that shape of problem can select a matching bullet. That is
  what makes it land.

### Choosing what to lead with, when they gave you little

Reason from what the company DOES to what they must be solving, then pick his closest
real work. Never assert a flaw in their system — say it as a shape of problem, not a
diagnosis of their code.

  - booking / scheduling → double-booking under concurrent writes, reminders, no-shows
  - insurance / lending → state machines, workflows that must not lose an event,
    idempotent money movement, late webhooks, reconciliation
  - marketplace → order state across parties, fan-out, partial-delivery reconciliation
  - social / feed → ranking freshness, fan-out cost, read latency at p99
  - realtime / chat → delivery guarantees, reconnect replay, presence

### Write it for the FORWARD, not for the read

A recruiter's own win condition is submitting someone the hiring manager approves. Make
that easy and you are serving their interest, not asking a favour: quantified, specific,
and copy-pasteable into an internal message without editing.

### Shape

100-150 words. Three or four short sentences, or two plus two bullets.

1. **The role, and the problem shape it implies.** One line.
2. **One or two proof lines** from `bank/bank.yaml`, with the real metric.
3. **Assume the meeting**: "Worth 15 minutes this week?"

Rules:
- Facts only from the bank and `profile.md`. **Never invent a metric, a system, or a
  claim.** Same no-fabrication rule as the resume.
- **Never mention `recruiter_safe: false` projects** (the job agent, Rezume). A
  job-search tool shown to the person you are applying to invites the conclusion that
  the message itself was generated.
- Present tense, no hedging ("I was wondering if maybe…" is banned), no flattery, no
  exclamation marks, no emoji.
- Never quote or reference fictional characters, films, or TV.
- Never state a weakness, a gap, or an apology. Omission is honest; self-criticism on
  an opening message is not (same rule as the bank header).
- Subject line: 4-6 words, concrete. "Feed infra — 10K users, sub-20ms p99" beats
  "Application for Backend Engineer".
- Mention he applied, once, briefly. Context, not a request for a favour.

Good example:

> Subject: Backend SDE-1 application — feed infra
>
> Saw you're hiring an SDE-1 for the feed team at Acme. Feed systems get hard at the
> read path, which is most of what I do right now: I run a sorted home feed for 10K+
> users at sub-20ms p99 on Pub/Sub, Dataflow and Redis, and I moved our message broker
> from Node to Go to sustain 1M+ RPS. I applied through the portal this morning —
> worth 15 minutes this week?
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
