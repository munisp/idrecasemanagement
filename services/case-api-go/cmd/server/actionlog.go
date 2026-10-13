package main

// actionlog.go — every action anyone takes, for monitoring through the OTel
// stack. Two outputs, both kept small and free of request bodies:
//
//   - one JSON line per request on stdout ({"kind":"action",...}); promtail
//     ships it to Loki, where Grafana's idre activity dashboard reads it. This
//     is the complete record: traces are tail-sampled (~10% of normal
//     requests), so they cannot be.
//   - OTel metrics: idre.actions (count) and idre.action.duration, by tenant,
//     action, role and outcome, exported through the collector to Prometheus
//     for rates, panels and alerts.
//
// Business events the app already records (logAudit, logActivity) are written
// as {"kind":"event",...} lines too, with sensitive fields left out.

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// actionRec carries who is acting from the auth middleware (inside a route
// group) back out to actionLog (around everything).
type actionRec struct{ p *principal }
type ctxActionRec struct{}

var actionOut = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
	ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.MessageKey {
			return slog.Attr{} // "kind" says what the line is
		}
		return a
	},
}))

var (
	actionCount    metric.Int64Counter
	actionDuration metric.Float64Histogram
	docsUnread     metric.Int64Gauge
	docsOverdue    metric.Int64Gauge
	docsStalled    metric.Int64Gauge
)

// initActionMetrics registers the instruments on the global meter provider
// (a no-op until initTelemetry installs a real one).
func initActionMetrics() {
	m := otel.Meter("idre-case-api")
	actionCount, _ = m.Int64Counter("idre.actions",
		metric.WithDescription("Actions taken in idre, by tenant, action, role and outcome"))
	actionDuration, _ = m.Float64Histogram("idre.action.duration", metric.WithUnit("s"),
		metric.WithDescription("Time to answer an action"))
	docsUnread, _ = m.Int64Gauge("idre.documents.unread", metric.WithDescription("Uploaded documents not read yet"))
	docsOverdue, _ = m.Int64Gauge("idre.documents.unread_overdue",
		metric.WithDescription("Documents waiting longer than the 30-minute grace for their first reading"))
	docsStalled, _ = m.Int64Gauge("idre.documents.stalled", metric.WithDescription("Documents the reader could not read after several tries"))
}

// recordDocumentGauges is called by the unread-document sweep (every 5 min).
func recordDocumentGauges(ctx context.Context, tenant string, unread, overdue, stalled int64) {
	if docsUnread == nil {
		return
	}
	at := metric.WithAttributes(attribute.String("tenant", tenant))
	docsUnread.Record(ctx, unread, at)
	docsOverdue.Record(ctx, overdue, at)
	docsStalled.Record(ctx, stalled, at)
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// quietActions are counted but not written line by line: the portal sends
// them on a timer, so they say nothing about what anyone did.
var quietActions = map[string]bool{"POST /presence/ping": true}

func actionLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		rec := &actionRec{}
		r = r.WithContext(context.WithValue(r.Context(), ctxActionRec{}, rec))
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		elapsed := time.Since(start)

		rctx := chi.RouteContext(r.Context())
		route, tenant, caseID, docID := "", "", "", ""
		if rctx != nil {
			route = rctx.RoutePattern()
			tenant, caseID, docID = rctx.URLParam("tenant"), rctx.URLParam("caseId"), rctx.URLParam("docId")
		}
		if route == "" || strings.HasSuffix(route, "/*") {
			// Turned away before a route matched (no sign-in, unknown
			// path): name it by its path, with the ids taken out.
			route = withoutIDs(r.URL.Path)
			if m := tenantPath.FindStringSubmatch(r.URL.Path); m != nil {
				if tenant == "" {
					tenant = m[1]
				}
				if caseID == "" {
					caseID = m[2]
				}
				route = "/v1/tenants/{tenant}" + strings.TrimPrefix(route, "/v1/tenants/"+m[1])
			}
		}
		action := actionName(r.Method, route)
		user, userID, roles := "anonymous", "", ""
		if rec.p != nil {
			user, userID, roles = displayName(*rec.p), rec.p.Subject, roleList(rec.p.Roles)
		} else if strings.HasPrefix(route, "/api/share/") {
			user = "share link"
		}
		outcome := outcomeOf(sw.code)

		attrs := metric.WithAttributes(attribute.String("tenant", tenant), attribute.String("action", action),
			attribute.String("role", roles), attribute.String("outcome", outcome))
		if actionCount != nil {
			actionCount.Add(r.Context(), 1, attrs)
			actionDuration.Record(r.Context(), elapsed.Seconds(), attrs)
		}
		if quietActions[action] || strings.HasPrefix(r.URL.Path, "/dapr/") {
			return
		}
		fields := []any{"kind", "action", "tenant", tenant, "user", user, "user_id", userID, "roles", roles,
			"action", action, "status", sw.code, "outcome", outcome, "ms", elapsed.Milliseconds(), "ip", clientIP(r)}
		if caseID != "" {
			fields = append(fields, "case_id", caseID)
		}
		if docID != "" {
			fields = append(fields, "doc_id", docID)
		}
		if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
			fields = append(fields, "trace_id", sc.TraceID().String())
		}
		actionOut.Info("", fields...)
	})
}

// noteActor records the authenticated caller for actionLog; it sits right
// after the auth middleware in each route group.
func noteActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec, ok := r.Context().Value(ctxActionRec{}).(*actionRec); ok {
			if p, ok := r.Context().Value(ctxPrincipal{}).(principal); ok {
				rec.p = &p
			}
		}
		next.ServeHTTP(w, r)
	})
}

// actionName is the method and route pattern, without the tenant prefix:
// "POST /cases/{caseId}/documents". A request that matched no route is
// "unmatched" (so probing for paths is visible without logging the paths).
func actionName(method, route string) string {
	if route == "" {
		return "unmatched"
	}
	route = strings.TrimPrefix(route, "/v1/tenants/{tenant}")
	if route == "" {
		route = "/"
	}
	return method + " " + strings.TrimSuffix(route, "/")
}

func outcomeOf(code int) string {
	switch {
	case code < 400:
		return "ok"
	case code == http.StatusUnauthorized:
		return "unauthenticated"
	case code == http.StatusForbidden:
		return "denied"
	case code == http.StatusNotFound:
		return "not_found"
	case code == http.StatusTooManyRequests:
		return "throttled"
	case code < 500:
		return "rejected"
	default:
		return "error"
	}
}

// appRole: idre's own roles are upper case; Keycloak adds its defaults
// (default-roles-idre, offline_access, uma_authorization) to every account.
var appRole = regexp.MustCompile(`^[A-Z][A-Z_]*$`)

// idSegment: a uuid or number path segment, replaced by {id} in action names.
var idSegment = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F-]{27}|\d+)$`)

// tenantPath: the tenant and (when present) case id of a tenant URL.
var tenantPath = regexp.MustCompile(`^/v1/tenants/([^/]+)(?:/cases/([0-9a-fA-F]{8}-[0-9a-fA-F-]{27}))?`)

func withoutIDs(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if idSegment.MatchString(p) {
			parts[i] = "{id}"
		}
	}
	return strings.Join(parts, "/")
}

func roleList(roles []string) string {
	rs := []string{}
	for _, r := range roles {
		if appRole.MatchString(r) {
			rs = append(rs, r)
		}
	}
	sort.Strings(rs)
	return strings.Join(rs, ",")
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	return r.RemoteAddr
}

// sensitiveKey: fields an event line never carries -- contact details, bank
// and card numbers, secrets, and free text (letters, notes, messages).
var sensitiveKey = regexp.MustCompile(`(?i)email|phone|ssn|dob|birth|account|routing|card|iban|token|secret|password|key|address|body|content|text|note|message|html`)

// eventDetail keeps an event's small, non-sensitive scalar fields.
func eventDetail(payload map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range payload {
		if sensitiveKey.MatchString(k) {
			continue
		}
		switch t := v.(type) {
		case string:
			out[k] = truncate(t, 200)
		case bool, int, int32, int64, float32, float64:
			out[k] = t
		}
	}
	return out
}

// logEvent writes one business event for monitoring, with the actor of the
// request it happened in (or "system" for background work).
func logEvent(ctx context.Context, source, tenant, caseID, event string, detail map[string]any) {
	user, roles := "system", ""
	if rec, ok := ctx.Value(ctxActionRec{}).(*actionRec); ok && rec.p != nil {
		user, roles = displayName(*rec.p), roleList(rec.p.Roles)
	} else if p, ok := ctx.Value(ctxPrincipal{}).(principal); ok {
		user, roles = displayName(p), roleList(p.Roles)
	}
	fields := []any{"kind", "event", "source", source, "event", event, "tenant", tenant, "user", user, "roles", roles}
	if caseID != "" {
		fields = append(fields, "case_id", caseID)
	}
	if len(detail) > 0 {
		fields = append(fields, "detail", detail)
	}
	actionOut.Info("", fields...)
}
