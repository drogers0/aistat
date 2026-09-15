package watchstate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/testenv"
)

func pct(v float64) *float64 { return &v }

// watchDir redirects the cache dir to a temp dir and returns the watch state
// directory inside it.
func watchDir(t *testing.T) string {
	t.Helper()
	testenv.RedirectHome(t, t.TempDir())
	d, err := dir()
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	return d
}

func TestPublishListClear(t *testing.T) {
	at := time.Date(2026, 9, 10, 21, 5, 20, 0, time.UTC)
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"missing directory lists nothing and creates nothing", func(t *testing.T) {
			d := watchDir(t)
			got, err := List(at)
			if err != nil || got != nil {
				t.Fatalf("List(at) = %v, %v; want nil, nil", got, err)
			}
			if _, err := os.Stat(d); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("List created %s (stat err %v)", d, err)
			}
		}},
		{"round trip preserves fields and off threshold", func(t *testing.T) {
			watchDir(t)
			hb := Heartbeat{
				PID:          42,
				Providers:    []string{"claude", "codex"},
				IntervalSecs: 300,
				Thresholds:   Thresholds{FiveHour: pct(85), Weekly: nil},
				LastTick:     at,
			}
			if err := Publish(hb); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			got, err := List(at)
			if err != nil || len(got) != 1 {
				t.Fatalf("List(at) = %v, %v; want one heartbeat", got, err)
			}
			g := got[0]
			if g.PID != 42 || g.IntervalSecs != 300 || !g.LastTick.Equal(at) ||
				len(g.Providers) != 2 || g.Thresholds.FiveHour == nil || *g.Thresholds.FiveHour != 85 ||
				g.Thresholds.Weekly != nil {
				t.Fatalf("round trip mismatch: %+v", g)
			}
		}},
		{"republish replaces and leaves no temp files", func(t *testing.T) {
			d := watchDir(t)
			for i := range 2 {
				if err := Publish(Heartbeat{PID: 7, IntervalSecs: 60, LastTick: at.Add(time.Duration(i) * time.Minute)}); err != nil {
					t.Fatalf("Publish: %v", err)
				}
			}
			entries, _ := os.ReadDir(d)
			if len(entries) != 1 || entries[0].Name() != "7.json" {
				t.Fatalf("dir entries = %v; want only 7.json", entries)
			}
			got, _ := List(at)
			if len(got) != 1 || !got[0].LastTick.Equal(at.Add(time.Minute)) {
				t.Fatalf("List(at) = %+v; want the second publish", got)
			}
		}},
		{"malformed and non-json files are skipped, newest first", func(t *testing.T) {
			d := watchDir(t)
			_ = Publish(Heartbeat{PID: 1, LastTick: at})
			_ = Publish(Heartbeat{PID: 2, LastTick: at.Add(time.Minute)})
			_ = os.WriteFile(filepath.Join(d, "3.json"), []byte("{not json"), 0600)
			_ = os.WriteFile(filepath.Join(d, "notes.txt"), []byte("{}"), 0600)
			got, err := List(at)
			if err != nil || len(got) != 2 || got[0].PID != 2 || got[1].PID != 1 {
				t.Fatalf("List(at) = %+v, %v; want pids [2 1]", got, err)
			}
		}},
		{"expired heartbeats are deleted, recent stale ones kept", func(t *testing.T) {
			d := watchDir(t)
			_ = Publish(Heartbeat{PID: 1, IntervalSecs: 60, LastTick: at.Add(-expireAfter)})
			_ = Publish(Heartbeat{PID: 2, IntervalSecs: 60, LastTick: at.Add(-expireAfter - time.Second)})
			got, err := List(at)
			if err != nil || len(got) != 1 || got[0].PID != 1 {
				t.Fatalf("List(at) = %+v, %v; want only pid 1", got, err)
			}
			if _, err := os.Stat(filepath.Join(d, "2.json")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("expired heartbeat not deleted (stat err %v)", err)
			}
		}},
		{"supersede removes other watchers with the same scope only", func(t *testing.T) {
			watchDir(t)
			_ = Publish(Heartbeat{PID: 1, Providers: []string{"claude", "codex"}, LastTick: at})
			_ = Publish(Heartbeat{PID: 2, Providers: []string{"claude"}, LastTick: at})
			_ = Publish(Heartbeat{PID: 3, Providers: []string{"claude"}, LastTick: at})
			if err := Supersede(Heartbeat{PID: 3, Providers: []string{"claude"}}, at); err != nil {
				t.Fatalf("Supersede: %v", err)
			}
			got, _ := List(at)
			if len(got) != 2 || got[0].PID+got[1].PID != 4 {
				t.Fatalf("List(at) = %+v; want pids 1 and 3 kept", got)
			}
		}},
		{"clear removes the file and tolerates a missing one", func(t *testing.T) {
			watchDir(t)
			_ = Publish(Heartbeat{PID: 9, LastTick: at})
			if err := Clear(9); err != nil {
				t.Fatalf("Clear: %v", err)
			}
			if got, _ := List(at); len(got) != 0 {
				t.Fatalf("List(at) after Clear = %+v; want empty", got)
			}
			if err := Clear(9); err != nil {
				t.Fatalf("second Clear: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestStale(t *testing.T) {
	last := time.Date(2026, 9, 10, 21, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		interval int
		age      time.Duration
		want     bool
	}{
		{"fresh", 300, time.Minute, false},
		{"exactly 1.2 intervals", 300, 6 * time.Minute, false},
		{"just past 1.2 intervals", 300, 6*time.Minute + time.Second, true},
		{"clock skew into the future", 300, -time.Minute, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Stale(last, tt.interval, last.Add(tt.age)); got != tt.want {
				t.Fatalf("Stale = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCovers(t *testing.T) {
	hb := Heartbeat{Providers: []string{"claude", "codex"}}
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"listed", "codex", true},
		{"not listed", "copilot", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hb.Covers(tt.id); got != tt.want {
				t.Fatalf("Covers(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
