package server

import "time"

// Defaults; cmd/main.go overrides each from its env var.
const (
	DefaultPort         = 8080
	DefaultHealthPath   = "/healthz"
	DefaultDecisionPath = "/api/v1/agent/placement/decision"
	DefaultReadTimeout  = 5 * time.Second
	DefaultMaxBodyBytes = 5 * 1024 * 1024 // 5 MiB
)

// Config holds every tunable of the service.
type Config struct {
	Port         int
	HealthPath   string
	DecisionPath string
	ReadTimeout  time.Duration
	MaxBodyBytes int64
}

func DefaultConfig() Config {
	return Config{
		Port:         DefaultPort,
		HealthPath:   DefaultHealthPath,
		DecisionPath: DefaultDecisionPath,
		ReadTimeout:  DefaultReadTimeout,
		MaxBodyBytes: DefaultMaxBodyBytes,
	}
}
