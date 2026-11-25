package scheduler

import (
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
	bindTo           string // 绑定表命中
	earliestRecovery time.Time
}

func (d fakeDeps) Bad(id string) bool                 { return d.bad[id] }
func (d fakeDeps) Busy(id string) bool                { return d.busy[id] }
func (d fakeDeps) Near(id string) bool                { return d.near[id] }
func (d fakeDeps) Pressure(id string) (float64, bool) { return d.pressure[id], d.hasPct[id] }
func (d fakeDeps) Inflight(id string) int             { return d.inflight[id] }
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
