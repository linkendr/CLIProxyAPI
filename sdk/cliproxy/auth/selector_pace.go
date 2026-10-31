package auth

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// RoutingStrategyPace is the routing.strategy value that selects PaceSelector.
const RoutingStrategyPace = "pace"

// PaceSelector spreads each credential's weekly quota over the time left in its window.
// A credential's pace is its remaining weekly quota per remaining hour: (1 - utilization)
// / hours until reset, from the Anthropic 7d window and, when the requested model's state
// carries it, the model-scoped 7d_oi window. The lower of the two counts, and the earlier
// of their resets is the credential's weekly reset. Readings come from the passive quota
// snapshot like EarliestResetSelector's; the newest 7d reading wins whatever model it came
// from, and 7d_oi is read only from the requested model's state.
//
// Each pick, in order:
//   - Reset override: credentials whose weekly reset is within overrideWindow, and that
//     still have quota left, are spent first. Among them the earliest reset second wins,
//     then the highest pace, then the credentials rotate round-robin.
//   - Otherwise the highest pace wins. The credential picked last for the provider and
//     model is kept while its pace is at least the best pace / (1 + switchMargin), so
//     routing does not flap between credentials of nearly the same pace (and loses no
//     prompt cache). A pick that leaves it takes the highest pace; on an exact tie the
//     credential after it in ID order.
//
// A credential with no known weekly window (no future reset with a utilization) is probed
// the same way as under EarliestResetSelector: first, at most once per
// earliestResetProbeTTL for a provider and model, then it ranks behind known credentials.
// When no credential has a known window, all of them rotate round-robin. Providers that
// send no Anthropic utilization headers (Codex, Gemini) therefore rotate.
type PaceSelector struct {
	mu sync.Mutex
	resetProbeState
	overrideWindow time.Duration
	switchMargin   float64
	now            func() time.Time
}

// NewPaceSelector returns a PaceSelector with the given reset override window (zero turns
// the override off) and switch margin (a fraction; zero always moves to the best pace).
func NewPaceSelector(overrideWindow time.Duration, switchMargin float64) *PaceSelector {
	return &PaceSelector{overrideWindow: overrideWindow, switchMargin: switchMargin}
}

// pacedAuth is a credential with at least one known weekly window.
type pacedAuth struct {
	auth  *Auth
	pace  float64   // remaining quota fraction per hour; the minimum over its windows
	reset time.Time // earliest reset of its windows
}

// Pick selects an unprobed credential with no known weekly window, else the credential the
// reset override or the pace ranking prefers.
func (s *PaceSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.begin(key, now)

	var known []pacedAuth
	var unknown, unprobed []*Auth
	for _, auth := range available {
		pace, reset, ok := weeklyPace(auth, model, now)
		if !ok {
			unknown = append(unknown, auth)
			if !s.probedRecently(key, auth, now) {
				unprobed = append(unprobed, auth)
			}
			continue
		}
		known = append(known, pacedAuth{auth: auth, pace: pace, reset: reset})
	}

	var picked *Auth
	switch {
	case len(unprobed) > 0:
		// The probe time, not the rotation state, moves the next pick on to the next
		// unprobed credential; lastPicked keeps the credential the margin protects.
		sortAuthsByID(unprobed)
		picked = unprobed[0]
		s.markProbed(key, picked, now)
		return picked, nil
	case len(known) == 0:
		sortAuthsByID(unknown)
		picked = unknown[successorIndex(unknown, s.lastPicked[key])]
	default:
		picked = s.rankPaced(known, s.lastPicked[key], now)
	}
	s.lastPicked[key] = picked.ID
	return picked, nil
}

// rankPaced applies the reset override, then the pace ranking with the switch margin.
// lastID is the credential picked last for this provider and model.
func (s *PaceSelector) rankPaced(known []pacedAuth, lastID string, now time.Time) *Auth {
	var override []pacedAuth
	if s.overrideWindow > 0 {
		for _, candidate := range known {
			if candidate.pace > 0 && candidate.reset.Sub(now) <= s.overrideWindow {
				override = append(override, candidate)
			}
		}
	}
	if len(override) > 0 {
		earliest := override[0].reset.Unix()
		for _, candidate := range override[1:] {
			if sec := candidate.reset.Unix(); sec < earliest {
				earliest = sec
			}
		}
		var soonest []pacedAuth
		for _, candidate := range override {
			if candidate.reset.Unix() == earliest {
				soonest = append(soonest, candidate)
			}
		}
		return fastestPaced(soonest, lastID)
	}

	best := known[0].pace
	for _, candidate := range known[1:] {
		if candidate.pace > best {
			best = candidate.pace
		}
	}
	for _, candidate := range known {
		if candidate.auth.ID == lastID && candidate.pace >= best/(1+s.switchMargin) {
			return candidate.auth
		}
	}
	return fastestPaced(known, lastID)
}

// fastestPaced returns the credential with the highest pace; equal paces rotate by ID
// after lastID.
func fastestPaced(candidates []pacedAuth, lastID string) *Auth {
	best := candidates[0].pace
	for _, candidate := range candidates[1:] {
		if candidate.pace > best {
			best = candidate.pace
		}
	}
	var tied []*Auth
	for _, candidate := range candidates {
		if candidate.pace == best {
			tied = append(tied, candidate.auth)
		}
	}
	sortAuthsByID(tied)
	return tied[successorIndex(tied, lastID)]
}

// weeklyPace returns the credential's pace and weekly reset for model: the minimum pace
// and the earliest reset over its known weekly windows (7d, and 7d_oi from the requested
// model's state). A window is known when its newest reading has a utilization and a
// reset in the future; a reset that has passed describes a window that already rolled
// over. ok is false when no window is known.
func weeklyPace(auth *Auth, model string, now time.Time) (pace float64, reset time.Time, ok bool) {
	if auth == nil {
		return 0, time.Time{}, false
	}
	consider := func(reading reserveReading, found bool) {
		if !found || !reading.resetAt.After(now) {
			return
		}
		remaining := math.Min(math.Max(1-reading.utilization, 0), 1)
		windowPace := remaining / reading.resetAt.Sub(now).Hours()
		if !ok || windowPace < pace {
			pace = windowPace
		}
		if !ok || reading.resetAt.Before(reset) {
			reset = reading.resetAt
		}
		ok = true
	}
	consider(latestReserveReading(auth, "7d"))
	consider(latestModelReading(auth, model, "7d_oi"))
	return pace, reset, ok
}

// latestModelReading returns the newest reading of window in the states of the requested
// model only. Model-scoped windows (7d_oi) describe that model; the credential snapshot
// may carry another model's.
func latestModelReading(auth *Auth, model, window string) (reserveReading, bool) {
	var best reserveReading
	found := false
	if model == "" {
		return best, false
	}
	modelKey := canonicalModelKey(model)
	for stateModel, state := range auth.ModelStates {
		if state == nil || canonicalModelKey(stateModel) != modelKey {
			continue
		}
		reading, ok := reserveReadingFromSignals(state.Quota, window)
		if ok && (!found || reading.observedAt.After(best.observedAt)) {
			best, found = reading, true
		}
	}
	return best, found
}

func sortAuthsByID(auths []*Auth) {
	sort.SliceStable(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
}
