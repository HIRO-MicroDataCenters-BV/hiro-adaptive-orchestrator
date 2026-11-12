package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/model"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/server"
)

func main() {
	cfg, err := loadConfig()
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	if err != nil {
		log.Error("Failed to load configuration", slog.Any("error", err))
		os.Exit(1)
	}

	placementDecider, err := model.New(cfg.ModelConfig, log)
	if err != nil {
		log.Error("Failed to create placement decider", slog.Any("error", err))
		os.Exit(1)
	}

	srv := server.New(cfg, placementDecider, log)
	log.Info("Starting AI Decision Service", "config", cfg)
	err = srv.Start()
	if err != nil {
		log.Error("Failed to start AI Decision Service", slog.Any("error", err))
		os.Exit(1)
	}
}

func loadConfig() (server.Config, error) {
	cfg := server.DefaultConfig()
	var err error
	cfg.Port, err = envInt("AI_DECISION_SERVICE_PORT", cfg.Port)
	if err != nil {
		return cfg, err
	}
	cfg.ReadTimeout, err = envDuration("AI_DECISION_SERVICE_READ_TIMEOUT", cfg.ReadTimeout)
	if err != nil {
		return cfg, err
	}
	maxBytes, err := envInt("AI_DECISION_SERVICE_MAX_BODY_BYTES", int(cfg.MaxBodyBytes))
	if err != nil {
		return cfg, err
	}
	cfg.MaxBodyBytes = int64(maxBytes)
	cfg.HealthPath = envString("AI_DECISION_SERVICE_HEALTH_PATH", cfg.HealthPath)
	cfg.DecisionPath = envString("AI_DECISION_SERVICE_DECISION_PATH", cfg.DecisionPath)
	cfg.ScorerConfig.PackedMaxUtil, err = envFloat("AI_DECISION_SERVICE_SCORER_PACKED_MAX_UTIL",
		cfg.ScorerConfig.PackedMaxUtil)
	if err != nil {
		return cfg, err
	}
	cfg.ScorerConfig.ImbalancePenalty, err = envFloat("AI_DECISION_SERVICE_SCORER_IMBALANCE_PENALTY",
		cfg.ScorerConfig.ImbalancePenalty)
	if err != nil {
		return cfg, err
	}
	cfg.ScorerConfig.SpreadZoneWeight, err = envFloat("AI_DECISION_SERVICE_SCORER_SPREAD_ZONE_WEIGHT",
		cfg.ScorerConfig.SpreadZoneWeight)
	if err != nil {
		return cfg, err
	}
	cfg.ScorerConfig.SpreadNodeWeight, err = envFloat("AI_DECISION_SERVICE_SCORER_SPREAD_NODE_WEIGHT",
		cfg.ScorerConfig.SpreadNodeWeight)
	if err != nil {
		return cfg, err
	}
	cfg.ScorerConfig.SpreadBalancedWeight, err = envFloat("AI_DECISION_SERVICE_SCORER_SPREAD_BALANCED_WEIGHT",
		cfg.ScorerConfig.SpreadBalancedWeight)
	if err != nil {
		return cfg, err
	}
	cfg.ScorerConfig.ZoneLabel = envString("AI_DECISION_SERVICE_SCORER_ZONE_LABEL", cfg.ScorerConfig.ZoneLabel)
	cfg.ScorerConfig.UnknownScore, err = envFloat("AI_DECISION_SERVICE_SCORER_UNKNOWN_SCORE",
		cfg.ScorerConfig.UnknownScore)
	if err != nil {
		return cfg, err
	}
	cfg.RebalanceConfig.EAOStaleAfter, err = envDuration("AI_DECISION_SERVICE_REBALANCE_EAO_STALE_AFTER",
		cfg.RebalanceConfig.EAOStaleAfter)
	if err != nil {
		return cfg, err
	}
	cfg.RebalanceConfig.FlipMoveActionWindow, err = envDuration("AI_DECISION_SERVICE_REBALANCE_FLIP_MOVE_ACTION_WINDOW",
		cfg.RebalanceConfig.FlipMoveActionWindow)
	if err != nil {
		return cfg, err
	}
	cfg.RebalanceConfig.RiskConditions = envList("AI_DECISION_SERVICE_REBALANCE_RISK_CONDITIONS",
		cfg.RebalanceConfig.RiskConditions)
	cfg.LogLevel, err = envLogLevel("AI_DECISION_SERVICE_LOG_LEVEL", cfg.LogLevel)
	if err != nil {
		return cfg, err
	}
	cfg.ModelConfig.Provider = envString("AI_DECISION_SERVICE_MODEL_PROVIDER", cfg.ModelConfig.Provider)
	cfg.ModelConfig.BaseURL = envString("AI_DECISION_SERVICE_MODEL_BASE_URL", cfg.ModelConfig.BaseURL)
	cfg.ModelConfig.Path = envString("AI_DECISION_SERVICE_MODEL_PATH", cfg.ModelConfig.Path)
	cfg.ModelConfig.ModelName = envString("AI_DECISION_SERVICE_MODEL_NAME", cfg.ModelConfig.ModelName)
	cfg.ModelConfig.Timeout, err = envDuration("AI_DECISION_SERVICE_MODEL_TIMEOUT", cfg.ModelConfig.Timeout)
	if err != nil {
		return cfg, err
	}
	return cfg, validateConfig(cfg)
}

func validateConfig(cfg server.Config) error {
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return fmt.Errorf("invalid port: %d", cfg.Port)
	}
	if cfg.ReadTimeout <= 0 {
		return fmt.Errorf("invalid read timeout: %v", cfg.ReadTimeout)
	}
	if cfg.MaxBodyBytes <= 0 {
		return fmt.Errorf("invalid max body bytes: %d", cfg.MaxBodyBytes)
	}
	if cfg.HealthPath == "" {
		return fmt.Errorf("health path cannot be empty")
	}
	if cfg.DecisionPath == "" {
		return fmt.Errorf("decision path cannot be empty")
	}
	if err := cfg.ScorerConfig.Validate(); err != nil {
		return fmt.Errorf("invalid scorer config: %w", err)
	}
	if err := cfg.RebalanceConfig.Validate(); err != nil {
		return fmt.Errorf("invalid rebalance config: %w", err)
	}
	if err := cfg.ModelConfig.Validate(); err != nil {
		return fmt.Errorf("invalid model config: %w", err)
	}
	return nil
}

// envInt returns defaultValue when name is unset, or an error when it isn't a number.
func envInt(name string, defaultValue int) (int, error) {
	valStr := os.Getenv(name)
	if valStr == "" {
		return defaultValue, nil
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a number: %w", name, valStr, err)
	}
	return val, nil
}

// envDuration is envInt for durations like "5s" or "300ms".
func envDuration(name string, defaultValue time.Duration) (time.Duration, error) {
	durationStr := os.Getenv(name)
	if durationStr == "" {
		return defaultValue, nil
	}
	duration, err := time.ParseDuration(durationStr)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid duration: %w", name, durationStr, err)
	}
	// Here we could add more validation for duration format if needed.
	return duration, nil
}

// envString returns defaultValue when name is unset.
func envString(name string, defaultValue string) string {
	val := os.Getenv(name)
	if val == "" {
		return defaultValue
	}
	return val
}

// envFloat returns defaultValue when name is unset, or an error when it isn't a valid float64.
func envFloat(name string, defaultValue float64) (float64, error) {
	valStr := os.Getenv(name)
	if valStr == "" {
		return defaultValue, nil
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid float64: %w", name, valStr, err)
	}
	return val, nil
}

// envList returns defaultValue when name is unset, else its comma-separated items, trimmed, empties dropped.
func envList(name string, defaultValue []string) []string {
	valStr := os.Getenv(name)
	if valStr == "" {
		return defaultValue
	}
	items := strings.Split(valStr, ",")
	var result []string
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

// envLogLevel is envInt for log levels: debug, info, warn or error (any case).
func envLogLevel(name string, defaultValue slog.Level) (slog.Level, error) {
	v := os.Getenv(name)
	if v == "" {
		return defaultValue, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(v)); err != nil {
		return defaultValue, fmt.Errorf("%s=%q is not a log level (debug, info, warn, error): %w", name, v, err)
	}
	return level, nil
}
