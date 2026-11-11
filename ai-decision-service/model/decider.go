package model

import (
	"context"
	"fmt"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// PlacementDecider answers one placement request; any error makes the caller use the scorer.
type PlacementDecider interface {
	DecidePlacement(ctx context.Context,
		request placement.DecisionRequest) (placement.DecisionResponse, error)
}

// ProviderKev selects Kev's System One API.
const ProviderKev = "kev"

// New returns the decider for cfg.Provider, or nil when the model is off.
func New(cfg Config) (PlacementDecider, error) {
	switch cfg.Provider {
	case "":
		return nil, nil // model off, the scorer answers
	case ProviderKev:
		return NewKevDecider(cfg), nil
	default:
		return nil, fmt.Errorf("unknown model provider %q", cfg.Provider)
	}
}
