package auth

import (
	"testing"
	"time"
)

// The reserve is one amount for all Max logins: 1 - the limit for the time left to the
// next known reset of any login, placed on the logins that reset last. A login is held
// when all its headroom is reserve, and released at the first limit step that frees it,
// or at that next reset.
func TestReserveGatePoolHolds(t *testing.T) {
	gate := compileReserveGate(reserveGateOn(nil))
	now := time.Date(2026, 11, 12, 14, 14, 0, 0, time.UTC)
	work := now.Add(8*time.Hour + 46*time.Minute)
	general := now.Add(26*time.Hour + 46*time.Minute)
	type want struct {
		held  bool
		until time.Time
	}
	for name, tt := range map[string]struct {
		now     time.Time
		entries []reservePoolEntry
		want    []want
	}{
		"earlier login spent while the later one holds the reserve": {
			now:     now,
			entries: []reservePoolEntry{{id: "work", headroom: 1 - 0.98, resetAt: work}, {id: "general", headroom: 1 - 0.96, resetAt: general}},
			want:    []want{{false, time.Time{}}, {true, work.Add(-6 * time.Hour)}},
		},
		"pool at the reserve holds every login until the 99% step": {
			now:     now,
			entries: []reservePoolEntry{{id: "work", headroom: 1 - 0.99, resetAt: work}, {id: "general", headroom: 1 - 0.96, resetAt: general}},
			want:    []want{{true, work.Add(-6 * time.Hour)}, {true, work.Add(-6 * time.Hour)}},
		},
		// An empty login stays held until its own reset whatever the reserve is.
		"near the next reset the reserve shrinks to 1%": {
			now:     work.Add(-5 * time.Hour),
			entries: []reservePoolEntry{{id: "work", headroom: 0, resetAt: work}, {id: "general", headroom: 1 - 0.96, resetAt: general}},
			want:    []want{{true, work}, {false, time.Time{}}},
		},
		"pool at the 1% reserve is held until the final hour": {
			now:     work.Add(-5 * time.Hour),
			entries: []reservePoolEntry{{id: "work", headroom: 0, resetAt: work}, {id: "general", headroom: 1 - 0.99, resetAt: general}},
			want:    []want{{true, work}, {true, work.Add(-time.Hour)}},
		},
		"empty pool is held until the next reset": {
			now:     work.Add(-30 * time.Minute),
			entries: []reservePoolEntry{{id: "work", headroom: 0, resetAt: work}, {id: "general", headroom: 0, resetAt: general}},
			want:    []want{{true, work}, {true, work}},
		},
		"final hour: nothing is reserve": {
			now:     work.Add(-30 * time.Minute),
			entries: []reservePoolEntry{{id: "work", headroom: 0, resetAt: work}, {id: "general", headroom: 1 - 0.99, resetAt: general}},
			want:    []want{{true, work}, {false, time.Time{}}},
		},
		"after the earlier reset the fresh login holds the reserve": {
			now:     work.Add(time.Minute),
			entries: []reservePoolEntry{{id: "work", headroom: 1, resetAt: work.Add(7 * 24 * time.Hour)}, {id: "general", headroom: 1 - 0.96, resetAt: general}},
			want:    []want{{false, time.Time{}}, {false, time.Time{}}},
		},
		"one login keeps the per-login rule": {
			now:     now,
			entries: []reservePoolEntry{{id: "work", headroom: 1 - 0.95, resetAt: work}},
			want:    []want{{true, work.Add(-6 * time.Hour)}},
		},
		"no known reset keeps 5% with no known release": {
			now:     now,
			entries: []reservePoolEntry{{id: "a", headroom: 1 - 0.98}, {id: "b", headroom: 1 - 0.97}},
			want:    []want{{true, time.Time{}}, {true, time.Time{}}},
		},
		"an unknown reset holds the reserve before a known one": {
			now:     now,
			entries: []reservePoolEntry{{id: "known", headroom: 1 - 0.97, resetAt: general}, {id: "unknown", headroom: 1 - 0.95}},
			want:    []want{{false, time.Time{}}, {true, general.Add(-6 * time.Hour)}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := gate.poolHolds(tt.entries, tt.now)
			for i, w := range tt.want {
				if got[i].held != w.held || !got[i].until.Equal(w.until) {
					t.Fatalf("%s: hold %+v, want held %v until %v", tt.entries[i].id, got[i], w.held, w.until)
				}
			}
		})
	}
}

// Through the manager: two logins above their own 95% still serve ordinary callers while
// together they have more than the 5% reserve. Once the pool is down to the reserve, every
// login is held and exempt callers keep it.
func TestReserveGatePoolServesUntilReserveLeft(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		aReset := time.Now().Add(9 * time.Hour)
		rig.observe(t, rig.a, "7d", 0.98, aReset)
		rig.observe(t, rig.b, "7d", 0.96, time.Now().Add(26*time.Hour))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a]+seen[rig.b] != 4 {
			t.Fatalf("ordinary picks = %v, want Max logins: 6%% left is above the 5%% reserve", seen)
		}

		rig.observe(t, rig.a, "7d", 0.99, aReset)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		requireReserveHold(t, err)
		// The reserve drops to 1% six hours before the earlier reset.
		requireRetryAfterNear(t, err, aReset.Add(-6*time.Hour))
		if seen := rig.picked(t, reserveTestExemptKey, 3); seen[rig.a]+seen[rig.b] != 3 {
			t.Fatalf("exempt picks = %v, want Max logins", seen)
		}
	})
}

// The book merges the readings of every login and stores the pool's hold of each.
func TestReserveGateBookEvaluatesThePool(t *testing.T) {
	gate := compileReserveGate(reserveGateOn(nil))
	var book reserveHoldBook
	now := time.Now()
	login := func(id string, utilization float64, reset time.Time) reserveLogin {
		l := reserveLogin{id: id}
		l.readings[0] = reserveReading{utilization: utilization, resetAt: reset, observedAt: now}
		l.ok[0] = true
		return l
	}
	accounts := book.evaluate(gate, now, []reserveLogin{
		login("work", 0.98, now.Add(9*time.Hour)),
		login("general", 0.96, now.Add(26*time.Hour)),
		{id: "fresh"},
	})
	if accounts["work"].held || !accounts["general"].held || accounts["fresh"].held {
		t.Fatalf("accounts = %+v, want only general held", accounts)
	}
	// The book keeps the readings: a later evaluation without live readings decides the same.
	again := book.evaluate(gate, now, []reserveLogin{{id: "work"}, {id: "general"}})
	if again["work"].held || !again["general"].held {
		t.Fatalf("accounts from the book = %+v, want only general held", again)
	}
	// A login at a real limit has no headroom whatever its reading says.
	limited := login("work", 0.50, now.Add(9*time.Hour))
	limited.atLimit = true
	if accounts := book.evaluate(gate, now, []reserveLogin{limited, {id: "general"}}); !accounts["work"].held || !accounts["general"].held {
		t.Fatalf("accounts = %+v, want both held: 4%% left is below the reserve", accounts)
	}
}
