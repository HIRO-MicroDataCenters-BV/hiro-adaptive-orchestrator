package scorer

import (
	"errors"
	"fmt"
)

// Defaults; cmd/main.go overrides each from its env var.
const (
	DefaultPackedMaxUtil        = 0.85                          // under Packed, a node above this utilisation scores 0
	DefaultImbalancePenalty     = 0.2                           // under Balanced, how much a CPU/memory gap costs
	DefaultSpreadZoneWeight     = 0.6                           // under Spread, weight of "fewer app pods in the zone"
	DefaultSpreadNodeWeight     = 0.3                           // under Spread, weight of "fewer app pods on the node"
	DefaultSpreadBalancedWeight = 0.1                           // under Spread, weight of the Balanced score as tie-break
	DefaultZoneLabel            = "topology.kubernetes.io/zone" // node label that names its zone
	DefaultUnknownScore         = 50.0                          // score for a node whose usage is unknown
)

// Config holds the configuration for the scorer.
type Config struct {
	PackedMaxUtil        float64
	ImbalancePenalty     float64
	SpreadZoneWeight     float64
	SpreadNodeWeight     float64
	SpreadBalancedWeight float64
	ZoneLabel            string
	UnknownScore         float64
}

// DefaultConfig returns the scorer settings with every default applied.
func DefaultConfig() Config {
	return Config{
		PackedMaxUtil:        DefaultPackedMaxUtil,
		ImbalancePenalty:     DefaultImbalancePenalty,
		SpreadZoneWeight:     DefaultSpreadZoneWeight,
		SpreadNodeWeight:     DefaultSpreadNodeWeight,
		SpreadBalancedWeight: DefaultSpreadBalancedWeight,
		ZoneLabel:            DefaultZoneLabel,
		UnknownScore:         DefaultUnknownScore,
	}
}

// Validate rejects settings that would make scores meaningless.
func (c Config) Validate() error {
	if c.PackedMaxUtil <= 0 || c.PackedMaxUtil > 1 {
		return fmt.Errorf("PackedMaxUtil must be in (0, 1], got %v", c.PackedMaxUtil)
	}
	if c.ImbalancePenalty < 0 || c.ImbalancePenalty > 1 {
		return fmt.Errorf("ImbalancePenalty must be between 0 and 1, got %v", c.ImbalancePenalty)
	}
	if c.SpreadZoneWeight < 0 || c.SpreadZoneWeight > 1 {
		return fmt.Errorf("SpreadZoneWeight must be between 0 and 1, got %v", c.SpreadZoneWeight)
	}
	if c.SpreadNodeWeight < 0 || c.SpreadNodeWeight > 1 {
		return fmt.Errorf("SpreadNodeWeight must be between 0 and 1, got %v", c.SpreadNodeWeight)
	}
	if c.SpreadBalancedWeight < 0 || c.SpreadBalancedWeight > 1 {
		return fmt.Errorf("SpreadBalancedWeight must be between 0 and 1, got %v", c.SpreadBalancedWeight)
	}
	if c.SpreadZoneWeight+c.SpreadNodeWeight+c.SpreadBalancedWeight <= 0 {
		return fmt.Errorf(
			"the sum of SpreadZoneWeight, SpreadNodeWeight, and SpreadBalancedWeight must be greater than 0, got %v",
			c.SpreadZoneWeight+c.SpreadNodeWeight+c.SpreadBalancedWeight,
		)
	}
	if c.ZoneLabel == "" {
		return errors.New("ZoneLabel cannot be empty")
	}
	if c.UnknownScore < 0 || c.UnknownScore > 100 {
		return fmt.Errorf("UnknownScore must be between 0 and 100, got %v", c.UnknownScore)
	}
	return nil
}
