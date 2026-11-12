package rebalance

import (
	"fmt"
	"slices"
	"time"
)

// Defaults; cmd/main.go overrides each from its env var.
const (
	// EAO data older than this is ignored by the rules
	DefaultEAOStaleAfter = 5 * time.Minute
	// Time window to prevent flip-flop moves
	DefaultFlipMoveActionWindow = 30 * time.Minute
)

// risk conditions
var DefaultRiskConditions = []string{"MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable"}

// Config holds the configuration for the rebalance logic.
type Config struct {
	EAOStaleAfter        time.Duration
	FlipMoveActionWindow time.Duration
	RiskConditions       []string
}

// DefaultConfig returns the rebalance settings with every default applied.
func DefaultConfig() Config {
	return Config{
		EAOStaleAfter:        DefaultEAOStaleAfter,
		FlipMoveActionWindow: DefaultFlipMoveActionWindow,
		RiskConditions:       slices.Clone(DefaultRiskConditions),
	}
}

// Validate checks that the rebalance configuration is valid.
func (c Config) Validate() error {
	if c.EAOStaleAfter <= 0 {
		return fmt.Errorf("EAOStaleAfter must be > 0, got %v", c.EAOStaleAfter)
	}
	if c.FlipMoveActionWindow <= 0 {
		return fmt.Errorf("FlipMoveActionWindow must be > 0, got %v", c.FlipMoveActionWindow)
	}
	if len(c.RiskConditions) == 0 {
		return fmt.Errorf("RiskConditions must not be empty")
	}
	return nil
}
