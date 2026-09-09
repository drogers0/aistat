package providers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLimitMarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"utc timestamp", func(t *testing.T) {
			at, _ := time.Parse(time.RFC3339, "2026-05-26T20:00:00Z")
			l := Limit{UsedPercent: 2, RemainingPercent: 98, ResetsAt: at, ResetAfterSeconds: 17625}
			b, err := json.Marshal(l)
			if err != nil {
				t.Fatal(err)
			}
			got := string(b)
			want := `{"used_percent":2,"remaining_percent":98,"resets_at":"2026-05-26T20:00:00+00:00","reset_after_seconds":17625}`
			if got != want {
				t.Fatalf("got %q\nwant %q", got, want)
			}
		}},
		{"non-utc normalized to +00:00", func(t *testing.T) {
			// A non-UTC input must be normalized to "+00:00". The JSON contract
			// documented in README.md is "ISO 8601, always +00:00 for UTC, never Z";
			// MarshalJSON enforces .UTC() at the boundary so a future provider that
			// forgets to call .UTC() does not silently leak a local offset.
			est := time.FixedZone("EST", -5*3600)
			at := time.Date(2026, 5, 26, 15, 0, 0, 0, est) // 20:00 UTC
			l := Limit{ResetsAt: at}
			b, _ := json.Marshal(l)
			if !strings.Contains(string(b), `"resets_at":"2026-05-26T20:00:00+00:00"`) {
				t.Fatalf("non-UTC ResetsAt must be normalized to +00:00; got %s", string(b))
			}
		}},
		{"field order", func(t *testing.T) {
			at, _ := time.Parse(time.RFC3339, "2026-05-26T20:00:00Z")
			l := Limit{UsedPercent: 2.34, RemainingPercent: 97.66, ResetsAt: at, ResetAfterSeconds: 1}
			b, _ := json.Marshal(l)
			s := string(b)
			idxUsed := strings.Index(s, "used_percent")
			idxRem := strings.Index(s, "remaining_percent")
			idxResets := strings.Index(s, "resets_at")
			idxAfter := strings.Index(s, "reset_after_seconds")
			if !(idxUsed < idxRem && idxRem < idxResets && idxResets < idxAfter) {
				t.Fatalf("field order wrong: %s", s)
			}
			if !strings.Contains(s, `"used_percent":2.34`) {
				t.Fatalf("fractional percent lost precision: %s", s)
			}
		}},
		{"whole number has no decimal", func(t *testing.T) {
			l := Limit{UsedPercent: 2.0, ResetsAt: time.Unix(1, 0)}
			b, _ := json.Marshal(l)
			if !strings.Contains(string(b), `"used_percent":2,`) {
				t.Fatalf("expected used_percent:2 (no .0), got %s", string(b))
			}
		}},
		{"rounds float artifact", func(t *testing.T) {
			l := Limit{UsedPercent: 67.339999999, RemainingPercent: 32.660000001, ResetsAt: time.Unix(1, 0)}
			b, _ := json.Marshal(l)
			s := string(b)
			if !strings.Contains(s, `"used_percent":67.34`) {
				t.Errorf("float artifact not rounded; got %s", s)
			}
			if !strings.Contains(s, `"remaining_percent":32.66`) {
				t.Errorf("float artifact not rounded; got %s", s)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestReportMarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"utc timestamp", func(t *testing.T) {
			at, _ := time.Parse(time.RFC3339, "2026-05-26T20:00:00Z")
			r := Report{CheckedAt: at, Providers: map[string]ProviderResult{}}
			b, _ := json.Marshal(r)
			if !strings.Contains(string(b), `"checked_at":"2026-05-26T20:00:00+00:00"`) {
				t.Fatalf("got %s", string(b))
			}
			if strings.Count(string(b), "checked_at") != 1 {
				t.Fatalf("checked_at appeared more than once: %s", string(b))
			}
		}},
		{"non-utc normalized to +00:00", func(t *testing.T) {
			est := time.FixedZone("EST", -5*3600)
			at := time.Date(2026, 5, 26, 15, 0, 0, 0, est) // 20:00 UTC
			r := Report{CheckedAt: at, Providers: map[string]ProviderResult{}}
			b, _ := json.Marshal(r)
			if !strings.Contains(string(b), `"checked_at":"2026-05-26T20:00:00+00:00"`) {
				t.Fatalf("non-UTC CheckedAt must be normalized to +00:00; got %s", string(b))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestProviderResultOmitempty_AccountsBranch(t *testing.T) {
	at, _ := time.Parse(time.RFC3339, "2026-05-26T20:00:00Z")
	accounts := []AccountResult{{Email: "a@example.com", UUID: "u1", Active: true}}
	limits := map[string]Limit{"five_hour": {ResetsAt: at}}

	// Helper: does this provider-level JSON object carry a top-level limits key?
	hasTopLimits := func(b []byte) bool {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		_, ok := m["limits"]
		return ok
	}

	// Accounts set → top-level "limits" key must be absent; the active-row
	// limits live in accounts[i].limits, so a top-level mirror would duplicate.
	b, _ := json.Marshal(ProviderResult{Accounts: accounts})
	if hasTopLimits(b) {
		t.Fatalf("expected no top-level limits key when Accounts set; got %s", b)
	}

	// Accounts set even with non-nil Limits → still no top-level limits.
	b, _ = json.Marshal(ProviderResult{Limits: limits, Accounts: accounts})
	if hasTopLimits(b) {
		t.Fatalf("expected no top-level limits key when Accounts set (even with non-nil Limits); got %s", b)
	}
	var asObj map[string]json.RawMessage
	_ = json.Unmarshal(b, &asObj)
	if _, ok := asObj["accounts"]; !ok {
		t.Fatalf("expected accounts key; got %s", b)
	}

	// Accounts nil, Limits set → limits present, accounts absent (legacy path
	// for Codex/Copilot and the Claude no-accounts error case).
	b, _ = json.Marshal(ProviderResult{Limits: limits})
	if !hasTopLimits(b) {
		t.Fatalf("expected limits key on legacy path; got %s", b)
	}
	if strings.Contains(string(b), `"accounts"`) {
		t.Fatalf("expected no accounts key on legacy path; got %s", b)
	}
}

func TestAccountResult(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"active false serialized", func(t *testing.T) {
			b, _ := json.Marshal(AccountResult{Active: false})
			if !strings.Contains(string(b), `"active":false`) {
				t.Fatalf("active:false must serialize (omitempty must NOT be on the bool); got %s", b)
			}
		}},
		{"field order", func(t *testing.T) {
			at, _ := time.Parse(time.RFC3339, "2026-05-26T20:00:00Z")
			a := AccountResult{
				Email:  "a@example.com",
				UUID:   "u1",
				Plan:   "Max 5x",
				Active: true,
				Limits: map[string]Limit{"x": {ResetsAt: at}},
				Error:  "oops",
			}
			b, _ := json.Marshal(a)
			s := string(b)
			if strings.Contains(s, `"uuid"`) {
				t.Fatalf("UUID must not appear in JSON (json:\"-\"); got %s", s)
			}
			idxEmail := strings.Index(s, "email")
			idxPlan := strings.Index(s, "plan")
			idxActive := strings.Index(s, "active")
			idxLimits := strings.Index(s, "limits")
			idxError := strings.Index(s, "error")
			if !(idxEmail < idxPlan && idxPlan < idxActive && idxActive < idxLimits && idxLimits < idxError) {
				t.Fatalf("AccountResult field order wrong; got %s", s)
			}
		}},
		{"claude context fields are additive and codex fields omit", func(t *testing.T) {
			claudeJSON, err := json.Marshal(AccountResult{
				Email:            "me@example.com",
				UUID:             "9f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c",
				Key:              "9f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042",
				Address:          "me@example.com/personal-9f2a41c7",
				OrganizationName: "me@example.com's Organization",
				OrganizationType: "claude_max",
				Plan:             "default_claude_max_5x",
				Active:           true,
				Limits:           map[string]Limit{},
			})
			if err != nil {
				t.Fatal(err)
			}
			const wantClaude = "{\"email\":\"me@example.com\",\"address\":\"me@example.com/personal-9f2a41c7\",\"organization_name\":\"me@example.com's Organization\",\"organization_type\":\"claude_max\",\"plan\":\"default_claude_max_5x\",\"active\":true,\"limits\":{}}"
			if string(claudeJSON) != wantClaude {
				t.Fatalf("Claude JSON = %s, want %s", claudeJSON, wantClaude)
			}

			codexJSON, err := json.Marshal(AccountResult{Email: "codex@example.com", UUID: "uuid-codex", Key: "uuid-codex", Active: true, Limits: map[string]Limit{}})
			if err != nil {
				t.Fatal(err)
			}
			const wantCodex = "{\"email\":\"codex@example.com\",\"plan\":\"\",\"active\":true,\"limits\":{}}"
			if string(codexJSON) != wantCodex {
				t.Fatalf("Codex JSON = %s, want %s", codexJSON, wantCodex)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestProviderResult_LimitsAlwaysEmitted(t *testing.T) {
	// Failure case: nil limits, error populated → "limits":null, error key
	// present.
	b, _ := json.Marshal(ProviderResult{Error: "boom"})
	if got := string(b); got != `{"limits":null,"error":"boom"}` {
		t.Fatalf("failure shape wrong, got %s", got)
	}
	// Success case: zero limits, no error → "limits":{} and no error key.
	b, _ = json.Marshal(ProviderResult{Limits: map[string]Limit{}})
	if got := string(b); got != `{"limits":{}}` {
		t.Fatalf("success-empty shape wrong, got %s", got)
	}
	// Success with a populated limit → no error key.
	at, _ := time.Parse(time.RFC3339, "2026-05-26T20:00:00Z")
	b, _ = json.Marshal(ProviderResult{Limits: map[string]Limit{"x": {ResetsAt: at}}})
	if strings.Contains(string(b), `"error"`) {
		t.Fatalf("empty error should be omitted: %s", string(b))
	}
}
