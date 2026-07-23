// Package agent runs the downstream stages (apply, outreach drafting) by
// invoking a coding agent CLI in headless mode. jobd owns this directly —
// there is no wrapper script to keep in sync.
//
// Two backends are supported so an exhausted subscription limit never stalls
// the pipeline: Claude Code (`claude -p`) and Codex (`codex exec`). In auto
// mode the primary runs first and a QUOTA failure — and only a quota failure —
// fails over to the other. Any other error is reported, not retried on the
// other backend's quota.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

type Backend string

const (
	Claude Backend = "claude"
	Codex  Backend = "codex"
	Auto   Backend = "auto"
)

// quotaRe matches the ways either CLI reports "you are out of usage".
// Learned the hard way: Claude's actual message was "You've hit your monthly
// spend limit", which the first version of this regex missed.
var quotaRe = regexp.MustCompile(`(?i)spend limit|usage limit|monthly limit|weekly limit|session limit|rate.?limit|quota|too many requests|\b429\b|out of credits?|credit balance|insufficient_quota|limit reached|hit your .{0,20}limit|upgrade to continue|resets at`)

type Runner struct {
	Backend Backend // auto | claude | codex
	Primary Backend // which one auto tries first
	Dir     string  // repo root the agent runs in
}

// Available reports whether a backend's CLI is installed.
func Available(b Backend) bool {
	_, err := exec.LookPath(string(b))
	return err == nil
}

type result int

const (
	ok result = iota
	quotaExhausted
	failed
)

// Run executes one stage prompt, failing over between backends.
//
// Failover policy (reliability-first): the secondary runs whenever the
// primary hit a quota wall OR died without making any progress (progressed
// reports whether the stage changed real state, e.g. jobs left the queue).
// The only case that does NOT fail over is a backend that did real work and
// then failed — rerunning that on a second quota could double-spend effort
// on a systemic fault, and the atomic claims make a retry next cycle safe.
func (r *Runner) Run(ctx context.Context, label, prompt string, progressed func() bool) error {
	fmt.Printf("=== %s ===\n", label)
	if progressed == nil {
		progressed = func() bool { return false }
	}

	if r.Backend != Auto {
		if !Available(r.Backend) {
			return fmt.Errorf("%s is not installed", r.Backend)
		}
		res, err := r.exec(ctx, r.Backend, prompt)
		if res == ok {
			return nil
		}
		return err
	}

	primary := r.Primary
	if primary == "" {
		primary = Claude
	}
	secondary := Codex
	if primary == Codex {
		secondary = Claude
	}

	var lastErr error
	for i, b := range []Backend{primary, secondary} {
		if !Available(b) {
			fmt.Printf("--- %s not installed, skipping\n", b)
			continue
		}
		fmt.Printf("--- backend: %s\n", b)
		res, err := r.exec(ctx, b, prompt)
		lastErr = err
		if res == ok {
			return nil
		}
		if i == 1 {
			break // secondary already ran; nothing left to fail over to
		}
		switch {
		case res == quotaExhausted:
			fmt.Printf("--- %s is out of quota, failing over to %s\n", b, secondary)
		case !progressed():
			fmt.Printf("--- %s failed before doing any work (%v), failing over to %s\n", b, err, secondary)
		default:
			return fmt.Errorf("%s failed after partial progress — not failing over, "+
				"remaining jobs retry next cycle: %w", b, err)
		}
	}
	if lastErr != nil {
		return fmt.Errorf("all backends failed: %w", lastErr)
	}
	return fmt.Errorf("no backend available (install claude or codex)")
}

func (r *Runner) exec(ctx context.Context, b Backend, prompt string) (result, error) {
	var cmd *exec.Cmd
	switch b {
	case Claude:
		cmd = exec.CommandContext(ctx, "claude", "-p", prompt)
	case Codex:
		// Codex sandboxes shell commands with NO network by default, which
		// silently breaks every curl to the jobd API — the stage "runs" and
		// applies to nothing. workspace-write + network_access is required.
		cmd = exec.CommandContext(ctx, "codex", "exec",
			"--skip-git-repo-check",
			"-s", "workspace-write",
			"-c", "sandbox_workspace_write.network_access=true",
			prompt)
	default:
		return failed, fmt.Errorf("unknown backend %q", b)
	}
	cmd.Dir = r.Dir

	// stream to our stdout (systemd journal) while keeping a copy to classify
	var buf strings.Builder
	cmd.Stdout = io2{os.Stdout, &buf}
	cmd.Stderr = io2{os.Stderr, &buf}

	err := cmd.Run()
	if err == nil {
		return ok, nil
	}
	if quotaRe.MatchString(buf.String()) {
		return quotaExhausted, err
	}
	return failed, err
}

// io2 tees writes to two destinations.
type io2 struct{ a, b interface{ Write([]byte) (int, error) } }

func (w io2) Write(p []byte) (int, error) {
	n, err := w.a.Write(p)
	w.b.Write(p)
	return n, err
}

// Stage prompts. The SKILL.md files hold the actual procedure; these just
// point the agent at them.
const (
	ApplyPrompt = "Read skills/jobd-apply/SKILL.md and follow it exactly. " +
		"Work the entire apply queue — there is no cap. " +
		"Report what you applied to, what you skipped, and why."

	OutreachPrompt = "Read skills/jobd-outreach/SKILL.md and follow it exactly. " +
		"Draft outreach for every company applied to in the last 2 days. " +
		"Stage drafts only — never send."
)
