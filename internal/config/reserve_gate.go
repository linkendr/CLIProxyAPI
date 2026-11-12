package config

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Default utilization level of routing.reserve-gate (fraction of a usage window).
const (
	DefaultReserveGateThreshold = 0.95
	// DefaultReserveGateStateFileName is the state file kept in auth-dir. It is not a
	// .json file, so the auth watcher and loader never read it as a credential.
	DefaultReserveGateStateFileName = "reserve-gate.state"
	// DefaultReserveGateWindDownMinutes is how long, after every Max login first
	// became held, a running task of a caller that is not exempt may still finish.
	DefaultReserveGateWindDownMinutes = 15
	// DefaultReserveGateNearResetHours is how long before a Max login's weekly reset its
	// limit rises to DefaultReserveGateNearResetThreshold.
	DefaultReserveGateNearResetHours = 6
	// DefaultReserveGateNearResetThreshold is the limit of a Max login near its reset.
	DefaultReserveGateNearResetThreshold = 0.99
	// DefaultReserveGateFinalReleaseHours is how long before a Max login's weekly reset
	// its limit is 1: nothing is kept, every caller may use what is left.
	DefaultReserveGateFinalReleaseHours = 1
)

// ReserveGateConfig keeps the last part of the Claude Max logins' 7-day usage for
// exempt callers (automations). A Max login is held while its latest 7d utilization is
// at or above its limit, which steps up as the login's own weekly reset comes near:
// Threshold while more than NearResetHours remain, NearResetThreshold from then on, and
// 1 in the final FinalReleaseHours. A login with no known reset keeps Threshold. The reserve is shared: callers that
// are not exempt are kept off held logins only while every Max login is held (or at a
// real upstream limit); while one still has room, they may use any of them. Paid credit
// keys (fallback-when) serve those callers only under their own fallback-when rule, and
// Hold pauses every Claude credential for them. While every Max login is held, a running
// task (the request answers a tool call) of such a caller is still served for
// WindDownMinutes, with a note asking the model to wrap up; new prompts are refused.
// Other providers and exempt callers are unaffected. Enabled false means no change at all.
type ReserveGateConfig struct {
	// Enabled turns the gate on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Hold pauses every Claude credential for callers that are not exempt.
	Hold bool `yaml:"hold,omitempty" json:"hold,omitempty"`
	// Threshold is the 7d utilization (0-1) at or above which a Max login is held.
	// Zero means DefaultReserveGateThreshold.
	Threshold float64 `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	// ExemptAPIKeys lists the client keys (access api-keys) the gate never applies to.
	ExemptAPIKeys []string `yaml:"exempt-api-keys,omitempty" json:"exempt-api-keys,omitempty"`
	// StateFile is where the gate keeps its last readings and holds so a restart does
	// not forget them. Empty means DefaultReserveGateStateFileName in auth-dir.
	StateFile string `yaml:"state-file,omitempty" json:"state-file,omitempty"`
	// WindDownMinutes is how long after every Max login first became held a running
	// task of a caller that is not exempt is still served. Unset means
	// DefaultReserveGateWindDownMinutes; 0 refuses running tasks at once.
	WindDownMinutes *int `yaml:"wind-down-minutes,omitempty" json:"wind-down-minutes,omitempty"`
	// NearResetHours is how long before a login's weekly reset its limit rises to
	// NearResetThreshold. Unset means DefaultReserveGateNearResetHours; 0 turns it off.
	NearResetHours *int `yaml:"near-reset-hours,omitempty" json:"near-reset-hours,omitempty"`
	// NearResetThreshold is the limit (0-1) near the reset; a Threshold above it wins.
	// Zero means DefaultReserveGateNearResetThreshold.
	NearResetThreshold float64 `yaml:"near-reset-threshold,omitempty" json:"near-reset-threshold,omitempty"`
	// FinalReleaseHours is how long before a login's weekly reset its limit is 1. Unset
	// means DefaultReserveGateFinalReleaseHours; 0 turns it off.
	FinalReleaseHours *int `yaml:"final-release-hours,omitempty" json:"final-release-hours,omitempty"`
}

// EffectiveNearReset returns NearResetHours after its default.
func (c ReserveGateConfig) EffectiveNearReset() time.Duration {
	if c.NearResetHours == nil {
		return DefaultReserveGateNearResetHours * time.Hour
	}
	return time.Duration(*c.NearResetHours) * time.Hour
}

// EffectiveNearResetThreshold returns NearResetThreshold after its default.
func (c ReserveGateConfig) EffectiveNearResetThreshold() float64 {
	if c.NearResetThreshold == 0 {
		return DefaultReserveGateNearResetThreshold
	}
	return c.NearResetThreshold
}

// EffectiveFinalRelease returns FinalReleaseHours after its default.
func (c ReserveGateConfig) EffectiveFinalRelease() time.Duration {
	if c.FinalReleaseHours == nil {
		return DefaultReserveGateFinalReleaseHours * time.Hour
	}
	return time.Duration(*c.FinalReleaseHours) * time.Hour
}

// EffectiveWindDown returns WindDownMinutes after its default.
func (c ReserveGateConfig) EffectiveWindDown() time.Duration {
	if c.WindDownMinutes == nil {
		return DefaultReserveGateWindDownMinutes * time.Minute
	}
	return time.Duration(*c.WindDownMinutes) * time.Minute
}

// EffectiveThreshold returns Threshold after its default.
func (c ReserveGateConfig) EffectiveThreshold() float64 {
	if c.Threshold == 0 {
		return DefaultReserveGateThreshold
	}
	return c.Threshold
}

// Validate rejects a reserve gate that cannot mean what it says. A typo must fail
// loudly: a silently ignored gate would let interactive use spend the reserve.
func (c ReserveGateConfig) Validate() error {
	threshold := c.EffectiveThreshold()
	if math.IsNaN(threshold) || threshold <= 0 || threshold > 1 {
		return fmt.Errorf("routing.reserve-gate.threshold: %v is outside (0, 1]", c.Threshold)
	}
	if c.WindDownMinutes != nil && (*c.WindDownMinutes < 0 || *c.WindDownMinutes > 24*60) {
		return fmt.Errorf("routing.reserve-gate.wind-down-minutes: %d is outside [0, 1440]", *c.WindDownMinutes)
	}
	if c.NearResetHours != nil && (*c.NearResetHours < 0 || *c.NearResetHours > 7*24) {
		return fmt.Errorf("routing.reserve-gate.near-reset-hours: %d is outside [0, 168]", *c.NearResetHours)
	}
	nearThreshold := c.EffectiveNearResetThreshold()
	if math.IsNaN(nearThreshold) || nearThreshold <= 0 || nearThreshold > 1 {
		return fmt.Errorf("routing.reserve-gate.near-reset-threshold: %v is outside (0, 1]", c.NearResetThreshold)
	}
	if c.FinalReleaseHours != nil && (*c.FinalReleaseHours < 0 || *c.FinalReleaseHours > 7*24) {
		return fmt.Errorf("routing.reserve-gate.final-release-hours: %d is outside [0, 168]", *c.FinalReleaseHours)
	}
	if near, final := c.EffectiveNearReset(), c.EffectiveFinalRelease(); near > 0 && final > near {
		return fmt.Errorf("routing.reserve-gate.final-release-hours: %v is longer than near-reset-hours %v", final, near)
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

// validateReserveGateYAML rejects a key under routing.reserve-gate that the gate does not
// know. The config loader ignores unknown keys, but a misspelt or removed reserve key
// would silently change who may spend the reserve, so it must fail loudly.
func validateReserveGateYAML(data []byte) error {
	var document yaml.Node
	if errUnmarshal := yaml.Unmarshal(data, &document); errUnmarshal != nil || len(document.Content) == 0 {
		return nil
	}
	node := yamlPath(expandConfigAliases(document.Content[0]), "routing.reserve-gate")
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	encoded, errMarshal := yaml.Marshal(node)
	if errMarshal != nil {
		return nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	var gate ReserveGateConfig
	if errDecode := decoder.Decode(&gate); errDecode != nil {
		return fmt.Errorf("routing.reserve-gate: %w", errDecode)
	}
	return nil
}
