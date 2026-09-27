package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// queryTracer puts a child span around every query.  (BR-092, S1-011)
//
// The acceptance asks for query duration on the span. A span already is a
// duration, so opening one in TraceQueryStart and ending it in TraceQueryEnd
// records it without anything here doing arithmetic -- and it also gives the
// shape of a request, which a single summed number would not.
//
// Hand-written rather than otelpgx: pgx ships the interface, otel is already a
// direct dependency, and this is twenty lines. A dependency earns its place by
// doing something harder than this.
type queryTracer struct{ tracer trace.Tracer }

// pgx calls TraceQueryStart and TraceQueryEnd on the same goroutine for one
// query, and hands the returned context back to TraceQueryEnd, so the span
// travels in the context rather than in a field.
type spanKey struct{}

func (t queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx, span := t.tracer.Start(ctx, "postgres.query")
	// The SQL text, never the arguments. Args carry emails, password hashes
	// and identity numbers, and a span is a place where those would be read by
	// anyone with access to the tracing backend.
	span.SetAttributes(attribute.String("db.query.text", data.SQL))
	return context.WithValue(ctx, spanKey{}, span)
}

func (t queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(spanKey{}).(trace.Span)
	if !ok {
		return
	}
	defer span.End()

	if data.Err != nil {
		// Not RecordError: the message can carry a constraint name and a row
		// value, and this is the same disclosure the problem envelope refuses
		// to make. The status alone says a query failed; the log line beside
		// it carries the detail, under the same trace id.
		span.SetStatus(codes.Error, "query failed")
		return
	}
	span.SetAttributes(attribute.String("db.response.command", data.CommandTag.String()))
}
