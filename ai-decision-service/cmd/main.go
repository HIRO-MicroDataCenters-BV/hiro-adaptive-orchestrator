package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/server"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig()
	if err != nil {
		log.Error("Failed to load configuration", slog.Any("error", err))
		os.Exit(1)
	}

	srv := server.New(cfg, log)
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
