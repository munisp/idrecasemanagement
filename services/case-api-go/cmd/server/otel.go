// Author: Patrick Munis at newwave
// otel.go — OpenTelemetry tracing for case-api.
//
// Spans are exported over OTLP/HTTP to the endpoint in
// OTEL_EXPORTER_OTLP_ENDPOINT (the standard variable the exporter reads
// itself). With that variable unset tracing is a no-op: nothing is exported
// and the service behaves exactly as before. Sampling is left to the
// collector (tail sampling), so every span is recorded here.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// initTelemetry installs the global tracer provider and W3C propagator and
// returns a shutdown func that flushes pending spans.
func initTelemetry(ctx context.Context) func(context.Context) {
	// Propagation is set even when export is off, so an incoming traceparent
	// still flows through to downstream calls.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		slog.Info("tracing disabled: OTEL_EXPORTER_OTLP_ENDPOINT not set")
		initActionMetrics()
		return func(context.Context) {}
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		slog.Error("tracing disabled: exporter init failed", "err", err)
		return func(context.Context) {}
	}
	// OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES override these defaults.
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("idre-case-api")),
		resource.WithFromEnv(), resource.WithHost())
	if err != nil {
		slog.Warn("tracing resource detection incomplete", "err", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(tp)
	// Every client built on the default transport (Dapr publish, Keycloak
	// admin, graph-intel, Stripe, ...) gets a client span and sends
	// traceparent downstream.
	http.DefaultTransport = otelhttp.NewTransport(http.DefaultTransport)
	slog.Info("tracing enabled", "endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	// Metrics (idre.actions, idre.action.duration, document counts) go to
	// the same collector, which exports them to Prometheus.
	var mp *sdkmetric.MeterProvider
	if mexp, err := otlpmetrichttp.New(ctx); err != nil {
		slog.Error("metrics disabled: exporter init failed", "err", err)
	} else {
		mp = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp, sdkmetric.WithInterval(30*time.Second))))
		otel.SetMeterProvider(mp)
	}
	initActionMetrics()
	return func(ctx context.Context) {
		if err := tp.Shutdown(ctx); err != nil {
			slog.Warn("tracing shutdown", "err", err)
		}
		if mp != nil {
			_ = mp.Shutdown(ctx)
		}
	}
}

// tracePool attaches query tracing to the pgx pool config.
func tracePool(pcfg *pgxpool.Config) {
	pcfg.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithTrimSQLInSpanName())
}

// traceRequests is the chi middleware: one server span per request, named by
// the matched route pattern (never the raw path, which would create one
// span name per case id). Health checks are not traced.
func traceRequests(next http.Handler) http.Handler {
	named := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		span := trace.SpanFromContext(r.Context())
		if !span.IsRecording() {
			return
		}
		if rc := chi.RouteContext(r.Context()); rc != nil {
			if pattern := rc.RoutePattern(); pattern != "" {
				span.SetName(r.Method + " " + pattern)
				span.SetAttributes(semconv.HTTPRoute(pattern))
			}
			if t := rc.URLParam("tenant"); t != "" {
				span.SetAttributes(attribute.String("idre.tenant", t))
			}
			if c := rc.URLParam("caseId"); c != "" {
				span.SetAttributes(attribute.String("idre.case_id", c))
			}
		}
	})
	traced := otelhttp.NewHandler(named, "http.request")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		traced.ServeHTTP(w, r)
	})
}

// traceparentOf returns the W3C traceparent for ctx, or "" when ctx carries
// no sampled span. Stamped onto asynchronous events so their consumers
// (doc-intel, lettergen) continue the same trace across Kafka.
func traceparentOf(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}
