package pool

import (
	"path/filepath"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// top-K 随机：候选必须是按积分排序的前 K 个（u2=500 > u3=300 > u1=100，
// K=3 时三者均可）；候选外（积分落后的大池）绝不能命中。
func TestPickHighestCredits(t *testing.T) {
	p := New("")
	uids := []string{"u1", "u2", "u3", "u4", "u5", "u6"}
	for _, u := range uids {
		p.Add(&auth.Auth{UID: u})
	}
	for i, u := range uids {
		p.SetCredits(u, int64(600-i*100)) // u1=600 ... u6=100
	}
	top := map[string]bool{"u1": true, "u2": true, "u3": true}
	for i := 0; i < 60; i++ {
		got := p.Pick()
		if got == nil || !top[got.UID] {
			t.Fatalf("pick=%v want one of top-3 (u1/u2/u3)", got)
		}
	}
	// 冷却 u2/u3 后候选重排：top-3 = u1(600)/u4(300)/u5(200)，u6(100) 绝不命中
	p.Cooldown("u2", CoolSoft, time.Hour, "t")
	p.Cooldown("u3", CoolSoft, time.Hour, "t")
	for i := 0; i < 20; i++ {
		got := p.Pick()
		if got == nil || (got.UID != "u1" && got.UID != "u4" && got.UID != "u5") {
			t.Fatalf("pick=%v want u1/u4/u5", got)
		}
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	p.Add(a1)
	p.Add(a2)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	p.Add(a1)
	p.SetCredits("u1", 100)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("cooldown lost after reload")
	}
	st, ok := p2.Status("u1")
	if !ok || st.Reason != "余额不足" {
		t.Errorf("status=%+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "12153 session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 500)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 0)
	if p.Pick() != nil {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 2; i++ {
		p.NoteError("u1", 3, 10*time.Minute)
		if p.Pick() == nil {
			t.Fatalf("cooling too early at %d", i+1)
		}
	}
	p.NoteError("u1", 3, 10*time.Minute)
	if p.Pick() != nil {
		t.Fatal("threshold 3 should cool the account")
	}
}

func TestNoteSuccessResetsCounter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	p.NoteSuccess("u1")
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	if p.Pick() == nil {
		t.Fatal("success should reset error counter")
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestRemoveMissingFromDir(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestRecordCheckinAndRemove(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.RecordCheckin("u1", true, "ok")
	st, _ := p.Status("u1")
	if !st.LastCheckinOK || st.LastCheckinAt.IsZero() {
		t.Errorf("status=%+v", st)
	}
	p.Remove("u1")
	if _, ok := p.Status("u1"); ok {
		t.Error("account should be removed")
	}
}

// 6004 模型级冷却：只冷却触发模型，账号对其他模型保持可选；截止精确到 resetAt。
func TestCooldownSoftForModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("u1", reset, "glm-5.3", "6004 model rate limit")
	if !p.CooledForModel("u1", "glm-5.3") {
		t.Fatal("glm-5.3 should be cooling")
	}
	if p.CooledForModel("u1", "kimi-k3-1") {
		t.Fatal("other model must not be affected")
	}
	// 整号健康：其他模型请求仍可选中该账号
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("account must stay pickable for other models, got %+v", got)
	}
	// 过期自动失效
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	e2 := e // 直接改内存截止模拟过期
	p.mu.Lock()
	e2.modelCool["glm-5.3"] = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if p.CooledForModel("u1", "glm-5.3") {
		t.Fatal("expired model cooldown should clear")
	}
}

func TestPickExcludingForModelSkipsCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 10)
	p.CooldownSoftForModel("u1", time.Now().Add(time.Hour), "glm-5.3", "6004")
	got := p.PickExcludingForModel(nil, "glm-5.3")
	if got == nil || got.UID != "u2" {
		t.Fatalf("glm-5.3 pick=%+v want u2", got)
	}
	other := p.PickExcludingForModel(nil, "kimi-k3-1")
	if other == nil {
		t.Fatal("other model should still pick")
	}
	if p.PickExcludingForModel(map[string]bool{"u2": true}, "glm-5.3") != nil {
		t.Fatal("no remaining glm-5.3 account")
	}
}

// 11140 熔断：1 小时内 3 次自动禁用（防持续违规内容烧号）。
func TestNoteContentBlockCircuitBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if p.NoteContentBlock("u1") {
		t.Fatal("first block must not disable")
	}
	if p.NoteContentBlock("u1") {
		t.Fatal("second block must not disable")
	}
	if !p.NoteContentBlock("u1") {
		t.Fatal("third block within 1h should disable")
	}
	st, _ := p.Status("u1")
	if !st.Disabled || st.Reason == "" {
		t.Fatalf("should be disabled with reason: %+v", st)
	}
}
