// Author: Patrick Munis at newwave
//! OpenTelemetry tracing for the vault: one server span per request,
//! continuing the caller's trace when it sends a `traceparent` header.
//!
//! Export is OTLP/HTTP to OTEL_EXPORTER_OTLP_ENDPOINT; with that unset this
//! module does nothing. It never sees request or response bodies — only the
//! method, the route path and the status code are recorded.

use axum::{extract::Request, middleware::Next, response::Response};
use opentelemetry::{
    global,
    trace::{FutureExt, Span, SpanKind, Status, TraceContextExt, Tracer},
    KeyValue,
};
use opentelemetry_http::HeaderExtractor;
use opentelemetry_otlp::SpanExporter;
use opentelemetry_sdk::{propagation::TraceContextPropagator, trace::SdkTracerProvider, Resource};

/// Installs the global tracer provider. Returns it so `main` keeps it alive.
pub fn init() -> Option<SdkTracerProvider> {
    if std::env::var("OTEL_EXPORTER_OTLP_ENDPOINT").is_err()
        && std::env::var("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT").is_err()
    {
        tracing::info!("tracing disabled: OTEL_EXPORTER_OTLP_ENDPOINT not set");
        return None;
    }
    let exporter = match SpanExporter::builder().with_http().build() {
        Ok(e) => e,
        Err(err) => {
            tracing::warn!("tracing disabled: exporter init failed: {err}");
            return None;
        }
    };
    let provider = SdkTracerProvider::builder()
        .with_batch_exporter(exporter)
        .with_resource(Resource::builder().with_service_name("idre-vault").build())
        .build();
    global::set_tracer_provider(provider.clone());
    global::set_text_map_propagator(TraceContextPropagator::new());
    tracing::info!("tracing enabled");
    Some(provider)
}

/// axum middleware: a server span per request.
pub async fn trace_request(req: Request, next: Next) -> Response {
    let parent = global::get_text_map_propagator(|p| p.extract(&HeaderExtractor(req.headers())));
    let tracer = global::tracer("idre-vault");
    let mut span = tracer
        .span_builder(format!("{} {}", req.method(), req.uri().path()))
        .with_kind(SpanKind::Server)
        .start_with_context(&tracer, &parent);
    span.set_attribute(KeyValue::new("http.request.method", req.method().to_string()));
    span.set_attribute(KeyValue::new("http.route", req.uri().path().to_string()));
    let cx = parent.with_span(span);
    let resp = next.run(req).with_context(cx.clone()).await;
    let span = cx.span();
    span.set_attribute(KeyValue::new(
        "http.response.status_code",
        i64::from(resp.status().as_u16()),
    ));
    if resp.status().is_server_error() {
        span.set_status(Status::error("server error"));
    }
    span.end();
    resp
}
