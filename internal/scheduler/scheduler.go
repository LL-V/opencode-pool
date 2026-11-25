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

// Pick balances load across the candidate pool. The old "yield when all
// healthy" policy let the host's built-in picker concentrate thousands of
// requests on one account (real report 2026-11-25: 1.4k requests / 33
// inflight on one account while peers idled). The plugin now always makes
// the decision so it can spread concurrency evenly.
//
// Session affinity still wins for a healthy (not busy, not near) bound
// account to preserve prompt cache; once it is busy/near the conversation
// migrates to the least-loaded peer (cache loss accepted, same as R10).
func Pick(in Input, deps Deps) Decision {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	// 无候选可判：交还宿主。宿主只认 Handled+Reject，空池 Reject 会让
	// 非本池模型（claude 等）也得到 503。
	if len(in.Candidates) == 0 {
		return Decision{}
	}
	cands := make([]Candidate, 0, len(in.Candidates))
	for _, c := range in.Candidates {
		if !deps.Bad(c.ID) {
			cands = append(cands, c)
		}
	}
	// Affinity first, but only while the bound account is not saturated:
	// sticking a busy account would pile even more concurrency onto it.
	if in.SessionID != "" {
		if bound, ok := deps.BoundAuth(in.SessionID); ok {
			for _, c := range cands {
				if c.ID == bound && !deps.Busy(bound) && !deps.Near(bound) {
					return Decision{Handled: true, AuthID: bound}
				}
			}
		}
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
		if deps.Busy(a.ID) != deps.Busy(b.ID) {
			return !deps.Busy(a.ID)
		}
		if deps.Near(a.ID) != deps.Near(b.ID) {
			return !deps.Near(a.ID)
		}
		// 并发平均优先于优先级：否则高优先级账号会先被连续选中，
		// 直到 busy 才溢出到其他号（用户实测 33/20 vs 全 0 的根源）。
		if ia, ib := deps.Inflight(a.ID), deps.Inflight(b.ID); ia != ib {
			return ia < ib
		}
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		pa, oka := deps.Pressure(a.ID)
		pb, okb := deps.Pressure(b.ID)
		if oka != okb {
			return oka // pct-known accounts order by pressure first
		}
		if oka && pa != pb {
			return pa > pb
		}
		return a.ID < b.ID
	})
	return Decision{Handled: true, AuthID: cands[0].ID}
}
