package httpapi_test

import (
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestMain installs a real tracer provider for every test in this package,
// because cmd/api installs one for every request in production.
//
// Without it OpenTelemetry's default provider is a no-op whose spans carry an
// all-zero trace id, and a test asserting on trace_id would be measuring the
// test's own setup rather than the server's behaviour.
func TestMain(m *testing.M) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	os.Exit(m.Run())
}
