package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/drogers0/aistat/v2/internal/httpx"
	"github.com/drogers0/aistat/v2/internal/providers"
)

func newTestProfileClient(t *testing.T, body []byte, status int) *profileClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	doer := httpx.NewDoer(srv.Client(), "aistat-test/0", "claude",
		map[string]string{"Anthropic-Beta": betaHeader}, nil)
	pc := newProfileClient(doer)
	pc.endpoint = srv.URL + "/api/oauth/profile"
	return pc
}

func TestProfileGet(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"full schema", func(t *testing.T) {
			body := []byte(`{
			"account": {
				"uuid": "9f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c",
				"email": "me@example.com",
				"display_name": "Example User"
			},
			"organization": {
				"uuid": "7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042",
				"name": "me@example.com's Organization",
				"organization_type": "claude_max",
				"seat_tier": null,
				"rate_limit_tier": "default_claude_max_5x",
				"has_extra_usage_enabled": true,
				"subscription_status": "active"
			}
		}`)
			pc := newTestProfileClient(t, body, 200)
			prof, err := pc.Get(context.Background(), "tok-test")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if prof.AccountUUID != "9f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c" {
				t.Errorf("AccountUUID = %q", prof.AccountUUID)
			}
			if prof.Email != "me@example.com" {
				t.Errorf("Email = %q", prof.Email)
			}
			if prof.DisplayName != "Example User" {
				t.Errorf("DisplayName = %q", prof.DisplayName)
			}
			if prof.RateLimitTier != "default_claude_max_5x" {
				t.Errorf("RateLimitTier = %q", prof.RateLimitTier)
			}
			if prof.OrganizationUUID != "7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042" || prof.OrganizationName != "me@example.com's Organization" || prof.OrganizationType != "claude_max" {
				t.Errorf("organization = %#v", prof)
			}
		}},
		{"missing account UUID", func(t *testing.T) {
			assertProfileMissingFields(t, `{"account":{"uuid":"","email":"user@example.com"}}`, `profile response missing required fields (account.uuid/account.email/organization.uuid): got uuid="" email="user@example.com" organization.uuid=""`)
		}},
		{"missing account email", func(t *testing.T) {
			assertProfileMissingFields(t, `{"account":{"uuid":"acct-uuid-1","email":""}}`, `profile response missing required fields (account.uuid/account.email/organization.uuid): got uuid="acct-uuid-1" email="" organization.uuid=""`)
		}},
		{"nil organization uses sentinel", func(t *testing.T) {
			body := []byte(`{
			"account": {
				"uuid": "acct-uuid-personal",
				"email": "personal@example.com",
				"display_name": "Personal User"
			}
		}`)
			pc := newTestProfileClient(t, body, 200)
			prof, err := pc.Get(context.Background(), "tok-test")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if prof.OrganizationUUID != "personal" {
				t.Errorf("OrganizationUUID = %q, want personal sentinel", prof.OrganizationUUID)
			}
		}},
		{"organization UUID empty", func(t *testing.T) {
			assertProfileMissingFields(t, `{"account":{"uuid":"acct-uuid-1","email":"user@example.com"},"organization":{"uuid":""}}`, `profile response missing required fields (account.uuid/account.email/organization.uuid): got uuid="acct-uuid-1" email="user@example.com" organization.uuid=""`)
		}},
		{"organization UUID personal", func(t *testing.T) {
			assertProfileMissingFields(t, `{"account":{"uuid":"acct-uuid-1","email":"user@example.com"},"organization":{"uuid":"personal"}}`, `profile response missing required fields (account.uuid/account.email/organization.uuid): got uuid="acct-uuid-1" email="user@example.com" organization.uuid="personal"`)
		}},
		{"organization UUID non-UUID", func(t *testing.T) {
			assertProfileMissingFields(t, `{"account":{"uuid":"acct-uuid-1","email":"user@example.com"},"organization":{"uuid":"not-a-uuid"}}`, `profile response missing required fields (account.uuid/account.email/organization.uuid): got uuid="acct-uuid-1" email="user@example.com" organization.uuid="not-a-uuid"`)
		}},
		{"organization UUID malformed RFC-4122", func(t *testing.T) {
			assertProfileMissingFields(t, `{"account":{"uuid":"acct-uuid-1","email":"user@example.com"},"organization":{"uuid":"7d3c58e9-6a2b-9f81-c771-1c9e5d3a7042"}}`, `profile response missing required fields (account.uuid/account.email/organization.uuid): got uuid="acct-uuid-1" email="user@example.com" organization.uuid="7d3c58e9-6a2b-9f81-c771-1c9e5d3a7042"`)
		}},
		{"401 wraps ErrAuthDenied", func(t *testing.T) {
			pc := newTestProfileClient(t, []byte(`{"error":"unauthorized"}`), 401)
			_, err := pc.Get(context.Background(), "tok-expired")
			if !errors.Is(err, providers.ErrAuthDenied) {
				t.Errorf("expected ErrAuthDenied, got: %v", err)
			}
		}},
		{"503 wraps ErrTransient", func(t *testing.T) {
			pc := newTestProfileClient(t, []byte(`{"error":"service unavailable"}`), 503)
			_, err := pc.Get(context.Background(), "tok-test")
			if !errors.Is(err, providers.ErrTransient) {
				t.Errorf("expected ErrTransient, got: %v", err)
			}
		}},
		{"non-JSON 200 bare error", func(t *testing.T) {
			pc := newTestProfileClient(t, []byte("<html>oops</html>"), 200)
			_, err := pc.Get(context.Background(), "tok-test")
			if err == nil {
				t.Fatal("expected error for non-JSON body, got nil")
			}
			// Must not be a sentinel — it's a bare error from JSON unmarshal.
			if errors.Is(err, ErrProfileMissingFields) || errors.Is(err, providers.ErrAuthDenied) || errors.Is(err, providers.ErrTransient) {
				t.Errorf("non-JSON 200 should be bare error, got sentinel: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func assertProfileMissingFields(t *testing.T, body, want string) {
	t.Helper()
	pc := newTestProfileClient(t, []byte(body), http.StatusOK)
	_, err := pc.Get(context.Background(), "tok-test")
	if !errors.Is(err, ErrProfileMissingFields) {
		t.Fatalf("errors.Is(..., ErrProfileMissingFields) = false: %v", err)
	}
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}
