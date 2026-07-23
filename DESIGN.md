# Job Pipeline v2 — Design Notes

Architecture blueprint agreed 2026-07-22. Deterministic Go spine (`jobd`) + SQLite on the
always-on Ubuntu laptop; Claude Pro scheduled sessions as the judgment LLM (no API key);
Ollama (Qwen3-4B class) on the 1650 Ti for high-volume JD gating; existing job-applier
skill + Chrome kept unchanged for the apply stage. Develop on the Mac, deploy to the
laptop last (`GOOS=linux go build` + rsync + systemd).

## Component map

| Stage | Runs where | Engine |
|---|---|---|
| Discovery poller (5-ATS registry sweep) | jobd (laptop) | pure Go, no LLM |
| Rules pre-filter (YOE/salary/title/location regex) | jobd | pure Go |
| JD gate (structured extraction from JD body) | jobd → Gemini 2.5 Flash-Lite (GCP $300 credits) | JSON-schema output, temp 0 |
| Scorer (weighted rules over gate output) | jobd | pure Go |
| Resume bullet selection | Claude scheduled session | in-session (subscription) |
| Resume render + ATS verification | jobd / resumegen | Go + tectonic + pypdf |
| Apply | Claude session + Chrome (Mac) | existing job-applier skill |
| Outreach contact discovery | Claude session (web search) + jobd (pattern inference) | mixed |
| Outreach drafting | Claude session | in-session |
| Outreach sending | jobd via Gmail API (Harshil's own OAuth grant) | pure Go |
| Learning loop | outcomes in SQLite; weekly reflection session proposes rule diffs as git commits | Claude (heavy) |

## Friction-point mitigations (agreed 2026-07-22)

### 1. Gmail sender reputation / bounce spikes
- Confidence tiers on inferred emails. **High confidence** (pattern observed on ≥2 real
  addresses at that domain, or previously delivered successfully) → eligible for
  batch-approve autosend. **Anything lower** → requires an individual Approve click on
  the dashboard; dashboard also offers the pitch as copy-paste text for LinkedIn instead.
- Bounce watcher: parse DSN replies (`550 user unknown`, `5.1.1`) from the Gmail sweep;
  on bounce, blacklist that domain's inferred pattern, downgrade all pending contacts at
  the domain, never retry the address.
- Circuit breaker: pause ALL outreach sends if trailing-50 bounce rate exceeds 8%.
- Volume cap: max ~15-20 outreach sends/day with ramp-up from ~5/day in week 1;
  randomized send times within working hours, never bursts.

### 1b. Outreach voice (master-prompt spec for the drafter)
The 3-sentence micro-pitch is written like a closer, not an applicant: (1) open with a
specific signal about THEIR team/role ("Saw you're hiring for the feed team"), (2) one
proof line built from a bank metric that mirrors their problem ("I run a sorted home
feed for 10K+ users at sub-20ms p99 on Dataflow and Redis"), (3) assume the meeting
("Worth 15 minutes this week?"). Confidence comes from specificity — concrete systems
and numbers, present tense, zero hedging ("I was wondering if maybe" is banned), zero
flattery, zero begging, no exclamation marks. Never cite or quote fictional characters
or media in the email itself. Bullets used in the pitch must come from the bank
(same no-fabrication rule as the resume).

### 2. JD-gate model provisioning (decided 2026-07-22)
- Primary: **Gemini 2.5 Flash-Lite via Gemini API**, billed to Harshil's $300 GCP trial
  credits (~$1-3/mo at expected volume). OpenAI-compatible endpoint → `LLMClient`
  backend `gemini`. Credits expire ~90 days out; plan the flip before then.
- Fallback chain: gemini (retry once with validation error) → Claude session queue.
- After credits (or if Gemini degrades): flip config default to `ollama` on the laptop
  (Qwen3-4B/Gemma class) — one config line, no code change. Rejected: always-on GPU VM
  (T4/L4 ≈ $150-500/mo burns the credits in weeks for ~10 min/day of real GPU work);
  acceptable self-hosted alternative if ever wanted: Cloud Run + L4 scale-to-zero
  running Ollama/Gemma (~$5-15/mo, pay-per-second).
- Harshil's setup tasks: enable Gemini API, create API key into `~/.config/jobd/env`
  (GEMINI_API_KEY), set a GCP budget alert (~$20) so nothing bills past the credits.

### 2b. LLM schema drift (applies to gemini AND any local model)
- Always request structured output (Gemini: `response_schema`; Ollama: `format: "json"`);
  temperature 0.
- Post-validate: JSON-schema check + `evidence_quote` must appear verbatim in the JD text
  (whitespace-normalized).
- On failure: retry once with the validation error appended to the prompt; second failure
  → enqueue the JD for the next Claude session (never trust unvalidated local output).
- Track per-model drift rate in SQLite; if >5% of JDs fall through to Claude, revisit
  model choice/prompt.
- Sanitize JD input before prompting: strip HTML, normalize Unicode (NFC), collapse
  whitespace — most drift comes from messy input, not the model.

### 3. ATS rate limits / anti-bot
- Jitter: poll interval uniform-random 30-60 min per ATS host, independently phased;
  randomize company order within a sweep.
- Conditional requests: cache and send `ETag` / `If-Modified-Since` per board endpoint;
  304 costs nothing and most boards change rarely.
- Realistic browser `User-Agent`, `Accept: application/json` (required for
  SmartRecruiters), HTTP/2 via Go's default transport.
- Per-host politeness: serialize requests per host with 1-3s spacing; exponential backoff
  + long cool-off on 429/403; a host that 403s twice in a row gets marked `?` in the
  registry (same convention as v1) instead of hammered.

## Resume compiler contract (Step 1 — this repo)
- `bank/bank.yaml` — the master experience bank. Facts frozen; the LLM never writes
  prose. Style rule: every bullet is one line, clean simple English, ≤105 chars.
- Selection = JSON (`selection.schema.md`): bullet IDs, per-role tech lines, skills-line
  items, optional alias substitutions — all validated against the bank by `resumegen`.
- `resumegen` renders `resume/template.tex` (single column, no tables/graphics, standard
  fonts), compiles with tectonic, then asserts: exactly 1 page AND every selected bullet
  survives `pypdf` text extraction (space-stripped compare) — the ATS-parse guarantee.
- Unknown bullet ID, unknown alias, over-budget section, or failed assert → hard error,
  fall back to baseline resume (baseline = `selections/baseline.json`, mirrors the
  current real resume).
