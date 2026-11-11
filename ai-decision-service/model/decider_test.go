package model

import (
	"testing"
)

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
	d, err := New(Config{Provider: ""})
	if d != nil {
		t.Errorf("expected d to be nil when model is off, got %v", d)
	}
	if err != nil {
		t.Errorf("expected err to be nil when model is off, got %v", err)
	}

	// kev: a *KevDecider
	d, err = New(Config{Provider: ProviderKev, Timeout: DefaultTimeout})
	if err != nil {
		t.Errorf("expected err to be nil for KevDecider, got %v", err)
	}
	if _, ok := d.(*KevDecider); !ok {
		t.Errorf("expected d to be of type *KevDecider, got %T", d)
	}

	// unknown provider
	_, err = New(Config{Provider: "unknown"})
	if err == nil {
		t.Errorf("expected error for unknown provider, got nil")
	}
}
