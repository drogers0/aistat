package notify

import (
	"context"
	"errors"
	"testing"
)

// withSeams replaces goos and captures osascript invocations.
func withSeams(t *testing.T, fakeGOOS string, err error) *[]string {
	t.Helper()
	var scripts []string
	oldGOOS, oldRun := goos, runOsascript
	goos = fakeGOOS
	runOsascript = func(_ context.Context, script string) error {
		scripts = append(scripts, script)
		return err
	}
	t.Cleanup(func() { goos, runOsascript = oldGOOS, oldRun })
	return &scripts
}

func TestSend(t *testing.T) {
	tests := []struct {
		name       string
		goos       string
		title, msg string
		runErr     error
		wantScript string // "" = no osascript call expected
		wantErr    bool
	}{
		{"darwin sends notification", "darwin", "Claude", "switched to b@x.com",
			nil, `display notification "switched to b@x.com" with title "Claude"`, false},
		{"escapes quotes and backslashes", "darwin", `T"t`, `m\"x`,
			nil, `display notification "m\\\"x" with title "T\"t"`, false},
		{"empty title and message still sends", "darwin", "", "",
			nil, `display notification "" with title ""`, false},
		{"non-darwin is a no-op", "linux", "Claude", "hi", nil, "", false},
		{"osascript error propagates", "darwin", "Claude", "hi", errors.New("boom"), `display notification "hi" with title "Claude"`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scripts := withSeams(t, tt.goos, tt.runErr)
			err := Send(context.Background(), tt.title, tt.msg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Send err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantScript == "" {
				if len(*scripts) != 0 {
					t.Fatalf("expected no osascript call, got %v", *scripts)
				}
				return
			}
			if len(*scripts) != 1 || (*scripts)[0] != tt.wantScript {
				t.Errorf("script = %v, want [%s]", *scripts, tt.wantScript)
			}
		})
	}
}
