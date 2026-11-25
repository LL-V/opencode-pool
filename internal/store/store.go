// Package store keeps per-account runtime state and session bindings per
// DESIGN.md M7. All mutations go through a single mutex; persistence is an
// atomic JSON file with a monotonically increasing generation counter.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// WindowTolerance is the clock-skew allowance for monotonic window updates
// and doubles as the early-reset detection threshold (DESIGN.md M4).
const WindowTolerance = 5 * time.Minute

// BindingTTL is how long a binding survives after its last successful use.
const BindingTTL = time.Hour

// defaultBindingCap caps the binding table (LRU eviction beyond it).
const defaultBindingCap = 10000

// Window is one quota window observation.
type Window struct {
	UsedPct float64   `json:"used_pct"`
	Until   time.Time `json:"until"`
	Source  string    `json:"source"`
	SeenAt  time.Time `json:"seen_at"`
}

// Account is the persisted per-account record.
type Account struct {
	RequiresSignIn          bool               `json:"requires_sign_in"`
	VerifyPending           bool               `json:"verify_pending"`
	Disabled                bool               `json:"disabled"`
	ManualUnblockUntil      *time.Time         `json:"manual_unblock_until,omitempty"`
	LearnedConcurrencyLimit int                `json:"learned_concurrency_limit,omitempty"`
	Windows                 map[string]*Window `json:"windows"`
	Usage                   Usage              `json:"usage"`
	LastError               *LastError         `json:"last_error,omitempty"`
	History                 []History          `json:"history,omitempty"`
	Models                  []string           `json:"models,omitempty"`
	ModelsCachedAt          *time.Time         `json:"models_cached_at,omitempty"`
}

// Usage accumulates absolute counters only (no percentages — DESIGN.md H9).
type Usage struct {
	Requests     int64      `json:"requests"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

// LastError is a compact recent-failure record.
type LastError struct {
	Code string    `json:"code"`
	At   time.Time `json:"at"`
}

type binding struct {
	AuthID   string    `json:"auth_id"`
	LastSeen time.Time `json:"last_seen"`
}

type state struct {
	Version    int                 `json:"version"`
	Generation uint64              `json:"generation"`
	Accounts   map[string]*Account `json:"accounts"`
	Bindings   map[string]*binding `json:"bindings"`
	// Purged tombstones account ids the operator deleted. Host auth files
	// cannot be removed through this ABI, so every register-time Reconcile
	// would otherwise resurrect deleted accounts from the host auth list.
	Purged map[string]bool `json:"purged,omitempty"`
}

// Store is the in-memory state with optional file persistence.
type Store struct {
	mu         sync.Mutex
	st         state
	inflight   map[string]int // memory-only by design
	dir        string
	bindingCap int
	bindingLRU []string // least-recently-bound first
}

// New creates a store; dir == "" means memory-only.
func New(dir string) *Store {
	return &Store{
		st: state{
			Version:  2,
			Accounts: map[string]*Account{},
			Bindings: map[string]*binding{},
			Purged:   map[string]bool{},
		},
		inflight:   map[string]int{},
		dir:        dir,
		bindingCap: defaultBindingCap,
	}
}

// WithBindingCap overrides the binding table cap (tests).
func (s *Store) WithBindingCap(n int) *Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindingCap = n
	return s
}

// Snapshot is the copy-on-read view returned by Store.Snapshot.
type Snapshot struct {
	Version    int
	Generation uint64
	Accounts   map[string]*Account
	Bindings   map[string]string
	Inflight   map[string]int
}

// Snapshot returns a copy for lock-free reads.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked(true)
}

// SnapshotScheduling is Snapshot without the binding table. Scheduler picks
// call it once per pick (bindings are queried separately by session id);
// copying up to 10k bindings per dependency lookup was pure overhead.
func (s *Store) SnapshotScheduling() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked(false)
}

func (s *Store) snapshotLocked(withBindings bool) Snapshot {
	snap := Snapshot{
		Version:    s.st.Version,
		Generation: s.st.Generation,
		Accounts:   make(map[string]*Account, len(s.st.Accounts)),
		Bindings:   make(map[string]string, len(s.st.Bindings)),
		Inflight:   make(map[string]int, len(s.inflight)),
	}
	if withBindings {
		for sid, b := range s.st.Bindings {
			snap.Bindings[sid] = b.AuthID
		}
	} else {
		snap.Bindings = nil
	}
	for id, acc := range s.st.Accounts {
		clone := *acc
		clone.Windows = make(map[string]*Window, len(acc.Windows))
		for w, win := range acc.Windows {
			c := *win
			clone.Windows[w] = &c
		}
		snap.Accounts[id] = &clone
	}
	for id, n := range s.inflight {
		snap.Inflight[id] = n
	}
	return snap
}

// —— 账号 ——

func (s *Store) ensureAccountLocked(id string) {
	if _, ok := s.st.Accounts[id]; !ok {
		s.st.Accounts[id] = &Account{Windows: map[string]*Window{}}
	}
}

// EnsureAccount creates the record if absent.
func (s *Store) EnsureAccount(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureAccountLocked(id)
}

// SetRequiresSignIn flags/unflags an account.
func (s *Store) SetRequiresSignIn(id string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureAccountLocked(id)
	s.st.Accounts[id].RequiresSignIn = v
	s.st.Generation++
}

// SetVerifyPending flags a suspect account pending verification.
func (s *Store) SetVerifyPending(id string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureAccountLocked(id)
	s.st.Accounts[id].VerifyPending = v
	s.st.Generation++
}

// LearnLimit records the upstream-advertised concurrency limit.
func (s *Store) LearnLimit(id string, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureAccountLocked(id)
	if s.st.Accounts[id].LearnedConcurrencyLimit == n {
		return
	}
	s.st.Accounts[id].LearnedConcurrencyLimit = n
	s.st.Generation++
}

// —— 窗口 ——

// ObserveWindow applies the monotonic-with-tolerance update (source "").
func (s *Store) ObserveWindow(id, window string, usedPct float64, until, now time.Time) {
	s.ObserveWindowSource(id, window, "", usedPct, until, now)
}

// sourceRank orders observation authority: a live /usage read outranks a
// passively harvested header, which outranks our own 429 marking.
func sourceRank(source string) int {
	switch source {
	case "api":
		return 3
	case "passive":
		return 2
	case "429":
		return 1
	}
	return 0
}

// ObserveWindowSource applies the M4 rule:
//   - until >= old.until−tolerance → refresh used_pct (until keeps the later
//     of the two; never shortens recovery inside the tolerance band);
//   - until far EARLIER → clock skew unless the new observation comes from a
//     strictly more authoritative source (e.g. a /usage read proving the
//     upstream window already reset) — only then adopt immediately.
func (s *Store) ObserveWindowSource(id, window, source string, usedPct float64, until, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureAccountLocked(id)
	s.st.Generation++
	w := s.st.Accounts[id].Windows[window]
	if w == nil {
		s.st.Accounts[id].Windows[window] = &Window{UsedPct: usedPct, Until: until, Source: source, SeenAt: now}
		return
	}
	if until.Before(w.Until.Add(-WindowTolerance)) {
		// 更高权威，或同为权威 API 的自我纠正（先长后短），都应采纳；
		// 否则一次错误的 api until 永远无法被后续正确的 /usage 修正。
		if sourceRank(source) > sourceRank(w.Source) || (source == "api" && w.Source == "api") {
			w.UsedPct, w.Until, w.SeenAt, w.Source = usedPct, until, now, source
		}
		// 同/低权威的远早 until 视为时钟漂移，拒绝。
		return
	}
	// Within tolerance: refresh pct, never shorten the recovery deadline.
	// The recorded source keeps the highest authority that established the
	// current until, so a later same-rank read cannot bypass skew protection.
	// A lower-authority source also cannot POSTPONE a live higher-authority
	// deadline (a 429's conservative +5h guess must not extend an api-proven
	// until — deep-check P2).
	if sourceRank(source) < sourceRank(w.Source) && now.Before(w.Until) && until.After(w.Until) {
		w.UsedPct = usedPct
		w.SeenAt = now
		return
	}
	w.UsedPct, w.SeenAt = usedPct, now
	if until.After(w.Until) {
		// 截止时间由本观测确立 → Source 必须跟着它走。否则一个低权威
		// 429 延长了已过期窗口，却保留 Source=api，后续真实 /usage 的
		// 更早 reset 会被误判为漂移拒绝，账号被多封 5h/7d。
		w.Until = until
		w.Source = source
	} else if sourceRank(source) >= sourceRank(w.Source) {
		w.Source = source
	}
}

// WindowExhausted reports whether a specific window is at 100% pre-recovery.
func (s *Store) WindowExhausted(id, window string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.st.Accounts[id]
	if acc == nil {
		return false
	}
	w := acc.Windows[window]
	return w != nil && w.UsedPct >= 100 && now.Before(w.Until)
}

// AnyWindowExhausted reports whether any known window is still exhausted at
// now. Upstream rejects (429) as soon as ANY window hits 100%, so a single
// live-exhausted window must pause the account; waiting for all windows to
// fill would keep ramming the 429 wall.
func (s *Store) AnyWindowExhausted(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.st.Accounts[id]
	if acc == nil {
		return false
	}
	for _, w := range acc.Windows {
		if w.UsedPct >= 100 && now.Before(w.Until) {
			return true
		}
	}
	return false
}

// AllWindowsExhausted reports whether every known window is exhausted at now.
func (s *Store) AllWindowsExhausted(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.st.Accounts[id]
	if acc == nil || len(acc.Windows) == 0 {
		return false
	}
	for _, w := range acc.Windows {
		if !(w.UsedPct >= 100 && now.Before(w.Until)) {
			return false
		}
	}
	return true
}

// —— inflight（纯内存）——

// InflightInc increments and returns the new count.
func (s *Store) InflightInc(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight[id]++
	return s.inflight[id]
}

// InflightDec decrements with a floor of zero and returns the new count.
func (s *Store) InflightDec(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[id] <= 0 {
		return 0
	}
	s.inflight[id]--
	return s.inflight[id]
}

// —— 绑定 ——

// Bind records the most recent successful auth for a session id (overwrite =
// migration) and enforces the LRU cap.
func (s *Store) Bind(sid, authID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Bindings[sid] = &binding{AuthID: authID, LastSeen: at}
	s.st.Generation++
	s.touchBindingLRU(sid)
	for len(s.st.Bindings) > s.bindingCap {
		oldest := s.bindingLRU[0]
		s.bindingLRU = s.bindingLRU[1:]
		delete(s.st.Bindings, oldest)
	}
}

func (s *Store) touchBindingLRU(sid string) {
	for i, id := range s.bindingLRU {
		if id == sid {
			s.bindingLRU = append(s.bindingLRU[:i], s.bindingLRU[i+1:]...)
			break
		}
	}
	s.bindingLRU = append(s.bindingLRU, sid)
}

// LookupBinding resolves a session to its bound auth. It never renews
// last_seen (Pick hits must not extend TTL — DESIGN.md M7).
func (s *Store) LookupBinding(sid string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.st.Bindings[sid]
	if b == nil || now.Sub(b.LastSeen) > BindingTTL {
		return "", false
	}
	return b.AuthID, true
}

// ClearBindings removes all bindings pointing at an auth (ops endpoint).
func (s *Store) ClearBindings(authID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	doomed := map[string]bool{}
	for sid, b := range s.st.Bindings {
		if b.AuthID == authID {
			delete(s.st.Bindings, sid)
			doomed[sid] = true
			n++
		}
	}
	if n > 0 {
		s.bindingLRU = dropLRU(s.bindingLRU, doomed)
		s.st.Generation++
	}
	return n
}

// dropLRU removes doomed sids from the LRU order (ghost entries would grow
// unbounded and break eviction once the map side is deleted).
func dropLRU(lru []string, doomed map[string]bool) []string {
	out := lru[:0]
	for _, sid := range lru {
		if !doomed[sid] {
			out = append(out, sid)
		}
	}
	return out
}

// —— 对账与持久化 ——

// IsPurged reports whether an account was deleted by the operator. Purged
// accounts are hidden from the panel and never resurrected by usage events
// or host reconciliation (the host auth file itself cannot be removed here).
func (s *Store) IsPurged(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Purged[id]
}

// ClearPurge lets a re-added account out of the tombstone (same key).
func (s *Store) ClearPurge(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Purged[id] {
		delete(s.st.Purged, id)
		s.st.Generation++
	}
}

// Reconcile aligns accounts with the live host auth set: drops orphans and
// initializes newcomers, while tombstoned ids stay deleted.
func (s *Store) Reconcile(live map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	doomedBindings := map[string]bool{}
	for id := range s.st.Accounts {
		if !live[id] || s.st.Purged[id] {
			delete(s.st.Accounts, id)
			delete(s.inflight, id)
			for sid, b := range s.st.Bindings {
				if b != nil && b.AuthID == id {
					delete(s.st.Bindings, sid)
					doomedBindings[sid] = true
				}
			}
			changed = true
		}
	}
	if len(doomedBindings) > 0 {
		s.bindingLRU = dropLRU(s.bindingLRU, doomedBindings)
	}
	for id := range live {
		if s.st.Purged[id] {
			continue
		}
		if _, ok := s.st.Accounts[id]; !ok {
			s.st.Accounts[id] = &Account{Windows: map[string]*Window{}}
			changed = true
		}
	}
	if changed {
		s.st.Generation++
	}
}

// PruneBindings drops expired session bindings and their LRU entries.
func (s *Store) PruneBindings(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doomed := map[string]bool{}
	for sid, b := range s.st.Bindings {
		if b == nil || now.Sub(b.LastSeen) > BindingTTL {
			delete(s.st.Bindings, sid)
			doomed[sid] = true
		}
	}
	if len(doomed) > 0 {
		s.bindingLRU = dropLRU(s.bindingLRU, doomed)
		s.st.Generation++
	}
}

// BindingCounts returns live (non-expired) binding counts per auth id.
func (s *Store) BindingCounts(now time.Time) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int, len(s.st.Accounts))
	for _, b := range s.st.Bindings {
		if b == nil || now.Sub(b.LastSeen) > BindingTTL {
			continue
		}
		counts[b.AuthID]++
	}
	return counts
}

// Flush atomically writes state.json (0600) into the store dir.
func (s *Store) Flush() error {
	s.mu.Lock()
	if s.dir == "" {
		s.mu.Unlock()
		return nil
	}
	raw, err := json.MarshalIndent(s.st, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.dir, "state.json")
	f, err := os.CreateTemp(s.dir, "state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // 掉电后也要保住最后一份状态
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// Load reads a persisted store (inflight starts at zero by design). The
// binding LRU is rebuilt from persisted last_seen order (oldest first) so a
// full table loaded from disk evicts by real recency instead of eating the
// just-bound entry (deep-check P1).
func Load(path string) (*Store, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if st.Accounts == nil {
		st.Accounts = map[string]*Account{}
	}
	if st.Bindings == nil {
		st.Bindings = map[string]*binding{}
	}
	if st.Purged == nil {
		st.Purged = map[string]bool{}
	}
	for id, acc := range st.Accounts {
		if acc == nil {
			delete(st.Accounts, id)
			continue
		}
		if acc.Windows == nil {
			acc.Windows = map[string]*Window{}
		}
		for name, w := range acc.Windows {
			if w == nil {
				delete(acc.Windows, name)
			}
		}
	}
	for sid, b := range st.Bindings {
		if b == nil {
			delete(st.Bindings, sid)
		}
	}
	sids := make([]string, 0, len(st.Bindings))
	for sid, b := range st.Bindings {
		if b != nil {
			sids = append(sids, sid)
		}
	}
	sort.Slice(sids, func(i, j int) bool {
		a, b := st.Bindings[sids[i]].LastSeen, st.Bindings[sids[j]].LastSeen
		if !a.Equal(b) {
			return a.Before(b)
		}
		return sids[i] < sids[j]
	})
	return &Store{
		st: st, inflight: map[string]int{}, dir: filepath.Dir(path),
		bindingCap: defaultBindingCap, bindingLRU: sids,
	}, nil
}

// StartAutoFlush periodically persists the store and does a final Flush on
// stop. Without a production Flush call site the atomic-persist contract
// never fires and restarts lose every window/binding/learned limit.
func (s *Store) StartAutoFlush(interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = 5 * time.Second // time.NewTicker panics on non-positive
	}
	var once sync.Once
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.PruneBindings(time.Now())
				_ = s.Flush()
			case <-done:
				s.PruneBindings(time.Now())
				_ = s.Flush()
				return
			}
		}
	}()
	return func() {
		once.Do(func() { close(done) })
		<-exited // final Flush must land before stop returns
	}
}

// —— 管理面板支持 ——

// PurgeAccount removes all plugin-side state for an auth (hard delete from
// the operator's perspective; the host auth file itself is host-owned).
func (s *Store) PurgeAccount(id string) { s.removeAccount(id, true) }

// Forget removes a bogus/unknown record without a tombstone: used when the
// host list proves nothing (empty IDs) and a permanent tombstone would
// wrongly block a legitimate auth that appears later.
func (s *Store) Forget(id string) { s.removeAccount(id, false) }

func (s *Store) removeAccount(id string, tombstone bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Tombstone even when the record is already gone: usage events keep
	// arriving for host auths and would recreate it on the next record.
	if tombstone {
		if s.st.Purged == nil {
			s.st.Purged = map[string]bool{}
		}
		s.st.Purged[id] = true
	}
	delete(s.st.Accounts, id)
	delete(s.inflight, id)
	doomed := map[string]bool{}
	for sid, b := range s.st.Bindings {
		if b.AuthID == id {
			delete(s.st.Bindings, sid)
			doomed[sid] = true
		}
	}
	s.bindingLRU = dropLRU(s.bindingLRU, doomed)
	s.st.Generation++
}

// SetEnabled reflects operator enable/disable (soft delete = disabled).
func (s *Store) SetEnabled(id string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureAccountLocked(id)
	acc := s.st.Accounts[id]
	acc.Disabled = !enabled
	s.st.Generation++
}

// ClearWindows drops all window observations (manual recovery).
func (s *Store) ClearWindows(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureAccountLocked(id)
	s.st.Accounts[id].Windows = map[string]*Window{}
	s.st.Generation++
}

// AddUsage accumulates absolute counters on success.
func (s *Store) AddUsage(id string, input, output int64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureAccountLocked(id)
	acc := s.st.Accounts[id]
	acc.Usage.Requests++
	acc.Usage.InputTokens += input
	acc.Usage.OutputTokens += output
	t := at
	acc.Usage.LastUsedAt = &t
	s.st.Generation++
}

// NoteError records the most recent failure category.
func (s *Store) NoteError(id, code string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureAccountLocked(id)
	acc := s.st.Accounts[id]
	acc.LastError = &LastError{Code: code, At: at}
	acc.History = append([]History{{At: at, Code: code}}, acc.History...)
	if len(acc.History) > 10 {
		acc.History = acc.History[:10]
	}
	s.st.Generation++
}

// History is one history entry (newest first, capped at 10).
type History struct {
	At   time.Time `json:"at"`
	Code string    `json:"code"`
}

// ModelsCache returns the cached per-account model catalog.
func (s *Store) ModelsCache(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.st.Accounts[id]
	if acc == nil || len(acc.Models) == 0 {
		return nil
	}
	return append([]string(nil), acc.Models...)
}

// SetModelsCache stores (or clears, ids==nil) the per-account catalog.
// ModelsCacheFresh reports whether the cached catalog is younger than ttl.
func (s *Store) ModelsCacheFresh(id string, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.st.Accounts[id]
	return acc != nil && acc.ModelsCachedAt != nil && time.Since(*acc.ModelsCachedAt) < ttl
}

func (s *Store) SetModelsCache(id string, ids []string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureAccountLocked(id)
	if ids == nil {
		s.st.Accounts[id].Models = nil
	} else {
		s.st.Accounts[id].Models = append([]string(nil), ids...)
	}
	s.st.Accounts[id].ModelsCachedAt = &at
	s.st.Generation++
}

// BindingsFor counts bindings pointing at an auth (detail view).
func (s *Store) BindingsFor(authID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.st.Bindings {
		if b.AuthID == authID {
			n++
		}
	}
	return n
}

// BindingsFor on Snapshot (copy already taken).
func (snap Snapshot) BindingsFor(authID string) int {
	n := 0
	for _, id := range snap.Bindings {
		if id == authID {
			n++
		}
	}
	return n
}
