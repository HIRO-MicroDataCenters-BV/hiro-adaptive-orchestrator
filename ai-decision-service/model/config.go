package model

import (
	"fmt"
	"time"
)

// Defaults; cmd/main.go overrides each from its env var.
const (
	DefaultProvider  = "" // empty = model off, the scorer answers
	DefaultBaseURL   = "" // no default address, it's set when a provider is chosen
	DefaultPath      = ""
	DefaultModelName = ""
	DefaultTimeout   = 5 * time.Second // must stay under the operator's DECISION_AGENT_TIMEOUT (8s)
	// one review asks about every pod; must stay under the operator's REBALANCE_DECISION_TIMEOUT (60s)
	DefaultRebalanceTimeout = 30 * time.Second
)

// Config holds the configuration for the model.
type Config struct {
	Provider  string
	BaseURL   string
	Path      string
	ModelName string
	Timeout   time.Duration
}

// DefaultConfig returns the default configuration for the model.
func DefaultConfig() Config {
	return Config{
		Provider:  DefaultProvider,
		BaseURL:   DefaultBaseURL,
		Path:      DefaultPath,
		ModelName: DefaultModelName,
		Timeout:   DefaultTimeout,
	}
}

// DefaultRebalanceConfig is DefaultConfig with the longer rebalance timeout.
func DefaultRebalanceConfig() Config {
	cfg := DefaultConfig()
	cfg.Timeout = DefaultRebalanceTimeout
	return cfg
}

// Validate checks if the configuration is valid.
// If the provider is empty, it considers the model off and returns no error.
// Otherwise, it ensures all fields are set.
func (c Config) Validate() error {
	if c.Provider == "" {
		return nil // model off, nothing else is needed
	}
	if c.BaseURL == "" {
		return fmt.Errorf("base URL is required")
	}
	if c.Path == "" {
		return fmt.Errorf("path is required")
	}
	if c.ModelName == "" {
		return fmt.Errorf("model name is required")
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	return nil
}
