#!/usr/bin/env bash
# Invoked by jobd after each gate cycle when the shortlist is non-empty.
# Runs a headless Claude session that follows the apply skill, then drafts outreach.
# Both stages are best-effort: a failure here must never stop discovery.
set -uo pipefail
# resolve the repo from this script's own location so it works on both the
# Mac (/Users/harshil/agent) and the laptop (/home/harshil/agent)
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "=== apply stage $(date -u +%FT%TZ) ==="
claude -p "Read skills/jobd-apply/SKILL.md and follow it exactly. Work the entire apply queue — there is no cap. Report what you applied to, what you skipped, and why." 2>&1

echo "=== outreach drafting $(date -u +%FT%TZ) ==="
claude -p "Read skills/jobd-outreach/SKILL.md and follow it exactly. Draft outreach for every company applied to in the last 2 days. Stage drafts only — never send." 2>&1

echo "=== apply+outreach cycle done $(date -u +%FT%TZ) ==="
