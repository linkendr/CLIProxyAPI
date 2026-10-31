package config

import (
	"fmt"
	"math"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults of routing.pace.
const (
	DefaultPaceOverrideHours = 24.0
	DefaultPaceSwitchMargin  = 0.10
	// maxPaceOverrideHours is one weekly window. A larger override would put every
	// credential in the reset override and make pace behave like earliest-reset.
	maxPaceOverrideHours = 7 * 24.0
)

// PaceConfig tunes routing.strategy "pace". The strategy spreads each credential's weekly
// quota evenly over the time left in its window: it picks the credential with the most
// remaining quota per remaining hour. A credential whose weekly window resets within
// OverrideHours is spent first, so quota that is about to expire is not lost. The fields
// are pointers because zero is a valid setting: override-hours 0 turns the override off,
// switch-margin 0 always moves to the best credential. The block is read only by the pace
// strategy and hot-reloads with the other routing settings.
type PaceConfig struct {
	// OverrideHours is how close (in hours) a weekly reset must be for the credential to
	// be spent first. Nil means DefaultPaceOverrideHours; 0 turns the override off.
	OverrideHours *float64 `yaml:"override-hours,omitempty" json:"override-hours,omitempty"`
	// SwitchMargin is how much better (as a fraction) another credential's pace must be
	// before routing leaves the credential it picked last. Nil means DefaultPaceSwitchMargin.
	SwitchMargin *float64 `yaml:"switch-margin,omitempty" json:"switch-margin,omitempty"`
}

// paceConfigKeys are the keys routing.pace accepts.
var paceConfigKeys = map[string]bool{"override-hours": true, "switch-margin": true}

// UnmarshalYAML rejects unknown keys under routing.pace. A misspelled key would
// otherwise be ignored silently and the default would apply unnoticed.
func (c *PaceConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(value.Content); index += 2 {
			if key := value.Content[index].Value; !paceConfigKeys[key] {
				return fmt.Errorf("routing.pace.%s: unknown key (want override-hours or switch-margin)", key)
			}
		}
	}
	type plain PaceConfig
	var decoded plain
	if errDecode := value.Decode(&decoded); errDecode != nil {
		return fmt.Errorf("routing.pace: %w", errDecode)
	}
	*c = PaceConfig(decoded)
	return nil
}

// Settings returns the effective override window and switch margin after defaults.
func (c PaceConfig) Settings() (override time.Duration, switchMargin float64) {
	hours := DefaultPaceOverrideHours
	if c.OverrideHours != nil {
		hours = *c.OverrideHours
	}
	switchMargin = DefaultPaceSwitchMargin
	if c.SwitchMargin != nil {
		switchMargin = *c.SwitchMargin
	}
	return time.Duration(hours * float64(time.Hour)), switchMargin
}

// Validate rejects pace settings that cannot mean what they say. A typo must fail
// loudly, so the block is checked even when another strategy is selected.
func (c PaceConfig) Validate() error {
	if hours := c.OverrideHours; hours != nil && (math.IsNaN(*hours) || *hours < 0 || *hours > maxPaceOverrideHours) {
		return fmt.Errorf("routing.pace.override-hours: %v is outside [0, %v]", *hours, maxPaceOverrideHours)
	}
	if margin := c.SwitchMargin; margin != nil && (math.IsNaN(*margin) || *margin < 0 || *margin > 1) {
		return fmt.Errorf("routing.pace.switch-margin: %v is outside [0, 1]", *margin)
	}
	return nil
}
