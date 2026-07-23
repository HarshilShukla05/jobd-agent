package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/harshil/agent/internal/gmail"
	"github.com/harshil/agent/internal/store"
	"golang.org/x/oauth2"
)

// Send policy (DESIGN.md): high-confidence contacts are batch-approvable,
// everything else needs an individual approval click; hard daily cap; and a
// circuit breaker that pauses all sending if recent bounces spike.
const (
	dailySendCap     = 20
	bounceRateLimit  = 0.08
	minSampleForRate = 12
)

// authGmail runs the one-time browser consent flow and stores the refresh token.
func authGmail() {
	cfg, err := gmail.Config()
	if err != nil {
		die("%v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("listen: %v", err)
	}
	defer ln.Close()
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)

	codeCh := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := r.URL.Query().Get("code"); code != "" {
			fmt.Fprintln(w, "jobd: Gmail authorized. You can close this tab.")
			codeCh <- code
			return
		}
		http.Error(w, "no code in callback", 400)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	url := cfg.AuthCodeURL("state", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	fmt.Println("Opening your browser to authorize Gmail send access...")
	fmt.Println("If it doesn't open, visit:\n" + url)
	openBrowser(url)

	select {
	case code := <-codeCh:
		tok, err := cfg.Exchange(context.Background(), code)
		if err != nil {
			die("token exchange: %v", err)
		}
		if err := gmail.SaveToken(tok); err != nil {
			die("save token: %v", err)
		}
		fmt.Println("authorized — token saved to ~/.config/jobd/gmail-token.json")
	case <-time.After(5 * time.Minute):
		die("timed out waiting for authorization")
	}
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	case "linux":
		exec.Command("xdg-open", url).Start()
	}
}

// outreachSend sends every approved draft, enforcing the cap and breaker.
func outreachSend(st *store.Store, from string, dryRun bool) {
	sentToday, bouncedRecent, sentRecent, err := st.SendStats()
	if err != nil {
		die("send stats: %v", err)
	}
	if sentRecent >= minSampleForRate {
		if rate := float64(bouncedRecent) / float64(sentRecent); rate > bounceRateLimit {
			die("CIRCUIT BREAKER: trailing bounce rate %.0f%% exceeds %.0f%% — "+
				"sending paused to protect your Gmail reputation. Review bounced rows first.",
				rate*100, bounceRateLimit*100)
		}
	}
	remaining := dailySendCap - sentToday
	if remaining <= 0 {
		fmt.Printf("daily cap reached (%d sent today) — nothing sent\n", sentToday)
		return
	}

	drafts, err := st.OutreachByStatus("approved", remaining)
	if err != nil {
		die("load approved: %v", err)
	}
	if len(drafts) == 0 {
		fmt.Println("no approved drafts to send (approve them on the dashboard first)")
		return
	}

	ctx := context.Background()
	var sender gmail.Sender
	if !dryRun {
		if sender, err = gmail.NewSender(ctx, from); err != nil {
			die("gmail: %v", err)
		}
	}

	sent := 0
	for _, o := range drafts {
		if dryRun {
			fmt.Printf("[dry-run] would send to %s <%s> (%s)\n  subject: %s\n  %s\n\n",
				o.PersonName, o.Email, o.Confidence, o.Subject, o.Body)
			continue
		}
		id, err := sender.Send(ctx, o.Email, o.Subject, o.Body)
		if err != nil {
			if err := st.SetOutreachStatus(o.ID, "approved", "send failed: "+err.Error()); err != nil {
				die("record failure: %v", err)
			}
			fmt.Printf("FAIL  %s: %v\n", o.Email, err)
			continue
		}
		if err := st.SetOutreachStatus(o.ID, "sent", "gmail id "+id); err != nil {
			die("record sent: %v", err)
		}
		sent++
		fmt.Printf("SENT  %-40s %s\n", o.Email, o.Subject)
		// spread sends out — never burst
		time.Sleep(time.Duration(30+randInt(90)) * time.Second)
	}
	if !dryRun {
		fmt.Printf("\nsent %d (%d/%d today)\n", sent, sentToday+sent, dailySendCap)
	}
}
