package rebalance

import (
	"fmt"
	"time"
)

// Defaults; cmd/main.go overrides each from its env var.
const (
	DefaultEAOStaleAfter        = 5 * time.Minute  // EAO data older than this is ignored by the rules
	DefaultFlipMoveActionWindow = 30 * time.Minute // Time window to prevent flip-flop moves
)

// Config holds the configuration for the rebalance logic.
type Config struct {
	EAOStaleAfter        time.Duration
	FlipMoveActionWindow time.Duration
}

// DefaultConfig returns the rebalance settings with every default applied.
func DefaultConfig() Config {
	return Config{
		EAOStaleAfter:        DefaultEAOStaleAfter,
		FlipMoveActionWindow: DefaultFlipMoveActionWindow,
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
	return nil
}
