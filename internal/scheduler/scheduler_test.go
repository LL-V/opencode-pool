package scheduler

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func ts(min int) time.Time { return time.Unix(1763700000, 0).Add(time.Duration(min) * time.Minute) }

type fakeDeps struct {
	bad              map[string]bool
	busy             map[string]bool
	near             map[string]bool
	pressure         map[string]float64
	hasPct           map[string]bool
	inflight         map[string]int
	selections       map[string]uint64
	bindTo           string // 绑定表命中
	earliestRecovery time.Time
}

func (d fakeDeps) Bad(id string) bool                 { return d.bad[id] }
func (d fakeDeps) Busy(id string) bool                { return d.busy[id] }
func (d fakeDeps) Near(id string) bool                { return d.near[id] }
func (d fakeDeps) Pressure(id string) (float64, bool) { return d.pressure[id], d.hasPct[id] }
func (d fakeDeps) Inflight(id string) int             { return d.inflight[id] }
func (d fakeDeps) Selections(id string) uint64        { return d.selections[id] }
func (d fakeDeps) EarliestRecovery() time.Time        { return d.earliestRecovery }

func (d fakeDeps) BoundAuth(sid string) (string, bool) {
	if d.bindTo == "" {
		return "", false
	}
	return d.bindTo, true
}

func cand(id string, prio int) Candidate { return Candidate{ID: id, Priority: prio} }

// 用户 2026-11-25：并发必须平均，不能全压在一个账号上。全健康时插件也必须
// 亲自选（旧“让权给宿主”导致 1.4k 请求集中在一个号）。
func TestHandlesWhenAllHealthy(t *testing.T) {
	d := fakeDeps{hasPct: map[string]bool{}}
	got := Pick(Input{Candidates: []Candidate{cand("a", 0), cand("b", 0)}, Now: ts(0)}, d)
	if !got.Handled || got.Reject || (got.AuthID != "a" && got.AuthID != "b") {
		t.Fatalf("全健康也应接管并选号：got %+v", got)
	}
}

func TestYieldBrokenByAnyFlag(t *testing.T) {
	cases := []fakeDeps{
		{bad: map[string]bool{"a": true}, hasPct: map[string]bool{}},
		{near: map[string]bool{"a": true}, hasPct: map[string]bool{}},
		{busy: map[string]bool{"a": true}, hasPct: map[string]bool{}},
	}
	for i, d := range cases {
		got := Pick(Input{Candidates: []Candidate{cand("a", 0), cand("b", 0)}, Now: ts(0)}, d)
		if !got.Handled {
			t.Fatalf("case %d：存在受损/受限账号应接管", i)
		}
	}
}

func TestRejectWhenAllBad(t *testing.T) {
	d := fakeDeps{
		bad:              map[string]bool{"a": true, "b": true},
		hasPct:           map[string]bool{},
		earliestRecovery: ts(120),
	}
	got := Pick(Input{Candidates: []Candidate{cand("a", 0), cand("b", 0)}, Now: ts(0)}, d)
	if !got.Reject || !got.Handled {
		t.Fatalf("全部 bad 应 Handled+Reject（宿主只在 Handled 时读取 Reject）：got %+v", got)
	}
	if !strings.Contains(got.RejectReason, "1763") && !strings.Contains(got.RejectReason, ":") {
		t.Fatalf("reason 应含最早恢复时间：%q", got.RejectReason)
	}
}

func TestAffinityMovesOffBusyBound(t *testing.T) {
	d := fakeDeps{
		near:   map[string]bool{"bound": true},
		busy:   map[string]bool{"bound": true},
		bindTo: "bound",
		hasPct: map[string]bool{},
	}
	got := Pick(Input{Candidates: []Candidate{cand("other", 5), cand("bound", 0)}, SessionID: "sid", Now: ts(0)}, d)
	if !got.Handled || got.AuthID != "other" {
		t.Fatalf("绑定号 busy/near 时必须迁移到健康号：got %+v", got)
	}
}

func TestAffinityFallsBackWhenBoundBad(t *testing.T) {
	// 绑定号 bad 且池内其余健康：让权（宿主轮询选号；绑定在下次成功后由
	// Executor 写入自然迁移——绝不回选 bad 号由"bound 不在 cands"保证）。
	d := fakeDeps{
		bad:    map[string]bool{"bound": true},
		bindTo: "bound",
		hasPct: map[string]bool{},
	}
	got := Pick(Input{Candidates: []Candidate{cand("healthy", 0)}, SessionID: "sid", Now: ts(0)}, d)
	if !got.Handled || got.AuthID != "healthy" {
		t.Fatalf("绑定号 bad 应改选健康号：got %+v", got)
	}
}

func TestAffinityFallsBackWhenBoundNotCandidate(t *testing.T) {
	d := fakeDeps{bindTo: "absent", hasPct: map[string]bool{}}
	got := Pick(Input{Candidates: []Candidate{cand("healthy", 0)}, SessionID: "sid", Now: ts(0)}, d)
	if !got.Handled || got.AuthID != "healthy" {
		t.Fatalf("绑定号不在候选时应改选健康号：got %+v", got)
	}
}

func TestOrderingPriorityFirst(t *testing.T) {
	d := fakeDeps{hasPct: map[string]bool{}}
	// 存在 near 账号才接管 → 用 near 触发
	d.near = map[string]bool{"low": true}
	got := Pick(Input{Candidates: []Candidate{cand("low", 0), cand("high", 3)}, Now: ts(0)}, d)
	if got.AuthID != "high" {
		t.Fatalf("Priority 高者优先：got %q", got.AuthID)
	}
}

func TestOrderingBusyDeprioritizedWithinTier(t *testing.T) {
	d := fakeDeps{busy: map[string]bool{"busy-one": true}, hasPct: map[string]bool{}}
	d.near = map[string]bool{"x": true} // 触发接管
	got := Pick(Input{Candidates: []Candidate{cand("busy-one", 5), cand("free", 5)}, Now: ts(0)}, d)
	if got.AuthID != "free" {
		t.Fatalf("同层内非 busy 优先：got %q", got.AuthID)
	}
}

func TestOrderingNearAvoidedForNewSessions(t *testing.T) {
	d := fakeDeps{near: map[string]bool{"almost": true}, hasPct: map[string]bool{}}
	got := Pick(Input{Candidates: []Candidate{cand("almost", 5), cand("fresh", 5)}, Now: ts(0)}, d)
	if got.AuthID != "fresh" {
		t.Fatalf("near 账号不作新会话首选：got %q", got.AuthID)
	}
}

func TestOrderingPressureDesc(t *testing.T) {
	d := fakeDeps{
		near:     map[string]bool{"trigger": true},
		pressure: map[string]float64{"expires-soon": 80, "long-window": 10},
		hasPct:   map[string]bool{"expires-soon": true, "long-window": true},
	}
	got := Pick(Input{Candidates: []Candidate{cand("trigger", 5), cand("expires-soon", 5), cand("long-window", 5)}, Now: ts(0)}, d)
	if got.AuthID != "expires-soon" {
		t.Fatalf("压力(剩余%%/距重置h)高者优先：got %q", got.AuthID)
	}
}

func TestOrderingInflightThenID(t *testing.T) {
	d := fakeDeps{
		near:     map[string]bool{"trigger": true},
		inflight: map[string]int{"loaded": 3, "idle-b": 1, "idle-a": 1},
		hasPct:   map[string]bool{},
	}
	got := Pick(Input{Candidates: []Candidate{cand("loaded", 5), cand("idle-b", 5), cand("idle-a", 5), cand("trigger", 5)}, Now: ts(0)}, d)
	if got.AuthID != "idle-a" {
		t.Fatalf("inflight 少者优先，再按 ID：got %q", got.AuthID)
	}
}

func TestBusyUnknownLimitIsNotBusy(t *testing.T) {
	// learned limit 未知（=0）时 busy 恒 false —— 由 deps 保证；此处验证接管逻辑不受影响
	d := fakeDeps{hasPct: map[string]bool{}}
	d.near = map[string]bool{"x": true}
	got := Pick(Input{Candidates: []Candidate{cand("x", 0), cand("y", 0)}, Now: ts(0)}, d)
	if !got.Handled || got.AuthID != "y" {
		t.Fatalf("未知上限不判 busy：got %+v", got)
	}
}

// 空候选（无账号或全部属于别的 provider）不得 Reject：宿主只认 Reject
// 会回 503，把所有非本池模型一并打死。必须让权给宿主内置选号。
func TestEmptyCandidatesYields(t *testing.T) {
	got := Pick(Input{Now: ts(0)}, fakeDeps{hasPct: map[string]bool{}})
	if got.Handled || got.Reject {
		t.Fatalf("空候选应让权：got %+v", got)
	}
}

// 随机化不变量压测：5000 轮随机状态，检查
//  1. 非空候选不得让权；
//  2. Reject 仅当全部候选都坏；
//  3. 绝不返回坏账号；
//  4. 存在非忙候选时不得返回忙账号；
//  5. 绑定账号健康（非忙非近）时必须命中亲和。
func TestPickInvariantsRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for iter := 0; iter < 5000; iter++ {
		n := rng.Intn(7) // 0..6
		d := fakeDeps{
			bad: map[string]bool{}, busy: map[string]bool{}, near: map[string]bool{},
			pressure: map[string]float64{}, hasPct: map[string]bool{},
			inflight: map[string]int{}, selections: map[string]uint64{},
		}
		var cands []Candidate
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("acct-%d", i)
			cands = append(cands, cand(id, rng.Intn(3)))
			d.bad[id] = rng.Intn(4) == 0
			d.busy[id] = rng.Intn(3) == 0
			d.near[id] = rng.Intn(5) == 0
			d.inflight[id] = rng.Intn(25)
			d.selections[id] = uint64(rng.Intn(100))
			if rng.Intn(2) == 0 {
				d.hasPct[id] = true
				d.pressure[id] = rng.Float64() * 50
			}
		}
		sid := ""
		if n > 0 && rng.Intn(3) == 0 {
			sid = "sid-x"
			d.bindTo = fmt.Sprintf("acct-%d", rng.Intn(n))
		}
		got := Pick(Input{Candidates: cands, SessionID: sid, Now: ts(iter % 1000)}, d)

		if len(cands) == 0 {
			if got.Handled || got.Reject {
				t.Fatalf("iter %d: 空候选必须让权：%+v", iter, got)
			}
			continue
		}
		if !got.Handled {
			t.Fatalf("iter %d: 非空候选不得让权", iter)
		}
		if got.Reject {
			for _, c := range cands {
				if !d.bad[c.ID] {
					t.Fatalf("iter %d: Reject 但候选 %s 不坏：%+v", iter, c.ID, got)
				}
			}
			continue
		}
		if got.AuthID == "" {
			t.Fatalf("iter %d: 既非 Reject 又没有 AuthID", iter)
		}
		if d.bad[got.AuthID] {
			t.Fatalf("iter %d: 选中了坏账号 %s", iter, got.AuthID)
		}
		anyNonBusy := false
		for _, c := range cands {
			if !d.bad[c.ID] && !d.busy[c.ID] {
				anyNonBusy = true
			}
		}
		if anyNonBusy && d.busy[got.AuthID] {
			t.Fatalf("iter %d: 存在非忙账号却选了忙账号 %s", iter, got.AuthID)
		}
		if sid != "" && d.bindTo != "" && !d.bad[d.bindTo] && !d.busy[d.bindTo] && !d.near[d.bindTo] {
			if got.AuthID != d.bindTo {
				t.Fatalf("iter %d: 绑定账号健康时应命中亲和 %s，got %s", iter, d.bindTo, got.AuthID)
			}
		}
	}
}
