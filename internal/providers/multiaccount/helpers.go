// Package multiaccount provides provider-neutral helpers for managing
// multi-account usage fetches: result sorting, outcome recording, reset-time
// recomputation, and pool-timeout budgeting.
package multiaccount

import (
	"errors"
	"sort"
	"time"

	"github.com/drogers0/aistat/v2/internal/providers"
)

// SortAccountResults sorts results in-place: active first, then email and
// opaque account key. Deterministic ordering keeps JSON output diff-stable.
func SortAccountResults(results []providers.AccountResult) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Active != results[j].Active {
			return results[i].Active
		}
		if results[i].Email != results[j].Email {
			return results[i].Email < results[j].Email
		}
		return results[i].Key < results[j].Key
	})
}

// RecordFetchOutcome populates ar with the result of a usage fetch and reports
// whether the call succeeded and (when it didn't) whether the failure was
// transient. The counter updates stay at the call site so the D8 retry rule
// (ErrTransient iff zero succeeded AND at least one transient) is visible in
// Fetch's body rather than buried in a helper. Used by Fetch's fallback-row and
// per-stored-account branches; the refresh-failure branch sets ar.Error itself
// via refreshErrorMessage and shares the same counter discipline.
func RecordFetchOutcome(ar *providers.AccountResult, limits map[string]providers.Limit, fetchErr error) (success, transient bool) {
	if fetchErr != nil {
		ar.Error = fetchErr.Error()
		return false, errors.Is(fetchErr, providers.ErrTransient)
	}
	ar.Limits = limits
	return true, false
}

// RecomputeResetAfter returns a new map with each Limit's ResetAfterSeconds
// recomputed from ResetsAt relative to now. The input map is not modified.
func RecomputeResetAfter(m map[string]providers.Limit, now time.Time) map[string]providers.Limit {
	out := make(map[string]providers.Limit, len(m))
	for k, l := range m {
		secs := int(l.ResetsAt.Sub(now).Seconds())
		if secs < 0 {
			secs = 0
		}
		l.ResetAfterSeconds = secs
		out[k] = l
	}
	return out
}

// Budget computes a pool-timeout duration: base plus perAccount for each of
// count accounts. count may be zero (returns base alone).
func Budget(base, perAccount time.Duration, count int) time.Duration {
	return base + time.Duration(count)*perAccount
}
