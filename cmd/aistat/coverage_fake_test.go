//go:build fake

package main

import (
	"context"
	"io"
	"testing"

	"github.com/drogers0/aistat/v2/internal/providers"
)

// TestFakeProvidersCoversKnownIDs is a tripwire for the fake-build variant.
// Runs only under `go test -tags=fake ./cmd/aistat`.
func TestFakeProvidersCoversKnownIDs(t *testing.T) {
	got := map[string]bool{}
	for _, p := range fakeProviders("") {
		got[p.ID()] = true
	}
	for _, id := range providers.KnownProviderIDs {
		if !got[id] {
			t.Errorf("fakeProviders missing provider %q", id)
		}
	}
}

func TestFakeClaudeContextsAreDistinct(t *testing.T) {
	var claude *fakeProvider
	for _, p := range fakeProviders("") {
		if p.ID() == "claude" {
			claude, _ = p.(*fakeProvider)
		}
	}
	if claude == nil {
		t.Fatal("fake Claude provider missing")
	}

	out, err := claude.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("Claude fake account count = %d, want 2", len(out.Accounts))
	}
	first, second := out.Accounts[0], out.Accounts[1]
	if first.Email != "fake@example.com" || second.Email != "fake@example.com" {
		t.Fatalf("fake Claude emails = %q and %q, want same email", first.Email, second.Email)
	}
	if first.Key == second.Key || first.Address == second.Address {
		t.Fatalf("fake Claude contexts are not distinct: keys %q/%q, addresses %q/%q", first.Key, second.Key, first.Address, second.Address)
	}
	if first.Limits["five_hour"].UsedPercent == second.Limits["five_hour"].UsedPercent {
		t.Fatalf("fake Claude limits are not distinct: %v and %v", first.Limits, second.Limits)
	}
	if first.Active == second.Active {
		t.Fatalf("fake Claude active flags = %t/%t, want exactly one active", first.Active, second.Active)
	}
	if first.OrganizationType != "claude_max" || second.OrganizationType != "claude_team" {
		t.Fatalf("fake Claude organization types = %q/%q", first.OrganizationType, second.OrganizationType)
	}

	var codex *fakeProvider
	for _, p := range fakeProviders("") {
		if p.ID() == "codex" {
			codex, _ = p.(*fakeProvider)
		}
	}
	if codex == nil {
		t.Fatal("fake Codex provider missing")
	}
	codexOut, err := codex.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Codex fake Fetch() error = %v", err)
	}
	if len(codexOut.Accounts) != 1 {
		t.Fatalf("Codex fake account count = %d, want 1", len(codexOut.Accounts))
	}
	codexRow := codexOut.Accounts[0]
	if codexRow.Address != "" || codexRow.OrganizationName != "" || codexRow.OrganizationType != "" {
		t.Fatalf("Codex fake organization fields = (%q, %q, %q), want empty", codexRow.Address, codexRow.OrganizationName, codexRow.OrganizationType)
	}
}

type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }

// TestRun_RenderErrorExits3 — when render fails (stdout write error), the CLI
// must return exit code 3, distinguishable from "providers failed" (exit 1).
func TestRun_RenderErrorExits3(t *testing.T) {
	code := run([]string{"--fake"}, failWriter{}, io.Discard)
	if code != 3 {
		t.Errorf("got exit code %d, want 3", code)
	}
}
