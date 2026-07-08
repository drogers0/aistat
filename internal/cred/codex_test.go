package cred

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/drogers0/aistat/v2/internal/testenv"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

// makeTestJWT creates a minimal JWT (header.payload.sig) from the given JSON payload.
// The signature segment is always "sig"; it is not verified by ParseCodexIDToken.
func makeTestJWT(t *testing.T, payloadJSON string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	return header + "." + payload + ".sig"
}

func writeAuth(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	testenv.RedirectHome(t, home)
	dir := filepath.Join(home, ".codex")
	testutil.WantNoErr(t, os.MkdirAll(dir, 0o700))
	testutil.WantNoErr(t, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600))
	return home
}

func TestReadCodexToken(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"happy path", func(t *testing.T) {
			writeAuth(t, `{"tokens":{"access_token":"tok-abc"}}`)
			got, err := ReadCodexToken(context.Background())
			testutil.WantNoErr(t, err)
			if got != "tok-abc" {
				t.Errorf("token = %q, want tok-abc", got)
			}
		}},
		{"missing", func(t *testing.T) {
			testenv.RedirectHome(t, t.TempDir()) // no .codex dir
			_, err := ReadCodexToken(context.Background())
			if !errors.Is(err, ErrCodexTokenNotFound) {
				t.Errorf("expected ErrCodexTokenNotFound, got: %v", err)
			}
		}},
		{"malformed json", func(t *testing.T) {
			writeAuth(t, `{ this is not json }`)
			_, err := ReadCodexToken(context.Background())
			if err == nil {
				t.Fatal("expected error for malformed JSON")
			}
			if !strings.Contains(err.Error(), "not valid JSON") {
				t.Errorf("error should mention 'not valid JSON', got: %v", err)
			}
			if errors.Is(err, ErrCodexTokenNotFound) {
				t.Errorf("malformed JSON should not wrap ErrCodexTokenNotFound: %v", err)
			}
		}},
		{"empty token", func(t *testing.T) {
			writeAuth(t, `{"tokens":{"access_token":""}}`)
			_, err := ReadCodexToken(context.Background())
			if !errors.Is(err, ErrCodexTokenNotFound) {
				t.Errorf("expected ErrCodexTokenNotFound, got: %v", err)
			}
		}},
		{"still works after credential read", func(t *testing.T) {
			writeAuth(t, `{"tokens":{"access_token":"my-token"}}`)
			got, err := ReadCodexToken(context.Background())
			testutil.WantNoErr(t, err)
			if got != "my-token" {
				t.Errorf("token = %q, want my-token", got)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- ParseCodexIDToken tests ---

func TestParseCodexIDToken(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"happy path", func(t *testing.T) {
			jwt := makeTestJWT(t, `{"sub":"u1","email":"user@example.com","exp":1700000000}`)
			sub, email, expSec, err := ParseCodexIDToken(jwt)
			testutil.WantNoErr(t, err)
			if sub != "u1" {
				t.Errorf("sub = %q, want u1", sub)
			}
			if email != "user@example.com" {
				t.Errorf("email = %q, want user@example.com", email)
			}
			if expSec != 1700000000 {
				t.Errorf("expSec = %d, want 1700000000", expSec)
			}
		}},
		{"no email", func(t *testing.T) {
			jwt := makeTestJWT(t, `{"sub":"u2","exp":1700000000}`)
			sub, email, expSec, err := ParseCodexIDToken(jwt)
			testutil.WantNoErr(t, err)
			if sub != "u2" {
				t.Errorf("sub = %q, want u2", sub)
			}
			if email != "" {
				t.Errorf("email = %q, want empty", email)
			}
			if expSec != 1700000000 {
				t.Errorf("expSec = %d, want 1700000000", expSec)
			}
		}},
		{"no exp", func(t *testing.T) {
			jwt := makeTestJWT(t, `{"sub":"u3"}`)
			sub, _, expSec, err := ParseCodexIDToken(jwt)
			testutil.WantNoErr(t, err)
			if sub != "u3" {
				t.Errorf("sub = %q, want u3", sub)
			}
			if expSec != 0 {
				t.Errorf("expSec = %d, want 0 when exp absent", expSec)
			}
		}},
		{"missing subject", func(t *testing.T) {
			jwt := makeTestJWT(t, `{}`)
			_, _, _, err := ParseCodexIDToken(jwt)
			if err == nil {
				t.Fatal("expected error for missing sub")
			}
			if !strings.Contains(err.Error(), "sub") {
				t.Errorf("error should mention 'sub', got: %v", err)
			}
		}},
		{"two segments only", func(t *testing.T) {
			_, _, _, err := ParseCodexIDToken("header.payload")
			if err == nil {
				t.Fatal("expected error for two-segment token")
			}
		}},
		{"one segment", func(t *testing.T) {
			_, _, _, err := ParseCodexIDToken("header")
			if err == nil {
				t.Fatal("expected error for one-segment token")
			}
		}},
		{"four segments", func(t *testing.T) {
			_, _, _, err := ParseCodexIDToken("a.b.c.d")
			if err == nil {
				t.Fatal("expected error for four-segment token")
			}
		}},
		{"empty segment", func(t *testing.T) {
			// Leading-dot (.b.c) and trailing-dot (a.b.) must both be rejected.
			for _, tok := range []string{".b.c", "a.b.", "a..c"} {
				if _, _, _, err := ParseCodexIDToken(tok); err == nil {
					t.Errorf("expected error for empty-segment token %q", tok)
				}
			}
		}},
		{"bad base64 payload", func(t *testing.T) {
			_, _, _, err := ParseCodexIDToken("header.!!!.sig")
			if err == nil {
				t.Fatal("expected error for bad base64")
			}
		}},
		{"bad json payload", func(t *testing.T) {
			payload := base64.RawURLEncoding.EncodeToString([]byte("not json"))
			_, _, _, err := ParseCodexIDToken("header." + payload + ".sig")
			if err == nil {
				t.Fatal("expected error for bad JSON payload")
			}
		}},
		{"empty id token", func(t *testing.T) {
			_, _, _, err := ParseCodexIDToken("")
			if err == nil {
				t.Fatal("expected error for empty id_token")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- ParseJWTExp tests ---

func TestParseJWTExp(t *testing.T) {
	tests := []struct {
		name    string
		run     func(t *testing.T) (int64, bool)
		wantExp int64
		wantOK  bool
	}{
		{"present", func(t *testing.T) (int64, bool) {
			return ParseJWTExp(makeTestJWT(t, `{"sub":"u1","exp":1700000000}`))
		}, 1700000000, true},
		{"past exp still returned", func(t *testing.T) (int64, bool) {
			return ParseJWTExp(makeTestJWT(t, `{"exp":1000000000}`))
		}, 1000000000, true},
		{"float exp truncated", func(t *testing.T) (int64, bool) {
			return ParseJWTExp(makeTestJWT(t, `{"exp":1700000000.9}`))
		}, 1700000000, true},
		{"exp absent", func(t *testing.T) (int64, bool) {
			return ParseJWTExp(makeTestJWT(t, `{"sub":"u1"}`))
		}, 0, false},
		{"exp zero", func(t *testing.T) (int64, bool) {
			return ParseJWTExp(makeTestJWT(t, `{"exp":0}`))
		}, 0, false},
		{"exp negative", func(t *testing.T) (int64, bool) {
			return ParseJWTExp(makeTestJWT(t, `{"exp":-1}`))
		}, 0, false},
		{"opaque non-jwt", func(t *testing.T) (int64, bool) {
			return ParseJWTExp("test-access-token-abc")
		}, 0, false},
		{"two segments", func(t *testing.T) (int64, bool) {
			return ParseJWTExp("header.payload")
		}, 0, false},
		{"empty segment", func(t *testing.T) (int64, bool) {
			return ParseJWTExp("a..c")
		}, 0, false},
		{"bad base64 payload", func(t *testing.T) (int64, bool) {
			return ParseJWTExp("header.!!!.sig")
		}, 0, false},
		{"bad json payload", func(t *testing.T) (int64, bool) {
			return ParseJWTExp("header." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".sig")
		}, 0, false},
		{"empty", func(t *testing.T) (int64, bool) {
			return ParseJWTExp("")
		}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expSec, ok := tt.run(t)
			if expSec != tt.wantExp || ok != tt.wantOK {
				t.Errorf("ParseJWTExp = (%d, %t), want (%d, %t)", expSec, ok, tt.wantExp, tt.wantOK)
			}
		})
	}
}

// --- ReadCodexCredential tests ---

const testIDToken = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJjb2RleC10ZXN0LXN1YiIsImVtYWlsIjoidGVzdEBleGFtcGxlLmNvbSIsImV4cCI6OTk5OTk5OTk5OX0.testsig"

func TestReadCodexCredential(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"happy path", func(t *testing.T) {
			// Codex Credential.ExpiresAt is always 0 — even with a valid id_token
			// present, parseCodexCredFull does not derive expiry (the refresh gate
			// reads the access-token JWT exp on demand in codex.StoredExpiresAt).
			body := `{"tokens":{"access_token":"tok","refresh_token":"ref","id_token":"` + testIDToken + `"}}`
			writeAuth(t, body)
			cred, err := ReadCodexCredential(context.Background())
			testutil.WantNoErr(t, err)
			if cred.AccessToken != "tok" {
				t.Errorf("AccessToken = %q, want tok", cred.AccessToken)
			}
			if cred.RefreshToken != "ref" {
				t.Errorf("RefreshToken = %q, want ref", cred.RefreshToken)
			}
			if cred.ExpiresAt != 0 {
				t.Errorf("ExpiresAt = %d, want 0 (codex never derives ExpiresAt)", cred.ExpiresAt)
			}
			if len(cred.Raw) == 0 {
				t.Error("Raw should be non-empty")
			}
		}},
		{"no id token still zero expiry", func(t *testing.T) {
			writeAuth(t, `{"tokens":{"access_token":"tok","refresh_token":"ref"}}`)
			cred, err := ReadCodexCredential(context.Background())
			testutil.WantNoErr(t, err)
			if cred.ExpiresAt != 0 {
				t.Errorf("ExpiresAt = %d, want 0", cred.ExpiresAt)
			}
			if cred.AccessToken != "tok" {
				t.Errorf("AccessToken = %q, want tok", cred.AccessToken)
			}
		}},
		{"missing access token", func(t *testing.T) {
			writeAuth(t, `{"tokens":{"access_token":"","refresh_token":"ref"}}`)
			_, err := ReadCodexCredential(context.Background())
			if !errors.Is(err, ErrCodexTokenNotFound) {
				t.Errorf("expected ErrCodexTokenNotFound, got: %v", err)
			}
		}},
		{"missing", func(t *testing.T) {
			testenv.RedirectHome(t, t.TempDir())
			_, err := ReadCodexCredential(context.Background())
			if !errors.Is(err, ErrCodexTokenNotFound) {
				t.Errorf("expected ErrCodexTokenNotFound, got: %v", err)
			}
		}},
		{"malformed json", func(t *testing.T) {
			writeAuth(t, `{not json}`)
			_, err := ReadCodexCredential(context.Background())
			if err == nil {
				t.Fatal("expected error for malformed JSON")
			}
			if errors.Is(err, ErrCodexTokenNotFound) {
				t.Errorf("malformed JSON should not wrap ErrCodexTokenNotFound: %v", err)
			}
		}},
		{"raw preserved", func(t *testing.T) {
			body := `{"tokens":{"access_token":"tok"},"extra_field":"preserved"}`
			writeAuth(t, body)
			cred, err := ReadCodexCredential(context.Background())
			testutil.WantNoErr(t, err)
			if !bytes.Equal(cred.Raw, []byte(body)) {
				t.Errorf("Raw = %q, want %q", cred.Raw, body)
			}
		}},
		{"api key mode fails", func(t *testing.T) {
			// D7 / A7: auth_mode=="api_key" with no tokens object → fail-closed with ErrCodexTokenNotFound.
			writeAuth(t, `{"auth_mode":"api_key","OPENAI_API_KEY":"sk-test"}`)
			_, err := ReadCodexCredential(context.Background())
			if !errors.Is(err, ErrCodexTokenNotFound) {
				t.Errorf("expected ErrCodexTokenNotFound for API-key mode, got: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- WriteCodexLiveBlob tests ---

func TestWriteCodexLiveBlob(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"happy path", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)
			// Pre-create the .codex dir so we can confirm mode on write.
			testutil.WantNoErr(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o700))
			data := []byte(`{"tokens":{"access_token":"written"}}`)
			testutil.WantNoErr(t, WriteCodexLiveBlob(context.Background(), data))
			path := filepath.Join(home, ".codex", "auth.json")
			got, err := os.ReadFile(path)
			testutil.WantNoErr(t, err)
			if !bytes.Equal(got, data) {
				t.Errorf("content = %q, want %q", got, data)
			}
			// POSIX file modes are advisory on Windows (NTFS ACLs), so assert the
			// 0600 mode only on unix; the write round-trip above still runs on Windows.
			if runtime.GOOS != "windows" {
				info, err := os.Stat(path)
				testutil.WantNoErr(t, err)
				if perm := info.Mode().Perm(); perm != 0o600 {
					t.Errorf("mode = %o, want 0600", perm)
				}
			}
		}},
		{"creates dir", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)
			// Do NOT pre-create .codex dir.
			data := []byte(`{"tokens":{"access_token":"new"}}`)
			testutil.WantNoErr(t, WriteCodexLiveBlob(context.Background(), data))
			got, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
			testutil.WantNoErr(t, err)
			if !bytes.Equal(got, data) {
				t.Errorf("content = %q, want %q", got, data)
			}
		}},
		{"atomic", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)
			dir := filepath.Join(home, ".codex")
			testutil.WantNoErr(t, os.MkdirAll(dir, 0o700))
			testutil.WantNoErr(t, WriteCodexLiveBlob(context.Background(), []byte(`{"tokens":{"access_token":"x"}}`)))
			matches, err := filepath.Glob(filepath.Join(dir, ".auth-*.json"))
			testutil.WantNoErr(t, err)
			if len(matches) != 0 {
				t.Errorf("temp files left behind: %v", matches)
			}
		}},
		{"overwrites", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)
			first := []byte(`{"tokens":{"access_token":"first"}}`)
			second := []byte(`{"tokens":{"access_token":"second"}}`)
			testutil.WantNoErr(t, WriteCodexLiveBlob(context.Background(), first))
			testutil.WantNoErr(t, WriteCodexLiveBlob(context.Background(), second))
			got, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
			testutil.WantNoErr(t, err)
			if !bytes.Equal(got, second) {
				t.Errorf("content = %q, want second write %q", got, second)
			}
		}},
		{"round trip", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)
			body := `{"tokens":{"access_token":"rt-tok","refresh_token":"rt-ref"}}`
			testutil.WantNoErr(t, WriteCodexLiveBlob(context.Background(), []byte(body)))
			cred, err := ReadCodexCredential(context.Background())
			testutil.WantNoErr(t, err)
			if cred.AccessToken != "rt-tok" {
				t.Errorf("AccessToken = %q, want rt-tok", cred.AccessToken)
			}
			if cred.RefreshToken != "rt-ref" {
				t.Errorf("RefreshToken = %q, want rt-ref", cred.RefreshToken)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
