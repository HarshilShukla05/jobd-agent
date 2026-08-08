#!/usr/bin/env bash
# One-off job: build a tailored resume for a single JD, outside the sweep/gate pipeline.
#
# The pipeline exists for volume. This is for the other case — Harshil is handed one
# posting (a link, a forwarded JD, a referral) and wants the same output the pipeline
# produces: a one-page tailored PDF plus the ATS and 7-second-skim report.
#
#   scripts/oneoff.sh <slug> [jd-url]
#
#   scripts/oneoff.sh bjak-backend                    # jds/bjak-backend.txt already written
#   scripts/oneoff.sh acme-payments https://...       # fetch the JD first, then build
#
# Inputs it expects:
#   jds/<slug>.txt          the job description as plain text
#   selections/<slug>.json  which bank bullets to use — this is the judgment call,
#                           made by Claude reading the JD against bank.yaml
#
# Output: out/Harshil_Shukla_<Slug>.pdf, plus the screening report on stdout.
set -euo pipefail

cd "$(dirname "$0")/.."

slug="${1:-}"
url="${2:-}"

if [ -z "$slug" ]; then
  echo "usage: scripts/oneoff.sh <slug> [jd-url]" >&2
  exit 2
fi

jd="jds/${slug}.txt"
sel="selections/${slug}.json"

# Fetch the JD if a URL was given and we don't already have the text.
#
# Prefer the JobPosting JSON-LD block that most boards embed for Google Jobs: it
# carries the description verbatim plus the req id, title and location, which the
# rendered page often buries. Target's careers site is the case that forced this —
# naive tag-stripping there returned the description with its HTML still escaped,
# so the tags survived into the JD text and polluted the keyword match. Falls back
# to stripping the page body when there is no JSON-LD.
if [ -n "$url" ] && [ ! -f "$jd" ]; then
  echo "fetching $url -> $jd"
  tmp="$(mktemp -t oneoff).html"
  trap 'rm -f "$tmp"' EXIT
  curl -fsSL --max-time 30 -A 'Mozilla/5.0' "$url" -o "$tmp" || {
    echo "error: fetch failed — paste the JD into $jd by hand" >&2; exit 1; }
  python3 - "$jd" "$tmp" <<'PY'
import html, json, re, sys
jd, src = sys.argv[1], sys.argv[2]
raw = open(src, encoding='utf-8', errors='replace').read()

def detag(s):
    for _ in range(3):                       # unescape FIRST, and repeatedly:
        n = html.unescape(s)                 # boards double-escape their HTML
        if n == s: break
        s = n
    s = re.sub(r'<(br|/p|/li|/div|/h\d)[^>]*>', '\n', s)
    s = re.sub(r'<[^>]+>', '', s)
    s = re.sub(r'[ \t]+', ' ', s)
    return "\n".join(l.strip() for l in s.split('\n') if l.strip())

post = None
for m in re.finditer(r'<script[^>]*application/ld\+json[^>]*>(.*?)</script>', raw, re.S):
    try: data = json.loads(m.group(1))
    except Exception: continue
    for o in (data if isinstance(data, list) else [data]):
        if isinstance(o, dict) and o.get('@type') == 'JobPosting':
            post = o

if post and post.get('description'):
    addr = (post.get('jobLocation') or {}).get('address') or {}
    head = "%s — %s, %s\nReq ID: %s   Posted: %s   Type: %s\n" % (
        (post.get('title') or '').strip(),
        addr.get('addressLocality', ''), addr.get('addressCountry', ''),
        post.get('identifier', 'n/a'), post.get('datePosted', 'n/a'),
        post.get('employmentType', 'n/a'))
    body = detag(post['description'])
else:
    head = ''
    body = detag(re.sub(r'<script[^>]*>.*?</script>', '', raw, flags=re.S))

if len(body) < 400:
    sys.exit("error: fetched nothing usable — paste the JD into %s by hand" % jd)
open(jd, 'w', encoding='utf-8').write(head + "\n" + body + "\n")
print("req id / header:", head.strip().replace("\n", " | ") or "(none — no JSON-LD)")
PY
fi

[ -f "$jd" ]  || { echo "error: missing $jd — write the JD text there first" >&2; exit 1; }
[ -f "$sel" ] || { echo "error: missing $sel — Claude picks the bullets; see skills/jobd-apply/SKILL.md" >&2; exit 1; }

# Title-case the slug for the filename: bjak-backend -> BJAK? no — Bjak_Backend.
# Keep it simple and predictable; rename with -name if you want something else.
pretty="$(echo "$slug" | tr '-' ' ' | awk '{for(i=1;i<=NF;i++) $i=toupper(substr($i,1,1)) substr($i,2)}1' | tr ' ' '_')"

# Rebuild only if the source is newer than the binary — the pipeline has been run
# with a stale binary before, and a silently outdated resume is worse than a slow one.
if [ -z "$(find ./cmd ./internal -name '*.go' -newer resumegen-bin 2>/dev/null | head -1)" ]; then
  :
else
  echo "resumegen-bin is stale; rebuilding"
  go build -o resumegen-bin ./cmd/resumegen
fi

exec ./resumegen-bin \
  -selection "$sel" \
  -jd "$jd" \
  -name "Harshil_Shukla_${pretty}"
