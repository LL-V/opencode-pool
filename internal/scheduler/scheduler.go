// Package scheduler implements the yield-first auth selection policy of
// DESIGN.md M6: healthy pools defer to the host scheduler, and the plugin
// only takes over when a candidate is flagged bad/near/busy.
package scheduler

import (
	"fmt"
	"sort"
	"time"
)

// Candidate mirrors the host scheduler candidate subset we consume.
type Candidate struct {
	ID       string
	Priority int
}

// Input is the request-scoped scheduling input.
type Input struct {
	Candidates []Candidate
	SessionID  string
	Now        time.Time
}

// Decision is the SchedulerPickResponse subset we produce.
type Decision struct {
	Handled      bool
	AuthID       string
	Reject       bool
	RejectReason string
}

// Deps abstracts per-account state lookups (backed by the store).
type Deps interface {
	Bad(id string) bool                 // requires_sign_in || all windows exhausted
	Busy(id string) bool                // learned limit known && inflight >= limit
	Near(id string) bool                // any window >= near threshold, not exhausted
	Pressure(id string) (float64, bool) // remaining%/max(hoursToReset,0.5); ok if pct known
	Inflight(id string) int
	BoundAuth(sid string) (string, bool)
	EarliestRecovery() time.Time // among bad candidates, for Reject reason
}

// Pick applies the M6 policy. Affinity precedes the yield decision: a
// request whose session is bound to a healthy candidate is picked by the
// plugin even in an otherwise healthy pool, because the host scheduler does
// not track our x-opencode-session bindings — yielding would rotate accounts
// and destroy upstream prompt-cache affinity for header-less clients.
func Pick(in Input, deps Deps) Decision {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	var bad, near, busy bool
	cands := make([]Candidate, 0, len(in.Candidates))
	for _, c := range in.Candidates {
		cBad := deps.Bad(c.ID)
		bad = bad || cBad
		near = near || (!cBad && deps.Near(c.ID))
		busy = busy || (!cBad && deps.Busy(c.ID))
		if !cBad {
			cands = append(cands, c)
		}
	}
	// Affinity first: bound auth wins unless bad (the host already excluded
	// tried candidates; a bound auth outside cands falls through below).
	if in.SessionID != "" {
		if bound, ok := deps.BoundAuth(in.SessionID); ok {
			for _, c := range cands {
				if c.ID == bound {
					return Decision{Handled: true, AuthID: bound}
				}
			}
		}
	}
	if !bad && !near && !busy {
		return Decision{} // yield: Handled=false
	}
	if len(cands) == 0 {
		reason := "no auth available"
		if er := deps.EarliestRecovery(); !er.IsZero() {
			reason = fmt.Sprintf("earliest recovery %s", er.UTC().Format(time.RFC3339))
		}
		// Host protocol: Reject is only honored when Handled is true
		// (pluginhost/scheduler.go returns early on !Handled; conductor only
		// reads Reject when handled). Handled=false here would silently fall
		// back to the built-in picker and re-select a bad/purged account.
		return Decision{Handled: true, Reject: true, RejectReason: reason}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if deps.Busy(a.ID) != deps.Busy(b.ID) {
			return !deps.Busy(a.ID)
		}
		if deps.Near(a.ID) != deps.Near(b.ID) {
			return !deps.Near(a.ID)
		}
		pa, oka := deps.Pressure(a.ID)
		pb, okb := deps.Pressure(b.ID)
		if oka != okb {
			return oka // pct-known accounts order by pressure first
		}
		if oka && pa != pb {
			return pa > pb
		}
		if ia, ib := deps.Inflight(a.ID), deps.Inflight(b.ID); ia != ib {
			return ia < ib
		}
		return a.ID < b.ID
	})
	return Decision{Handled: true, AuthID: cands[0].ID}
}
