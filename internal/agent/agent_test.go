package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeCLI drops an executable shell script named `name` into dir.
func fakeCLI(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func marker(dir, name string) string { return filepath.Join(dir, name+".ran") }

func TestFailoverOnSpendLimit(t *testing.T) {
	// Reproduces the 2026-07-23 incident verbatim: claude reports a monthly
	// spend limit and exits 1. Codex must take over.
	dir := t.TempDir()
	fakeCLI(t, dir, "claude",
		`echo "You've hit your monthly spend limit · raise it at claude.ai/settings"; touch `+marker(dir, "claude")+`; exit 1`)
	fakeCLI(t, dir, "codex",
		`touch `+marker(dir, "codex")+`; echo done; exit 0`)
	t.Setenv("PATH", dir+":/usr/bin:/bin") // fakes first; coreutils still reachable

	r := &Runner{Backend: Auto, Primary: Claude, Dir: dir}
	if err := r.Run(context.Background(), "test", "prompt", func() bool { return false }); err != nil {
		t.Fatalf("expected failover to codex to succeed, got: %v", err)
	}
	if _, err := os.Stat(marker(dir, "codex")); err != nil {
		t.Fatal("codex was never invoked — failover did not happen")
	}
}

func TestFailoverOnZeroProgressCrash(t *testing.T) {
	// Primary dies with an unrecognized error but touched nothing — the
	// evidence-based check must still fail over.
	dir := t.TempDir()
	fakeCLI(t, dir, "claude", `echo "segfault or some novel error"; exit 1`)
	fakeCLI(t, dir, "codex", `touch `+marker(dir, "codex")+`; exit 0`)
	t.Setenv("PATH", dir+":/usr/bin:/bin") // fakes first; coreutils still reachable

	r := &Runner{Backend: Auto, Primary: Claude, Dir: dir}
	if err := r.Run(context.Background(), "test", "prompt", func() bool { return false }); err != nil {
		t.Fatalf("expected zero-progress failover to succeed, got: %v", err)
	}
	if _, err := os.Stat(marker(dir, "codex")); err != nil {
		t.Fatal("codex was never invoked despite zero progress")
	}
}

func TestNoFailoverAfterPartialProgress(t *testing.T) {
	// Primary did real work (progressed=true) then failed: do NOT burn the
	// second backend; the claims system makes a next-cycle retry safe.
	dir := t.TempDir()
	fakeCLI(t, dir, "claude", `echo "browser crashed mid-form"; exit 1`)
	fakeCLI(t, dir, "codex", `touch `+marker(dir, "codex")+`; exit 0`)
	t.Setenv("PATH", dir+":/usr/bin:/bin") // fakes first; coreutils still reachable

	r := &Runner{Backend: Auto, Primary: Claude, Dir: dir}
	err := r.Run(context.Background(), "test", "prompt", func() bool { return true })
	if err == nil {
		t.Fatal("expected an error after partial-progress failure")
	}
	if _, statErr := os.Stat(marker(dir, "codex")); statErr == nil {
		t.Fatal("codex ran — partial progress must not fail over")
	}
}

func TestQuotaOnBothBackends(t *testing.T) {
	dir := t.TempDir()
	fakeCLI(t, dir, "claude", `echo "usage limit reached"; exit 1`)
	fakeCLI(t, dir, "codex", `echo "You have hit your usage limit. Try again later."; exit 1`)
	t.Setenv("PATH", dir+":/usr/bin:/bin") // fakes first; coreutils still reachable

	r := &Runner{Backend: Auto, Primary: Claude, Dir: dir}
	if err := r.Run(context.Background(), "test", "prompt", func() bool { return false }); err == nil {
		t.Fatal("expected error when both backends are out of quota")
	}
}
