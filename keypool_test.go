package main

import (
	"testing"
	"time"
)

func TestNextRefreshTimeDST(t *testing.T) {
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	cases := []struct {
		name string
		in   time.Time
		want string // 预期刷新时刻（北京时间）
	}{
		{"DST 内（夏令时=北京15:00）", time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC), "15:00"},
		{"DST 外（冬令时=北京16:00）", time.Date(2026, 12, 20, 8, 0, 0, 0, time.UTC), "16:00"},
		{"DST 开始前一刻", time.Date(2026, 3, 8, 9, 59, 0, 0, time.UTC), "15:00"}, // 10:00 UTC 切 DST，下一午夜已是 PDT
		{"DST 开始后一刻", time.Date(2026, 3, 8, 10, 59, 0, 0, time.UTC), "15:00"},
		{"DST 结束前一刻", time.Date(2026, 11, 1, 8, 59, 0, 0, time.UTC), "16:00"}, // 09:00 UTC 结束 DST，下一午夜已是 PST
		{"DST 结束后一刻", time.Date(2026, 11, 1, 9, 1, 0, 0, time.UTC), "16:00"},
	}
	for _, c := range cases {
		got := nextRefreshTime(c.in).In(shanghai)
		if got.Format("15:04") != c.want {
			t.Errorf("%s: nextRefreshTime(%v) 北京时间 = %v, want %s",
				c.name, c.in.Format(time.RFC3339), got.Format("15:04"), c.want)
		}
	}
}

func TestPickRoundRobin(t *testing.T) {
	p := NewPool([]string{"k1", "k2", "k3"})
	got := map[string]int{}
	for i := 0; i < 6; i++ {
		k := p.Pick("m")
		if k == nil {
			t.Fatal("unexpected nil key")
		}
		got[k.id]++
	}
	for _, raw := range []string{"k1", "k2", "k3"} {
		id := shortID(raw)
		if got[id] != 2 {
			t.Errorf("key %s picked %d times, want 2", id, got[id])
		}
	}
}

func TestPickSkipsLockedAndInvalid(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	p := NewPool([]string{"k1", "k2", "k3"},
		WithClock(func() time.Time { return now }),
		WithRefresh(func(time.Time) time.Time { return now.Add(24 * time.Hour) }),
	)
	p.LockModel(shortID("k1"), "modelA", LockRPD)
	p.MarkInvalid(shortID("k2"), 400)

	for i := 0; i < 3; i++ {
		if k := p.Pick("modelA"); k == nil || k.id != shortID("k3") {
			t.Fatalf("modelA: want k3, got %v", k)
		}
	}
	// 其他模型不受 k1 的 RPD 锁影响
	if k := p.Pick("modelB"); k == nil {
		t.Fatal("modelB: k1 should be usable")
	}
}

func TestRPMLockExpires(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	cur := base
	p := NewPool([]string{"k1"},
		WithClock(func() time.Time { return cur }),
		WithRPMLock(100*time.Millisecond),
	)
	id := shortID("k1")
	p.LockModel(id, "m", LockRPM)

	if k := p.Pick("m"); k != nil {
		t.Fatal("k1 should be RPM-locked")
	}
	cur = base.Add(101 * time.Millisecond)
	if k := p.Pick("m"); k == nil {
		t.Fatal("k1 should be usable after cooldown")
	}
}

func TestTPMLockExpires(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	cur := base
	p := NewPool([]string{"k1"},
		WithClock(func() time.Time { return cur }),
		WithRPMLock(100*time.Millisecond),
	)
	id := shortID("k1")
	p.LockModel(id, "m", LockTPM)

	if k := p.Pick("m"); k != nil {
		t.Fatal("k1 should be TPM-locked")
	}
	cur = base.Add(101 * time.Millisecond)
	if k := p.Pick("m"); k == nil {
		t.Fatal("k1 should be usable after cooldown")
	}
}

func TestRPDLockUntilRefresh(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	refresh := time.Date(2026, 7, 21, 7, 0, 0, 0, time.UTC)
	p := NewPool([]string{"k1", "k2"},
		WithClock(func() time.Time { return now }),
		WithRefresh(func(time.Time) time.Time { return refresh }),
	)
	id := shortID("k1")
	p.LockModel(id, "m", LockRPD)

	if k := p.Pick("m"); k == nil || k.id != shortID("k2") {
		t.Fatal("k1 locked, want k2")
	}
	// 刷新点之后：赦免
	p.nowFn = func() time.Time { return refresh }
	if k := p.Pick("m"); k == nil || k.id != shortID("k1") {
		t.Fatal("k1 should be pardoned after refresh")
	}
}

func TestLockExtendOnly(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	p := NewPool([]string{"k1"},
		WithClock(func() time.Time { return now }),
		WithRefresh(func(time.Time) time.Time { return now.Add(24 * time.Hour) }),
		WithRPMLock(5*time.Minute),
	)
	id := shortID("k1")
	p.LockModel(id, "m", LockRPD)
	p.LockModel(id, "m", LockRPM) // 短锁不应覆盖长锁
	snap := p.Snapshot()
	lock := snap.Keys[0].Locks["m"]
	if lock.Kind != string(LockRPD) {
		t.Errorf("lock kind = %s, want rpd", lock.Kind)
	}
}

func TestAddRemoveSetState(t *testing.T) {
	p := NewPool([]string{"k1"})
	id := p.Add("k2")
	if p.Pick("m") == nil || p.Pick("m") == nil {
		t.Fatal("expected two keys")
	}
	p.Remove(shortID("k1"))
	p.SetState(id, StateDisabled)
	if k := p.Pick("m"); k != nil {
		t.Fatal("disabled key should not be picked")
	}
	p.SetState(id, StateAvailable)
	if k := p.Pick("m"); k == nil {
		t.Fatal("enabled key should be picked")
	}
}
