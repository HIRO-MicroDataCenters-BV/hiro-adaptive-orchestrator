package server

import (
	"log/slog"
	"time"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/model"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/rebalance"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
)

// Defaults; cmd/main.go overrides each from its env var.
const (
	DefaultPort         = 8080
	DefaultHealthPath   = "/healthz"
	DefaultDecisionPath = "/api/v1/agent/placement/decision"
	DefaultReadTimeout  = 5 * time.Second
	DefaultMaxBodyBytes = 5 * 1024 * 1024 // 5 MiB
	DefaultLogLevel     = slog.LevelInfo
)

// Config holds every tunable of the service.
type Config struct {
	Port            int
	HealthPath      string
	DecisionPath    string
	ReadTimeout     time.Duration
	MaxBodyBytes    int64
	ScorerConfig    scorer.Config
	RebalanceConfig rebalance.Config
	ModelConfig     model.Config // placement's model
	// RebalanceModelConfig is the rebalance flow's own model, so each flow can use a different one
	RebalanceModelConfig model.Config
	LogLevel             slog.Level
}

func DefaultConfig() Config {
	return Config{
		Port:                 DefaultPort,
		HealthPath:           DefaultHealthPath,
		DecisionPath:         DefaultDecisionPath,
		ReadTimeout:          DefaultReadTimeout,
		MaxBodyBytes:         DefaultMaxBodyBytes,
		ScorerConfig:         scorer.DefaultConfig(),
		RebalanceConfig:      rebalance.DefaultConfig(),
		ModelConfig:          model.DefaultConfig(),
		RebalanceModelConfig: model.DefaultRebalanceConfig(),
		LogLevel:             DefaultLogLevel,
	}
}
