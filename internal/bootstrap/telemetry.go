package bootstrap

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"google.golang.org/adk/v2/telemetry"

	"agentlite/internal/config"
)

// tracing turns on adk's OpenTelemetry setup, which exports spans (and logs)
// over OTLP itself when OTEL_EXPORTER_OTLP_ENDPOINT is set; Jaeger in
// development. The W3C propagator lets a run's trace continue through the
// Valkey queue into the worker and into sub-agent runs.
func tracing(ctx context.Context, cfg *config.Config) (func(), error) {
	providers, err := telemetry.New(ctx, telemetry.WithResource(resource.NewSchemaless(attribute.String("service.name", cfg.AppName))))
	if err != nil {
		return nil, err
	}
	providers.SetGlobalOtelProviders()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = providers.Shutdown(ctx)
	}, nil
}
