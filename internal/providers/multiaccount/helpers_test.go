package multiaccount

import (
	"errors"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/providers"
)

// ── SortAccountResults ───────────────────────────────────────────────────────

func TestSortAccountResults(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"active first", func(t *testing.T) {
			results := []providers.AccountResult{
				{Email: "beta@example.com", Active: false},
				{Email: "alpha@example.com", Active: true},
			}
			SortAccountResults(results)
			if !results[0].Active {
				t.Errorf("sorted[0] should be active; got email=%q", results[0].Email)
			}
			if results[1].Active {
				t.Errorf("sorted[1] should not be active; got email=%q", results[1].Email)
			}
		}},
		{"email ascending among inactive", func(t *testing.T) {
			results := []providers.AccountResult{
				{Email: "zeta@example.com", Active: false},
				{Email: "alpha@example.com", Active: false},
				{Email: "mu@example.com", Active: false},
			}
			SortAccountResults(results)
			for i := 1; i < len(results); i++ {
				if results[i-1].Email >= results[i].Email {
					t.Errorf("not sorted: results[%d].Email=%q >= results[%d].Email=%q",
						i-1, results[i-1].Email, i, results[i].Email)
				}
			}
		}},
		{"key tiebreak for equal emails", func(t *testing.T) {
			// Equal-email rows use their opaque key as the deterministic tie-break.
			results := []providers.AccountResult{
				{Email: "same@example.com", Key: "second", Active: false},
				{Email: "same@example.com", Key: "first", Active: false},
			}
			SortAccountResults(results)
			if results[0].Key != "first" {
				t.Errorf("key tie-break = %q, want first", results[0].Key)
			}
		}},
		{"active before email sort", func(t *testing.T) {
			results := []providers.AccountResult{
				{Email: "zeta@example.com", Active: false},
				{Email: "alpha@example.com", Active: false},
				{Email: "beta@example.com", Active: true},
			}
			SortAccountResults(results)
			if !results[0].Active {
				t.Errorf("active account should be first; got email=%q active=%v", results[0].Email, results[0].Active)
			}
			if results[1].Email >= results[2].Email {
				t.Errorf("inactive accounts not sorted by email: %q >= %q", results[1].Email, results[2].Email)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── RecordFetchOutcome ───────────────────────────────────────────────────────

func TestRecordFetchOutcome(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"success", func(t *testing.T) {
			limits := map[string]providers.Limit{"five_hour": {UsedPercent: 50}}
			ar := providers.AccountResult{}
			ok, trans := RecordFetchOutcome(&ar, limits, nil)
			if !ok {
				t.Error("success=false, want true")
			}
			if trans {
				t.Error("transient=true on success, want false")
			}
			if ar.Limits == nil {
				t.Error("Limits not set on success")
			}
			if ar.Error != "" {
				t.Errorf("Error set on success: %q", ar.Error)
			}
		}},
		{"non transient error", func(t *testing.T) {
			ar := providers.AccountResult{}
			err := errors.New("some permanent error")
			ok, trans := RecordFetchOutcome(&ar, nil, err)
			if ok {
				t.Error("success=true on error, want false")
			}
			if trans {
				t.Error("transient=true for non-transient error, want false")
			}
			if ar.Error == "" {
				t.Error("Error not set on failure")
			}
		}},
		{"transient error", func(t *testing.T) {
			ar := providers.AccountResult{}
			err := providers.ErrTransient
			ok, trans := RecordFetchOutcome(&ar, nil, err)
			if ok {
				t.Error("success=true on transient error, want false")
			}
			if !trans {
				t.Error("transient=false for ErrTransient, want true")
			}
			if ar.Error == "" {
				t.Error("Error not set on transient failure")
			}
		}},
		{"auth denied not transient", func(t *testing.T) {
			ar := providers.AccountResult{}
			err := providers.ErrAuthDenied
			ok, trans := RecordFetchOutcome(&ar, nil, err)
			if ok {
				t.Error("success=true on auth-denied, want false")
			}
			if trans {
				t.Error("auth-denied classified as transient, want false")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── RecomputeResetAfter ──────────────────────────────────────────────────────

func TestRecomputeResetAfter(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"returns new map", func(t *testing.T) {
			now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
			resetsAt := now.Add(time.Hour)
			input := map[string]providers.Limit{
				"five_hour": {ResetsAt: resetsAt, ResetAfterSeconds: 9999},
			}
			out := RecomputeResetAfter(input, now)
			if &out == &input {
				t.Error("RecomputeResetAfter returned same map pointer, want a new map")
			}
		}},
		{"does not mutate input", func(t *testing.T) {
			now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
			resetsAt := now.Add(time.Hour)
			input := map[string]providers.Limit{
				"five_hour": {ResetsAt: resetsAt, ResetAfterSeconds: 9999},
			}
			_ = RecomputeResetAfter(input, now)
			if input["five_hour"].ResetAfterSeconds != 9999 {
				t.Errorf("input mutated: ResetAfterSeconds = %d, want 9999", input["five_hour"].ResetAfterSeconds)
			}
		}},
		{"clamps negative to zero", func(t *testing.T) {
			now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
			// ResetsAt is in the past.
			resetsAt := now.Add(-time.Hour)
			input := map[string]providers.Limit{
				"five_hour": {ResetsAt: resetsAt, ResetAfterSeconds: 3600},
			}
			out := RecomputeResetAfter(input, now)
			if got := out["five_hour"].ResetAfterSeconds; got != 0 {
				t.Errorf("negative clamp: got %d, want 0", got)
			}
		}},
		{"preserves resets at", func(t *testing.T) {
			now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
			resetsAt := now.Add(time.Hour)
			input := map[string]providers.Limit{
				"five_hour": {ResetsAt: resetsAt, ResetAfterSeconds: 0},
			}
			out := RecomputeResetAfter(input, now)
			if !out["five_hour"].ResetsAt.Equal(resetsAt) {
				t.Errorf("ResetsAt not preserved: got %v, want %v", out["five_hour"].ResetsAt, resetsAt)
			}
		}},
		{"recomputes from resets at", func(t *testing.T) {
			now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
			resetsAt := now.Add(2 * time.Hour)
			input := map[string]providers.Limit{
				"five_hour": {ResetsAt: resetsAt, ResetAfterSeconds: 0},
			}
			out := RecomputeResetAfter(input, now)
			wantSecs := int(resetsAt.Sub(now).Seconds())
			if got := out["five_hour"].ResetAfterSeconds; got != wantSecs {
				t.Errorf("ResetAfterSeconds: got %d, want %d", got, wantSecs)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── Budget ───────────────────────────────────────────────────────────────────

func TestBudget(t *testing.T) {
	tests := []struct {
		name       string
		base       time.Duration
		perAccount time.Duration
		count      int
		want       time.Duration
	}{
		{"zero accounts", 5 * time.Second, 15 * time.Second, 0, 5 * time.Second},
		{"multiple accounts", 5 * time.Second, 15 * time.Second, 3, 5*time.Second + 3*15*time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Budget(tt.base, tt.perAccount, tt.count)
			if got != tt.want {
				t.Errorf("Budget(%v, %v, %d) = %v, want %v", tt.base, tt.perAccount, tt.count, got, tt.want)
			}
		})
	}
}
