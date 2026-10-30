package auth

import "time"

// reserveHoldError reports that routing.reserve-gate removed every Claude candidate
// for a caller that is not exempt.
type reserveHoldError struct {
	// manual is true for routing.reserve-gate.hold.
	manual bool
	// resetAt is the earliest time a held account is expected to be released; zero if unknown.
	resetAt time.Time
}

func (e *reserveHoldError) Error() string { return "quota reserve hold" }
