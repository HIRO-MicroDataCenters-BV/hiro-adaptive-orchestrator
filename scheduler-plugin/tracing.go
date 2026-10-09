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

package schedulerplugin

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
	"k8s.io/klog/v2"
)

// Separate from the operator's internal/tracing: this module must not pull in
// controller-runtime. Env var names are the OpenTelemetry spec's, not ours —
// the SDK reads the endpoint itself; we only read it to decide on/off.
const (
	envOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envServiceName  = "OTEL_SERVICE_NAME"
	envSampleRatio  = "OTEL_TRACES_SAMPLER_ARG"
)

const (
	// DefaultServiceName is a standalone-run fallback only. The chart sets
	// OTEL_SERVICE_NAME from scheduler.plugin.schedulerName, which stays the
	// single source of truth for the scheduler's name.
	DefaultServiceName = "hiro-scheduler"

	// DefaultSampleRatio traces everything: scheduling volume is modest, and
	// a partial sample defeats the point when chasing one pod's placement.
	DefaultSampleRatio = 1.0
)

// InitTracing configures the global TracerProvider and W3C propagator.
// Returns a nil shutdown func when the OTLP endpoint is unset, leaving otel's
// no-op provider in place.
func InitTracing(ctx context.Context, version string) (func(context.Context) error, error) {
	if os.Getenv(envOTLPEndpoint) == "" {
		return nil, nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP trace exporter: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(serviceName()),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, fmt.Errorf("building trace resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio()))),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	klog.InfoS(PluginName+": tracing enabled",
		"endpoint", os.Getenv(envOTLPEndpoint), "service", serviceName())
	return provider.Shutdown, nil
}

func serviceName() string {
	if name := os.Getenv(envServiceName); name != "" {
		return name
	}
	return DefaultServiceName
}

func sampleRatio() float64 {
	raw := os.Getenv(envSampleRatio)
	if raw == "" {
		return DefaultSampleRatio
	}
	ratio, err := strconv.ParseFloat(raw, 64)
	if err != nil || ratio < 0 || ratio > 1 {
		klog.InfoS(PluginName+": ignoring invalid sampler arg", "value", raw,
			"using", DefaultSampleRatio)
		return DefaultSampleRatio
	}
	return ratio
}

// logTrace returns trace_id/span_id key-values for the active span, so a log
// line can be linked to its trace. Empty when tracing is off.
func logTrace(ctx context.Context) []any {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []any{"trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String()}
}
