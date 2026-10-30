package config

import (
	"fmt"
	"math"
	"strings"
)

// Default utilization levels of routing.reserve-gate (fractions of a usage window).
const (
	DefaultReserveGateThreshold   = 0.95
	DefaultReserveGateResumeBelow = 0.90
)

// ReserveGateConfig keeps the last part of every Claude account's 5-hour and 7-day
// usage windows for exempt callers (automations). For every other caller a Claude
// account is not selected while its latest utilization of either window is held:
// at or above Threshold, and, once held, until it drops below ResumeBelow or the
// window resets. Paid credit keys (fallback-when) are never selected for those
// callers, and Hold pauses every Claude credential for them. Other providers and
// exempt callers are unaffected. Enabled false means no change at all.
type ReserveGateConfig struct {
	// Enabled turns the gate on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Hold pauses every Claude credential for callers that are not exempt.
	Hold bool `yaml:"hold,omitempty" json:"hold,omitempty"`
	// Threshold is the utilization (0-1) at or above which an account window is held.
	// Zero means DefaultReserveGateThreshold.
	Threshold float64 `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	// ResumeBelow is the utilization (0-1) below which a held window is released.
	// Zero means DefaultReserveGateResumeBelow, or Threshold when that is lower.
	ResumeBelow float64 `yaml:"resume-below,omitempty" json:"resume-below,omitempty"`
	// ExemptAPIKeys lists the client keys (access api-keys) the gate never applies to.
	ExemptAPIKeys []string `yaml:"exempt-api-keys,omitempty" json:"exempt-api-keys,omitempty"`
}

// Levels returns the effective threshold and resume level after defaults.
func (c ReserveGateConfig) Levels() (threshold, resumeBelow float64) {
	threshold = c.Threshold
	if threshold == 0 {
		threshold = DefaultReserveGateThreshold
	}
	resumeBelow = c.ResumeBelow
	if resumeBelow == 0 {
		resumeBelow = math.Min(DefaultReserveGateResumeBelow, threshold)
	}
	return threshold, resumeBelow
}

// Validate rejects a reserve gate that cannot mean what it says. A typo must fail
// loudly: a silently ignored gate would let interactive use spend the reserve.
func (c ReserveGateConfig) Validate() error {
	threshold, resumeBelow := c.Levels()
	if math.IsNaN(threshold) || threshold <= 0 || threshold > 1 {
		return fmt.Errorf("routing.reserve-gate.threshold: %v is outside (0, 1]", c.Threshold)
	}
	if math.IsNaN(resumeBelow) || resumeBelow <= 0 || resumeBelow > threshold {
		return fmt.Errorf("routing.reserve-gate.resume-below: %v must be above 0 and at most threshold %v", c.ResumeBelow, threshold)
	}
	if c.Hold && !c.Enabled {
		return fmt.Errorf("routing.reserve-gate.hold: requires routing.reserve-gate.enabled: true")
	}
	for index, key := range c.ExemptAPIKeys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("routing.reserve-gate.exempt-api-keys[%d]: empty key", index)
		}
	}
	return nil
}
