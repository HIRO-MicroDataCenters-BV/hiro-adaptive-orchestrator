package model

import (
	"io"
	"log/slog"
	"testing"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
)

// discardLog drops every line, so tests stay quiet.
var discardLog = slog.New(slog.NewJSONHandler(io.Discard, nil))

func TestValidateConfig(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected no error when the model is off, got %v", err)
	}

	// Valid configuration should not return an error
	cfg = Config{
		Provider:  "provider",
		BaseURL:   "http://example.com",
		Path:      "/model/path",
		ModelName: "model",
		Timeout:   DefaultTimeout,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected no error for valid config, got %v", err)
	}

	// Missing BaseURL should return an error
	cfg = Config{
		Provider:  "provider",
		BaseURL:   "",
		Path:      "/model/path",
		ModelName: "model",
		Timeout:   DefaultTimeout,
	}
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error when BaseURL is empty, got nil")
	}

	// Missing Path should return an error
	cfg = Config{
		Provider:  "provider",
		BaseURL:   "http://example.com",
		Path:      "",
		ModelName: "model",
		Timeout:   DefaultTimeout,
	}
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error when Path is empty, got nil")
	}

	// Missing ModelName should return an error
	cfg = Config{
		Provider:  "provider",
		BaseURL:   "http://example.com",
		Path:      "/model/path",
		ModelName: "",
		Timeout:   DefaultTimeout,
	}
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error when ModelName is empty, got nil")
	}

	// Non-positive Timeout should return an error
	cfg = Config{
		Provider:  "provider",
		BaseURL:   "http://example.com",
		Path:      "/model/path",
		ModelName: "model",
		Timeout:   0,
	}
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error when Timeout is non-positive, got nil")
	}
}

func TestNew(t *testing.T) {
	// model off: a true nil, so the server can check decider == nil
	d, err := New(Config{Provider: ""}, scorer.DefaultConfig(), discardLog)
	if d != nil {
		t.Errorf("expected d to be nil when model is off, got %v", d)
	}
	if err != nil {
		t.Errorf("expected err to be nil when model is off, got %v", err)
	}

	// kev: a *KevDecider
	d, err = New(Config{Provider: ProviderKev, Timeout: DefaultTimeout}, scorer.DefaultConfig(), discardLog)
	if err != nil {
		t.Errorf("expected err to be nil for KevDecider, got %v", err)
	}
	if _, ok := d.(*KevDecider); !ok {
		t.Errorf("expected d to be of type *KevDecider, got %T", d)
	}

	// unknown provider
	_, err = New(Config{Provider: "unknown"}, scorer.DefaultConfig(), discardLog)
	if err == nil {
		t.Errorf("expected error for unknown provider, got nil")
	}
}

func TestNewRebalance(t *testing.T) {
	// model off: a true nil, so the server can check decider == nil
	d, err := NewRebalance(DefaultRebalanceConfig(), scorer.DefaultConfig(), discardLog)
	if d != nil || err != nil {
		t.Errorf("model off: got %v, %v, want nil, nil", d, err)
	}

	// a provider without a rebalance adapter
	if _, err := NewRebalance(Config{Provider: "unknown"}, scorer.DefaultConfig(), discardLog); err == nil {
		t.Error("unknown provider: got no error, want one")
	}
}

func TestDefaultRebalanceConfig(t *testing.T) {
	cfg := DefaultRebalanceConfig()
	if cfg.Timeout != DefaultRebalanceTimeout {
		t.Errorf("got timeout %v, want %v", cfg.Timeout, DefaultRebalanceTimeout)
	}
	if cfg.Provider != DefaultProvider {
		t.Errorf("got provider %q, want the model off", cfg.Provider)
	}
}
