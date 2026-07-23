# Deploying to the Ubuntu laptop

Fresh install. Everything is a static Go binary plus a SQLite file — no runtime deps
except a LaTeX engine (tectonic) and python3+pypdf for the ATS check.

## 1. Build for Linux (on the Mac)

    cd ~/agent
    GOOS=linux GOARCH=amd64 go build -o jobd-linux ./cmd/jobd
    GOOS=linux GOARCH=amd64 go build -o resumegen-linux ./cmd/resumegen

(`modernc.org/sqlite` is pure Go, so this cross-compiles with no CGO.)

## 2. Copy to the laptop

    rsync -av --exclude out --exclude '*.db' --exclude tools \
      ~/agent/ harshil@<laptop>:~/agent/
    rsync -av ~/.config/jobd/ harshil@<laptop>:~/.config/jobd/

On the laptop: rename `jobd-linux` → `jobd-bin`, `resumegen-linux` → `resumegen-bin`,
and `chmod 600 ~/.config/jobd/*`.

Start the database fresh, seeded from the v1 pipeline so the applied/rejected ledger
carries over (this is what prevents re-applying to anything from v1):

    ./jobd-bin seed -db jobd.db -skill <path to v1 SKILL.md>

## 3. Laptop prerequisites

    sudo apt install -y python3-pip poppler-utils
    pip3 install --user pypdf
    curl -fsSL https://drop-sh.fullyjustified.net | sh    # tectonic → ./tectonic
    sudo mv tectonic /usr/local/bin/

**Claude Code** (needed by the apply stage — this is the piece that opens the session):

    curl -fsSL https://claude.ai/install.sh | bash
    claude          # log in once with your Pro account, interactively

**Chrome** must be installed and signed in for the apply stage's browser automation.

## 4. Install the service

    sudo cp deploy/jobd.service /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable --now jobd
    journalctl -u jobd -f          # watch it work

Dashboard: `http://<laptop>:8383` (or over Tailscale). It listens on 0.0.0.0 in the
unit file — keep the laptop off untrusted networks, or bind to the Tailscale IP.

## 5. Gmail sending (one time — one paste, no OAuth client)

Outreach sends via Gmail SMTP with an App Password. This is deliberately NOT the OAuth
path: Google provides no CLI or API to create an OAuth 2.0 client ID (`gcloud iam
oauth-clients` is Workforce Identity Federation — different product, can't do
`gmail.send`), so OAuth would mean console clicking plus a consent screen. The App
Password is one page.

1. 2-Step Verification must be on: https://myaccount.google.com/signinoptions/twosv
2. Create an App Password, app type "Mail": https://myaccount.google.com/apppasswords
3. Add the 16 characters to `~/.config/jobd/env`:

       GMAIL_APP_PASSWORD=abcdefghijklmnop

Test it end to end by mailing yourself:

    ./jobd-bin outreach-send -db jobd.db -dry-run   # preview, sends nothing
    ./jobd-bin outreach-send -db jobd.db            # actually sends approved drafts

The password lives only in `~/.config/jobd/env` (mode 600) and is used for SMTP
submission only. Revoke it any time from the same Google page.

**Alternate (OAuth) path**, if you ever prefer a send-only scoped token: create a
Desktop-app OAuth client in the console, save the JSON to
`~/.config/jobd/gmail-client.json`, run `./jobd-bin auth-gmail`, and jobd will use it
automatically when no App Password is set.

## How the autonomous loop works

    jobd (systemd)
      └─ every 30-60 min (jittered):
           release stale claims → sweep all boards → prefilter → LLM gate (unlimited)
           └─ if anything got shortlisted: exec deploy/run-apply.sh
                ├─ claude -p "follow skills/jobd-apply/SKILL.md"    (applies to ALL of them)
                └─ claude -p "follow skills/jobd-outreach/SKILL.md" (stages drafts only)

So yes — **jobd opens the Claude Code session itself** on each run. Nothing is capped:
every prefilter survivor gets gated, and every shortlisted job gets applied to.

Outreach never sends automatically. Approve drafts on the dashboard, then:

    ./jobd-bin outreach-send -db jobd.db          # or -dry-run to preview

Safety rails on sending: 20/day cap, 30-120s spacing between sends, and a circuit
breaker that refuses to send if the trailing-50 bounce rate exceeds 8%. A bounce
blacklists that domain for every future run.

## Ops

    systemctl status jobd
    journalctl -u jobd -f
    journalctl -u jobd --since "1 hour ago" | grep -E "SHOR|applied|FAIL"
    sqlite3 ~/agent/jobd.db "SELECT applied_at, title, token FROM jobs WHERE status='applied' ORDER BY applied_at DESC LIMIT 20"

Cost: `https://console.cloud.google.com/billing?project=jobd-agent-hs` — expect
low single-digit dollars/month against your $300 credits.
