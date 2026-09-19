package telemetry_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/miqbalhamdani/sewain-api/internal/platform/telemetry"
)

// TestSetup exists because this package broke the server at startup and no test
// noticed: resource.Merge refuses to combine resources whose schema URLs
// differ, so a semconv import one version out from the SDK's own meant the
// process would not come up at all. Everything else was tested against a tracer
// the tests built themselves, which never went through this function.
func TestSetup(t *testing.T) {
	t.Run("starts without an exporter", func(t *testing.T) {
		// The normal case on a developer's machine, and the case before P1-001
		// provides a collector.
		shutdown, err := telemetry.Setup(t.Context(), "test-service", "0.0.0", "test", "")
		if err != nil {
			t.Fatalf("Setup: %v", err)
		}
		t.Cleanup(func() {
			if err := shutdown(context.WithoutCancel(t.Context())); err != nil {
				t.Errorf("shutdown: %v", err)
			}
		})
	})

	// The point of installing a real provider rather than leaving the default
	// in place. OpenTelemetry's default is a no-op whose spans carry an all-zero
	// trace id, which would put thirty-two zeroes in every error response.
	t.Run("mints a real trace id with nothing collecting", func(t *testing.T) {
		shutdown, err := telemetry.Setup(t.Context(), "test-service", "0.0.0", "test", "")
		if err != nil {
			t.Fatalf("Setup: %v", err)
		}
		t.Cleanup(func() { _ = shutdown(context.WithoutCancel(t.Context())) })

		_, span := otel.Tracer("test").Start(t.Context(), "probe")
		defer span.End()

		id := span.SpanContext().TraceID()
		if !id.IsValid() {
			t.Fatalf("trace id %s is not valid; the default no-op provider is still installed", id)
		}
		if len(id.String()) != 32 {
			t.Errorf("trace id %q is %d characters, want 32", id, len(id.String()))
		}
	})

	t.Run("reports a bad exporter endpoint rather than starting half-configured", func(t *testing.T) {
		if _, err := telemetry.Setup(t.Context(), "test-service", "0.0.0", "test", "://not a url"); err == nil {
			t.Error("Setup accepted an unparseable endpoint")
		}
	})
}
