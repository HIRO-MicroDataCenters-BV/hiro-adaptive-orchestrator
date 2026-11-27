package model

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/rebalance"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// PlacementDecider answers one placement request; any error makes the caller use the scorer.
type PlacementDecider interface {
	DecidePlacement(ctx context.Context,
		request placement.DecisionRequest) (placement.DecisionResponse, error)
}

// RebalanceDecider answers one rebalance request; its answer is checked against the assessment,
// and any error makes the caller use the rules' answer.
type RebalanceDecider interface {
	DecideRebalance(ctx context.Context, request placement.DecisionRequest,
		assessment rebalance.Assessment) (placement.RebalanceDecisionResponse, error)
}

// New returns the decider for cfg.Provider, or nil when the model is off; each provider's const lives in its adapter.
func New(cfg Config, scorerCfg scorer.Config, log *slog.Logger) (PlacementDecider, error) {
	switch cfg.Provider {
	case "":
		return nil, nil // model off, the scorer answers
	case ProviderKev:
		return NewKevDecider(cfg, scorerCfg, log), nil
	default:
		return nil, fmt.Errorf("unknown model provider %q", cfg.Provider)
	}
}

// NewRebalance returns the rebalance decider for cfg.Provider, or nil when the model is off;
// each flow accepts only the providers that have an adapter for it.
func NewRebalance(cfg Config, scorerCfg scorer.Config, log *slog.Logger) (RebalanceDecider, error) {
	switch cfg.Provider {
	case "":
		return nil, nil // model off, the rules answer
	default:
		return nil, fmt.Errorf("unknown rebalance model provider %q", cfg.Provider)
	}
}
