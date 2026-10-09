/*
Copyright 2026 HIRO Adaptive Orchestrator.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package tracing sets up OpenTelemetry tracing for the operator, letting one
// trace span the scheduler, the PlacementServer, and the AI agent.
//
// Like internal/metrics this is a leaf dependency — it must never import a
// package that imports it back. The scheduler-plugin module has its own copy
// rather than importing this one, since it must not pull in controller-runtime.
package tracing

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"
	"go.opentelemetry.io/otel/trace"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Env var names are the OpenTelemetry spec's, not ours — the SDK reads the
// endpoint itself; we only read it to decide whether tracing is on at all.
const (
	envOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envServiceName  = "OTEL_SERVICE_NAME"
	envSampleRatio  = "OTEL_TRACES_SAMPLER_ARG"
)

const (
	// DefaultServiceName is a standalone-run fallback; the chart sets
	// OTEL_SERVICE_NAME so one trace can tell two releases apart.
	DefaultServiceName = "hiro-adaptive-orchestrator"

	// DefaultSampleRatio traces everything — placement and rebalance volume
	// is modest, and a partial sample defeats the point when chasing one
	// pod's decision.
	DefaultSampleRatio = 1.0
)

// Init configures the global TracerProvider and W3C propagator. Returns a nil
// shutdown func when the OTLP endpoint is unset, leaving otel's no-op
// provider in place so an un-configured deploy behaves exactly as before.
func Init(ctx context.Context, version string) (func(context.Context) error, error) {
	endpoint := os.Getenv(envOTLPEndpoint)
	if endpoint == "" {
		return nil, nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP trace exporter: %w", err)
	}

	// WithFromEnv honours OTEL_RESOURCE_ATTRIBUTES, so extra attributes
	// (cluster, environment, …) can be added without a code change.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(ServiceName()),
			semconv.ServiceVersion(version),
		),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("building trace resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio(ctx)))),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	logf.FromContext(ctx).Info("tracing enabled",
		"endpoint", endpoint, "service", ServiceName())
	return provider.Shutdown, nil
}

// ServiceName is the service.name reported on every span.
func ServiceName() string {
	if name := os.Getenv(envServiceName); name != "" {
		return name
	}
	return DefaultServiceName
}

func sampleRatio(ctx context.Context) float64 {
	raw := os.Getenv(envSampleRatio)
	if raw == "" {
		return DefaultSampleRatio
	}
	ratio, err := strconv.ParseFloat(raw, 64)
	if err != nil || ratio < 0 || ratio > 1 {
		logf.FromContext(ctx).Info("ignoring invalid sampler arg",
			"value", raw, "using", DefaultSampleRatio)
		return DefaultSampleRatio
	}
	return ratio
}

// LogValues returns trace_id/span_id key-values for the active span, so a log
// line can be linked to its trace. Empty when tracing is off.
func LogValues(ctx context.Context) []any {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []any{"trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String()}
}
