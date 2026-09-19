// Package telemetry starts and stops OpenTelemetry tracing.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// Must match the version resource.Default() uses. resource.Merge refuses to
	// combine resources whose schema URLs differ, and the failure is at startup:
	// the process does not come up at all.
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Setup installs a tracer provider and returns a function that flushes it.
//
// The provider is installed whether or not anything is collecting. That matters
// more than it looks: OpenTelemetry's default global provider is a no-op whose
// spans carry an all-zero trace id, so an API spec.md 1.1 trace_id would come
// out as thirty-two zeroes on every response and correlate with nothing. A real
// SDK provider mints real ids regardless of where -- or whether -- they are
// exported.
//
// With no endpoint configured, spans are recorded and dropped. The ids are
// still real and still appear in the logs next to the request that produced
// them, which is most of the value on a developer's machine and all of the
// value before P1-001 gives us somewhere to send them.
func Setup(ctx context.Context, serviceName, version, environment, otlpEndpoint string) (func(context.Context) error, error) {
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
		// The raw convention key rather than a helper: the helper moved between
		// semconv versions, and this attribute is not worth coupling the file
		// to one.
		attribute.String("deployment.environment.name", environment),
	))
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		// Everything, for now. Phase 1 traffic is one pilot merchant, and a
		// sampled-away trace is exactly the one the support ticket quotes.
		// Revisit when volume makes that expensive, not before.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	}

	if otlpEndpoint != "" {
		// Checked here because the exporter will not do it. Given an
		// unparseable URL it logs internally, returns no error, and falls back
		// to its default endpoint -- so a typo means traces go somewhere else
		// entirely while the startup log says they are being exported.
		if u, err := url.Parse(otlpEndpoint); err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf(
				"OTEL_EXPORTER_OTLP_ENDPOINT %q is not an absolute URL", otlpEndpoint)
		}

		exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(otlpEndpoint))
		if err != nil {
			return nil, fmt.Errorf("open OTLP exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
		slog.Info("tracing: exporting", "endpoint", otlpEndpoint)
	} else {
		slog.Info("tracing: no OTEL_EXPORTER_OTLP_ENDPOINT; ids are real, spans are not exported")
	}

	provider := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(provider)

	// Accept and forward W3C traceparent, so a trace that starts at the browser
	// or at Caddy continues here instead of restarting.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		// Bounded: a shutdown that blocks on an unreachable collector would
		// hold a deploy open.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return provider.Shutdown(ctx)
	}, nil
}
