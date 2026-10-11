"""OpenTelemetry tracing shared by the Python services.

Spans go over OTLP/HTTP to OTEL_EXPORTER_OTLP_ENDPOINT. With that variable
unset, or with the OpenTelemetry packages not installed (unit tests, local
runs), every helper here is a no-op and the service behaves as before.
Sampling is left to the collector, so every span is recorded.
"""
import os
from contextlib import contextmanager

_tracer = None
_propagator = None


def init(service_name: str) -> bool:
    """Install the tracer provider. Returns True when export is active."""
    global _tracer, _propagator
    if not (os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT")
            or os.environ.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")):
        print(f"[otel] tracing disabled for {service_name}: OTEL_EXPORTER_OTLP_ENDPOINT not set", flush=True)
        return False
    try:
        from opentelemetry import trace
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
        from opentelemetry.sdk.resources import Resource
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor
        from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
    except ImportError as exc:
        print(f"[otel] tracing disabled for {service_name}: {exc}", flush=True)
        return False
    # Resource.create merges OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES over
    # this default.
    provider = TracerProvider(resource=Resource.create({"service.name": service_name}))
    provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(provider)
    _tracer = trace.get_tracer(service_name)
    _propagator = TraceContextTextMapPropagator()
    try:  # outgoing httpx calls (model endpoint, vault, case-api signals)
        from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor
        HTTPXClientInstrumentor().instrument()
    except Exception as exc:  # noqa: BLE001 - optional
        print(f"[otel] httpx instrumentation unavailable: {exc}", flush=True)
    print(f"[otel] tracing enabled for {service_name} -> {os.environ.get('OTEL_EXPORTER_OTLP_ENDPOINT')}", flush=True)
    return True


class _NoSpan:
    def set_attribute(self, *_a, **_k): pass
    def record_exception(self, *_a, **_k): pass
    def set_status(self, *_a, **_k): pass


@contextmanager
def span(name: str, traceparent: str | None = None, consumer: bool = False, **attrs):
    """Start a span; `traceparent` (from an event) makes it a child of the
    publisher's trace. An exception leaving the block marks the span failed
    and is re-raised."""
    if _tracer is None:
        yield _NoSpan()
        return
    from opentelemetry import trace
    ctx = _propagator.extract({"traceparent": traceparent}) if traceparent else None
    kind = trace.SpanKind.CONSUMER if consumer else trace.SpanKind.INTERNAL
    clean = {k: v for k, v in attrs.items() if v is not None}
    with _tracer.start_as_current_span(name, context=ctx, kind=kind, attributes=clean) as sp:
        yield sp


def mark_error(sp, message: str) -> None:
    """Flag a span as failed without raising (handled errors)."""
    if _tracer is None:
        return
    from opentelemetry.trace import Status, StatusCode
    sp.set_status(Status(StatusCode.ERROR, message[:200]))


def enabled() -> bool:
    return _tracer is not None


def instrument_fastapi(app) -> None:
    """Server spans for a FastAPI app (no-op when tracing is off)."""
    if _tracer is None:
        return
    try:
        from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
        FastAPIInstrumentor.instrument_app(app, excluded_urls="healthz,health,metrics")
    except Exception as exc:  # noqa: BLE001 - optional
        print(f"[otel] fastapi instrumentation unavailable: {exc}", flush=True)


def temporal_interceptors() -> list:
    """Temporal client interceptors that trace workflows and activities."""
    if _tracer is None:
        return []
    try:
        from temporalio.contrib.opentelemetry import TracingInterceptor
        # always_create_workflow_spans: without it a workflow span is only
        # recorded when the starter/signaller passed a trace in, and case-api's
        # Temporal client does not -- so nothing was recorded at all.
        return [TracingInterceptor(always_create_workflow_spans=True)]
    except Exception as exc:  # noqa: BLE001 - optional
        print(f"[otel] temporal tracing unavailable: {exc}", flush=True)
        return []
