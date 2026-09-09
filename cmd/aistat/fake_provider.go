//go:build fake

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/drogers0/aistat/v2/internal/providers"
)

type fakeProvider struct {
	id         string
	shouldFail bool
}

func newFakeProvider(id string, shouldFail bool) *fakeProvider {
	return &fakeProvider{id: id, shouldFail: shouldFail}
}

func (f *fakeProvider) ID() string { return f.id }

func (f *fakeProvider) Fetch(ctx context.Context) (providers.ProviderOutput, error) {
	if f.shouldFail {
		return providers.ProviderOutput{}, fmt.Errorf("fake injected failure for %s", f.id)
	}
	now := time.Now().UTC().Truncate(time.Second)
	mk := func(used float64, in time.Duration) providers.Limit {
		return providers.Limit{
			UsedPercent:       used,
			RemainingPercent:  100 - used,
			ResetsAt:          now.Add(in),
			ResetAfterSeconds: int(in.Seconds()),
		}
	}
	switch f.id {
	case "claude":
		activeLimits := map[string]providers.Limit{
			"five_hour":        mk(12, 4*time.Hour+53*time.Minute),
			"seven_day":        mk(21, 2*24*time.Hour+5*time.Hour),
			"seven_day_sonnet": mk(4, 2*24*time.Hour+5*time.Hour),
		}
		return providers.ProviderOutput{
			Limits: activeLimits,
			Accounts: []providers.AccountResult{
				{
					Email:            "fake@example.com",
					UUID:             "aaaaaaaa-1111-2222-3333-444444444444",
					Key:              "aaaaaaaa-1111-2222-3333-444444444444_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042",
					Address:          "fake@example.com/personal-aaaaaaaa",
					OrganizationName: "fake@example.com's Organization",
					OrganizationType: "claude_max",
					Plan:             "default_claude_max_5x",
					Active:           true,
					Limits:           activeLimits,
				},
				{
					Email:            "fake@example.com",
					UUID:             "aaaaaaaa-1111-2222-3333-444444444444",
					Key:              "aaaaaaaa-1111-2222-3333-444444444444_550e8400-e29b-41d4-a716-446655440000",
					Address:          "fake@example.com/engineering-550e8400",
					OrganizationName: "Engineering",
					OrganizationType: "claude_team",
					Plan:             "default_claude_max_20x",
					Active:           false,
					Limits: map[string]providers.Limit{
						"five_hour": mk(71, 5*time.Minute),
						"seven_day": mk(44, 5*24*time.Hour+9*time.Hour),
					},
				},
			},
		}, nil
	case "codex":
		codexLimits := map[string]providers.Limit{
			"five_hour":             mk(0, 3*time.Hour+12*time.Minute),
			"seven_day":             mk(11, 4*24*time.Hour+1*time.Hour),
			"code_review_seven_day": mk(0, 4*24*time.Hour+1*time.Hour),
		}
		return providers.ProviderOutput{
			Accounts: []providers.AccountResult{
				{
					Email:  "user@codex.example.com",
					UUID:   "cccccccc-1111-2222-3333-444444444444",
					Key:    "cccccccc-1111-2222-3333-444444444444",
					Active: true,
					Limits: codexLimits,
				},
			},
		}, nil
	case "copilot":
		return providers.ProviderOutput{Limits: map[string]providers.Limit{
			"month": mk(4, 5*24*time.Hour+7*time.Hour),
		}}, nil
	}
	panic("unknown fake provider id: " + f.id)
}
