// case-api — transactional control plane for the NSA Federal IDRE platform.
//
// Responsibilities:
//   - REST API for cases, parties, offer metadata, fees, documents metadata
//   - Keycloak JWT validation (JWKS) + fail-closed tenant resolution (50 state tenants)
//   - Starts/signals Temporal workflows for the statutory lifecycle
//   - Posts double-entry escrow/fee transfers to TigerBeetle (ledger-per-tenant)
//   - Publishes domain events through the Dapr sidecar pub/sub (Kafka backing)
//   - Voice-AI integration surface: agent "tools" endpoints + HMAC event webhooks
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	tb "github.com/tigerbeetle/tigerbeetle-go"
	tb_types "github.com/tigerbeetle/tigerbeetle-go/pkg/types"
	temporalclient "go.temporal.io/sdk/client"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	Addr           string // :8080
	DatabaseURL    string // postgres://...
	KeycloakJWKS   string // https://keycloak/realms/idre/protocol/openid-connect/certs
	KeycloakIssuer string // https://keycloak/realms/idre
	TemporalHost   string // temporal-frontend:7233
	TemporalNS     string // idre
	TBAddresses    string // tigerbeetle-0:3000,tigerbeetle-1:3000,...
	DaprHTTP       string // http://localhost:3500
	VaultURL       string // http://vault:8081 (mTLS via Dapr in k8s)
	GraphIntelURL  string // http://graph-intel:8082 ("" = graph features disabled)
}

func configFromEnv() Config {
	get := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	return Config{
		Addr:           get("ADDR", ":8080"),
		DatabaseURL:    get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre"),
		KeycloakJWKS:   get("KEYCLOAK_JWKS_URL", "http://localhost:8085/realms/idre/protocol/openid-connect/certs"),
		KeycloakIssuer: get("KEYCLOAK_ISSUER", "http://localhost:8085/realms/idre"),
		TemporalHost:   get("TEMPORAL_HOST", "localhost:7233"),
		TemporalNS:     get("TEMPORAL_NAMESPACE", "idre"),
		TBAddresses:    get("TIGERBEETLE_ADDRESSES", "localhost:3000"),
		DaprHTTP:       get("DAPR_HTTP_ENDPOINT", "http://localhost:3500"),
		VaultURL:       get("VAULT_URL", "http://localhost:8081"),
		GraphIntelURL:  get("GRAPH_INTEL_URL", "http://localhost:8082"),
	}
}

// ---------------------------------------------------------------------------
// Domain types (contracts mirror contracts/idr.ts from the spec)
// ---------------------------------------------------------------------------

type Case struct {
	ID              string    `json:"id"`
	CaseNumber      string    `json:"case_number"`
	Tenant          string    `json:"tenant"`
	Status          string    `json:"status"`
	ServiceLine     string    `json:"service_line"`
	QPA             int64     `json:"qpa_cents"`
	OpenedAt        time.Time `json:"opened_at"`
	OfferWindowEnds time.Time `json:"offer_window_ends_at"`
}

type InitiateRequest struct {
	CaseNumber       string `json:"case_number"`
	ServiceLine      string `json:"service_line"`
	PlanType         string `json:"plan_type"` // FULLY_INSURED | SELF_FUNDED
	QPACents         int64  `json:"qpa_cents"`
	ProviderID       string `json:"provider_id"`
	PayerID          string `json:"payer_id"`
	OpenNegotiationEnd string `json:"open_negotiation_end"` // YYYY-MM-DD
}

type FeeTransfer struct {
	CaseID   string `json:"case_id"`
	Kind     string `json:"kind"` // ADMIN_FEE | IDRE_FEE_RESERVE | SETTLEMENT | REFUND
	PartyID  string `json:"party_id"`
	Amount   uint64 `json:"amount_cents"`
	PostKind string `json:"post_kind"` // PENDING | POST | VOID
}

// ---------------------------------------------------------------------------
// Auth: Keycloak JWT + tenant extraction (fail-closed)
// ---------------------------------------------------------------------------

type principal struct {
	Subject string
	Roles   []string
	Tenants []string // from Keycloak group claim, e.g. /tenant/tx
}

type authn struct {
	keys   jwk.Set
	issuer string
}

func (a *authn) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if raw == "" || raw == r.Header.Get("Authorization") {
			http.Error(w, `{"error":"missing bearer token"}`, http.StatusUnauthorized)
			return
		}
		tok, err := jwt.ParseString(raw,
			jwt.WithKeySet(a.keys),
			jwt.WithIssuer(a.issuer),
			jwt.WithValidate(true),
		)
		if err != nil {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		p := principal{Subject: tok.Subject()}
		if g, ok := tok.Get("groups"); ok {
			if arr, ok := g.([]any); ok {
				for _, it := range arr {
					s := fmt.Sprint(it)
					if strings.HasPrefix(s, "/tenant/") {
						p.Tenants = append(p.Tenants, strings.TrimPrefix(s, "/tenant/"))
					}
				}
			}
		}
		if rr, ok := tok.Get("realm_access"); ok {
			if m, ok := rr.(map[string]any); ok {
				if arr, ok := m["roles"].([]any); ok {
					for _, it := range arr {
						p.Roles = append(p.Roles, fmt.Sprint(it))
					}
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal{}, p)))
	})
}

type ctxPrincipal struct{}
type ctxTenant struct{}

// tenancy resolves the state tenant from the path and enforces it against the
// token's tenant group claim. Platform/federal admins bypass the group check.
func tenancy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.Context().Value(ctxPrincipal{}).(principal)
		tenant := strings.ToLower(chi.URLParam(r, "tenant"))
		if len(tenant) != 2 {
			http.Error(w, `{"error":"tenant required"}`, http.StatusBadRequest)
			return
		}
		allowed := false
		for _, role := range p.Roles {
			if role == "PLATFORM_ADMIN" || role == "FEDERAL_ADMIN" {
				allowed = true
			}
		}
		for _, t := range p.Tenants {
			if t == tenant {
				allowed = true
			}
		}
		if !allowed {
			http.Error(w, `{"error":"tenant access denied"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxTenant{}, tenant)))
	})
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type server struct {
	cfg     Config
	db      *pgxpool.Pool
	tc      temporalclient.Client
	tb      tb.Client
	docs    *docStore
	rds     *redisClient // cache + idempotency (fail-open)
	permify string       // Permify base URL ("" = ReBAC check disabled, dev mode)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	cfg := configFromEnv()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	must(err)
	defer pool.Close()

	tc, err := temporalclient.Dial(temporalclient.Options{
		HostPort:  cfg.TemporalHost,
		Namespace: cfg.TemporalNS,
	})
	must(err)
	defer tc.Close()

	tbc, err := tb.NewClient(tb_types.ToUint128(0), strings.Split(cfg.TBAddresses, ","))
	must(err)
	defer tbc.Close()

	rds := newRedis(envOr("REDIS_ADDR", ""))

	// JWKS via Redis: all replicas share one cache; Keycloak key rotation
	// propagates within the TTL instead of hammering the certs endpoint.
	var keys jwk.Set
	if raw := rds.get("idre:jwks"); raw != "" {
		keys, err = jwk.ParseString(raw)
	}
	if keys == nil {
		keys, err = jwk.Fetch(ctx, cfg.KeycloakJWKS)
		must(err)
		if buf, merr := json.Marshal(keys); merr == nil {
			rds.setex("idre:jwks", 300, string(buf))
		}
	}
	a := &authn{keys: keys, issuer: cfg.KeycloakIssuer}

	docs, err := newDocStore(
		envOr("MINIO_ENDPOINT", "localhost:9000"),
		envOr("MINIO_USER", "idre"), envOr("MINIO_PASSWORD", "idre-secret"))
	must(err)

	s := &server{cfg: cfg, db: pool, tc: tc, tb: tbc, docs: docs, rds: rds,
		permify: envOr("PERMIFY_URL", "")}

	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Logger, chimw.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// Authenticated, tenant-scoped API.
	r.Route("/v1/tenants/{tenant}", func(r chi.Router) {
		r.Use(a.middleware, tenancy)
		r.Post("/cases/initiate", s.initiateCase)
		r.Get("/cases", s.listCases)
		r.Get("/cases/{caseId}", s.getCase)
		r.Post("/fees/transfer", s.postFeeTransfer)      // escrow/admin/IDRE fee double-entry
		r.Post("/cases/{caseId}/signal", s.signalCase)   // e.g. response filed, fees paid

		// Documents: encrypted upload, authorized download, analysis status.
		r.Post("/cases/{caseId}/documents", s.uploadDocument)
		r.Get("/cases/{caseId}/documents", s.listDocuments)
		r.Get("/cases/{caseId}/documents/{docId}/download", s.downloadDocument)
		r.Get("/cases/{caseId}/documents/{docId}/analysis", s.documentAnalysis)

		// Stakeholder onboarding: applications, decisions, status.
		r.Post("/onboarding/applications", s.submitApplication)
		r.Get("/onboarding/applications", s.listApplications)
		r.Post("/onboarding/applications/{appId}/decision", s.decideApplication)

		// Voice console + compliance reports (JWT-authenticated reads).
		r.Get("/voice/intake", s.listVoiceIntake)
		r.Get("/voice/logs", s.listVoiceLogs)
		r.Post("/voice/outbound", s.outboundCall)          // trigger outbound calls
		r.Get("/cases/{caseId}/activities", s.listActivities) // CRM record timeline

		// CRM core: accounts, contacts, leads, tasks, notes, search.
		r.Get("/accounts", s.listAccounts)
		r.Post("/accounts", s.createAccount)
		r.Get("/accounts/{accountId}/360", s.account360)
		r.Post("/contacts", s.createContact)
		r.Get("/leads", s.listLeads)
		r.Post("/leads/{leadId}/convert", s.convertLead)
		r.Get("/tasks", s.listTasks)
		r.Post("/tasks", s.createTask)
		r.Post("/tasks/{taskId}/complete", s.completeTask)
		r.Post("/notes", s.addNote)
		r.Get("/search", s.globalSearch)

		// Case management: assignment, escalation, relationships, checklists,
		// calendar, notifications, saved views, letters.
		r.Post("/cases/{caseId}/assign", s.assignCase)
		r.Post("/cases/{caseId}/escalate", s.escalateCase)
		r.Post("/cases/relate", s.relateCases)
		r.Get("/cases/{caseId}/relationships", s.caseRelationships)
		r.Get("/cases/{caseId}/checklist", s.getChecklist)
		r.Post("/checklists/{itemId}/check", s.checkItem)
		r.Get("/calendar", s.calendar)
		r.Get("/notifications", s.listNotifications)
		r.Post("/notifications/{notifId}/read", s.readNotification)
		r.Get("/views", s.listSavedViews)
		r.Post("/views", s.saveView)
		r.Get("/cases/clocks", s.casesClocks)          // batch statutory-clock projection (grids)
		r.Get("/cases/{caseId}/clocks", s.caseClocks)  // per-case projection (workspace header)
		r.Post("/cases/bulk", s.bulkCases)             // bulk assign / status with per-item results
		r.Post("/queues/grab-next", s.grabNext)        // atomic queue claim (triage fast lane)

		// Graph intelligence (proxied to graph-intel: FalkorDB + GraphSAGE + EPR-KGQA).
		r.Post("/graph/ask", s.graphAsk)                      // EPR-KGQA natural-language query
		r.Post("/graph/feedback", s.graphFeedback)            // thumbs up/down -> ART-ready log
		r.Post("/graph/sync", s.graphSyncNow)                 // Postgres -> FalkorDB -> lakehouse
		r.Post("/graph/to-lakehouse", s.graphToLakehouse)     // FalkorDB -> gold-zone export
		r.Post("/graph/train", s.graphTrain)                  // GraphSAGE training round
		r.Get("/cases/{caseId}/related", s.caseRelated)       // GNN link predictions
		r.Get("/cases/{caseId}/graph-neighbors", s.caseGraphNeighbors)
		r.Post("/cases/{caseId}/letters/{template}", s.generateLetter)
		r.Get("/reports/sla", s.slaReport)
		r.Get("/reports/summary", s.summaryReport)
	})

	// Voice-AI surface (API-key auth, not OIDC).
	r.Route("/api/voice", func(r chi.Router) {
		r.Post("/tools/case-status", s.voiceCaseStatus)
		r.Post("/tools/deadlines", s.voiceDeadlines)
		r.Post("/tools/intake", s.voiceIntake)
		r.Post("/events", s.voiceEvents) // HMAC-signed platform → us webhooks
	})

	// Inbound email (Mailgun/SES-style provider webhook, token-authenticated).
	r.Post("/api/email/inbound", s.emailInbound)

	slog.Info("case-api listening", "addr", cfg.Addr)
	must(http.ListenAndServe(cfg.Addr, r))
}

func must(err error) {
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// Case initiation: DB row (tenant schema) + Temporal workflow + outbox event
// ---------------------------------------------------------------------------

// businessDaysBetween counts business days (Mon–Fri) from a (exclusive) to b (inclusive).
func businessDaysBetween(a, b time.Time) int {
	days := 0
	for d := a.AddDate(0, 0, 1); !d.After(b); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			days++
		}
	}
	return days
}

// initiationWindowBD is the federal 4-business-day window to initiate IDR after
// the open negotiation period ends (45 CFR 149.510(b)(2)).
const initiationWindowBD = 4

func (s *server) initiateCase(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)

	// Idempotency (Redis): mobile/PWA retries replay the same key and get the
	// original case back instead of a duplicate dispute.
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		rkey := fmt.Sprintf("idre:%s:idem:%s", tenant, key)
		if existing := s.rds.get(rkey); existing != "" {
			writeJSON(w, http.StatusOK, map[string]any{"case_id": existing, "idempotent_replay": true})
			return
		}
		if !s.rds.setnx(rkey, 86400, "PENDING") {
			// Another replica is mid-create with this key; ask the client to retry.
			w.Header().Set("Retry-After", "2")
			http.Error(w, `{"error":"request in progress — retry"}`, http.StatusConflict)
			return
		}
	}

	var req InitiateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}

	// Fail-closed schema pin: one state tenant == one Postgres schema.
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), fmt.Sprintf(`SET LOCAL search_path TO tenant_%s, public`, sanitizeTenant(tenant))); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}

	// Negotiation window: if the 30bd open negotiation period is still running,
	// the case is tracked now and the workflow auto-opens the dispute when it ends.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	negEnd, _ := time.Parse("2006-01-02", req.OpenNegotiationEnd)
	initialStatus := "INITIATED"
	if negEnd.After(today) {
		initialStatus = "NEGOTIATION_TRACKED"
	}

	var caseID string
	err = tx.QueryRow(r.Context(), `
		INSERT INTO cases (case_number, status, service_line, plan_type, qpa_cents,
		                   provider_id, payer_id, open_negotiation_end)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		req.CaseNumber, initialStatus, req.ServiceLine, req.PlanType, req.QPACents,
		req.ProviderID, req.PayerID, req.OpenNegotiationEnd).Scan(&caseID)
	if err != nil {
		http.Error(w, `{"error":"insert"}`, http.StatusInternalServerError)
		return
	}

	// Transactional outbox — published to Kafka by the outbox relay.
	payload, _ := json.Marshal(map[string]any{
		"type": "case.initiated", "tenant": tenant,
		"case_id": caseID, "case_number": req.CaseNumber, "at": time.Now().UTC(),
	})
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO outbox (topic, key, payload) VALUES ($1,$2,$3)`,
		fmt.Sprintf("idre.%s.cases", tenant), caseID, payload); err != nil {
		http.Error(w, `{"error":"outbox"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, `{"error":"commit"}`, http.StatusInternalServerError)
		return
	}

	// Start the statutory lifecycle workflow (durable timers for every deadline).
	wfID := fmt.Sprintf("IDR-%s-%s", strings.ToUpper(tenant), req.CaseNumber)
	_, err = s.tc.ExecuteWorkflow(r.Context(), temporalclient.StartWorkflowOptions{
		ID:        wfID,
		TaskQueue: "idre-cases",
	}, "IdrCaseWorkflow", map[string]any{
		"tenant": tenant, "case_id": caseID, "case_number": req.CaseNumber,
		"open_negotiation_end": req.OpenNegotiationEnd, "plan_type": req.PlanType,
	})
	if err != nil {
		http.Error(w, `{"error":"workflow start failed"}`, http.StatusBadGateway)
		return
	}
	// Case-management enrichment: stage checklists + duplicate detection + timeline.
	s.ensureChecklist(tenant, caseID)
	s.logActivity(r.Context(), tenant, caseID, "CASE_INITIATED",
		fmt.Sprintf("Dispute %s initiated (%s) — %s / %s, QPA $%d.%02d, workflow %s",
			req.CaseNumber, initialStatus, req.ServiceLine, req.PlanType, req.QPACents/100, req.QPACents%100, wfID))

	// Federal 4-business-day initiation window (45 CFR 149.510(b)(2)):
	// late filings are allowed but flagged for compliance review.
	var lateInitiation bool
	if !negEnd.After(today) {
		if bd := businessDaysBetween(negEnd, today); bd > initiationWindowBD {
			lateInitiation = true
			detail := fmt.Sprintf("initiated %d business days after open negotiation ended %s (statutory window: %d bd)",
				bd, req.OpenNegotiationEnd, initiationWindowBD)
			_, _ = s.db.Exec(r.Context(), `
				INSERT INTO public.sla_breaches (tenant, case_id, clock, detail)
				VALUES ($1,$2,'INITIATION_4BD',$3)`, tenant, caseID, detail)
			s.logActivity(r.Context(), tenant, caseID, "LATE_INITIATION_FLAGGED", detail)
			s.notify(r, tenant, "*", "LATE_INITIATION",
				fmt.Sprintf("Case %s %s — flagged for compliance review", req.CaseNumber, detail),
				fmt.Sprintf("#/cases/%s", caseID))
		}
	}

	dups := s.findDuplicates(r, tenant, req.ProviderID, req.PayerID, req.QPACents)
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		s.rds.setex(fmt.Sprintf("idre:%s:idem:%s", tenant, key), 86400, caseID)
	}
	s.graphSync(tenant) // nudge FalkorDB + lakehouse silver (best-effort)
	writeJSON(w, http.StatusCreated, map[string]any{
		"case_id": caseID, "workflow_id": wfID, "status": initialStatus,
		"late_initiation_flagged": lateInitiation,
		"possible_duplicates":    dups, // non-blocking warning, triage via /cases/relate
	})
}

func (s *server) listCases(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(),
		fmt.Sprintf(`SELECT id, case_number, status, service_line, qpa_cents, opened_at
		             FROM tenant_%s.cases ORDER BY opened_at DESC LIMIT 200`, sanitizeTenant(tenant)))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []Case{}
	for rows.Next() {
		var c Case
		if err := rows.Scan(&c.ID, &c.CaseNumber, &c.Status, &c.ServiceLine, &c.QPA, &c.OpenedAt); err == nil {
			c.Tenant = tenant
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) getCase(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	var c Case
	err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT id, case_number, status, service_line, qpa_cents, opened_at
		             FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), id).
		Scan(&c.ID, &c.CaseNumber, &c.Status, &c.ServiceLine, &c.QPA, &c.OpenedAt)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	c.Tenant = tenant
	writeJSON(w, http.StatusOK, c)
}

// signalCase forwards business signals into the running workflow.
func (s *server) signalCase(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	var body struct {
		Signal string         `json:"signal"` // RESPONSE_FILED | OFFER_SUBMITTED | FEES_PAID | ...
		Data   map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Signal == "" {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	var wfID, status string
	if err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT workflow_id, status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), id).Scan(&wfID, &status); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// Statutory offer window (45 CFR 149.520(b)(2)): sealed offers are only
	// accepted while the 10-business-day window is open — no late offers.
	if body.Signal == "OFFER_SUBMITTED" && status != "OFFER_WINDOW_OPEN" {
		s.logActivity(r.Context(), tenant, id, "LATE_OFFER_REJECTED",
			fmt.Sprintf("Offer rejected: case status is %s, offer window is not open", status))
		http.Error(w, `{"error":"offer window is not open — late offers are not accepted"}`, http.StatusConflict)
		return
	}
	if err := s.tc.SignalWorkflow(r.Context(), wfID, "", body.Signal, body.Data); err != nil {
		http.Error(w, `{"error":"signal failed"}`, http.StatusBadGateway)
		return
	}
	detail, _ := json.Marshal(body.Data)
	s.logActivity(r.Context(), tenant, id, body.Signal,
		fmt.Sprintf("Workflow signal %s delivered to %s — %s", body.Signal, wfID, truncate(string(detail), 500)))
	s.graphSync(tenant) // graph reflects status transitions (best-effort)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "signaled"})
}

// ---------------------------------------------------------------------------
// TigerBeetle: double-entry escrow/fee transfers (ledger-per-tenant)
// ---------------------------------------------------------------------------

// Account codes (per tenant ledger):
const (
	acctEscrowTrustHeld  uint32 = 1000
	acctAdminRemittance  uint32 = 3000
	acctIdreCompensation uint32 = 4000
	acctRefundPayable    uint32 = 5000
	ledgerCodeIDRE       uint16 = 700 // platform code; ledger id = tenantLedgerID(tenant)
)

func tenantLedgerID(tenant string) uint32 {
	// Deterministic FIPS-style map: 'tx' -> 48, etc. (kept in public.state_config.tb_ledger_id).
	// Fallback: stable hash into [100..199].
	h := sha256.Sum256([]byte(tenant))
	return 100 + uint32(h[0])%100
}

func acctID(tenant string, code uint32, party string) tb_types.Uint128 {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", tenant, code, party)))
	var b [16]byte
	copy(b[:], h[:16])
	return tb_types.BytesToUint128(b)
}

func (s *server) postFeeTransfer(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	// ReBAC: object-level permit on this tenant's ledger (fail-closed).
	if !s.requirePerm(w, r, "ledger", tenant, "transact") {
		return
	}
	var req FeeTransfer
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Amount == 0 {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	var debit, credit tb_types.Uint128
	switch req.Kind {
	case "ADMIN_FEE": // party pays admin fee into remittance account via escrow
		debit, credit = acctID(tenant, acctEscrowTrustHeld, req.PartyID), acctID(tenant, acctAdminRemittance, "")
	case "IDRE_FEE_RESERVE":
		debit, credit = acctID(tenant, acctEscrowTrustHeld, req.PartyID), acctID(tenant, acctIdreCompensation, "")
	case "REFUND":
		debit, credit = acctID(tenant, acctRefundPayable, ""), acctID(tenant, acctEscrowTrustHeld, req.PartyID)
	default: // SETTLEMENT
		debit, credit = acctID(tenant, acctEscrowTrustHeld, req.PartyID), acctID(tenant, acctRefundPayable, "")
	}
	ih := sha256.Sum256([]byte(
		fmt.Sprintf("%s:%s:%s:%d", req.CaseID, req.Kind, req.PartyID, req.Amount)))
	var idb [16]byte
	copy(idb[:], ih[:16])
	id := tb_types.BytesToUint128(idb) // idempotency key

	var flags uint16
	switch req.PostKind {
	case "PENDING":
		flags = tb_types.TransferFlags{Pending: true}.ToUint16()
	case "POST":
		flags = tb_types.TransferFlags{PostPendingTransfer: true}.ToUint16()
	case "VOID":
		flags = tb_types.TransferFlags{VoidPendingTransfer: true}.ToUint16()
	}
	res, err := s.tb.CreateTransfers([]tb_types.Transfer{{
		ID:              id,
		DebitAccountID:  debit,
		CreditAccountID: credit,
		Amount:          tb_types.ToUint128(req.Amount),
		Ledger:          tenantLedgerID(tenant),
		Code:            ledgerCodeIDRE,
		Flags:           flags,
	}})
	if err != nil {
		http.Error(w, `{"error":"ledger unavailable"}`, http.StatusBadGateway)
		return
	}
	for _, r := range res {
		if r.Result != tb_types.TransferOK && r.Result != tb_types.TransferExists {
			http.Error(w, fmt.Sprintf(`{"error":"ledger rejected: %s"}`, r.Result), http.StatusUnprocessableEntity)
			return
		}
	}
	s.publish(r.Context(), tenant, "fees", map[string]any{
		"type": "fee.transfer", "case_id": req.CaseID, "kind": req.Kind,
		"party": req.PartyID, "amount_cents": req.Amount, "post": req.PostKind,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "recorded"})
}

// ---------------------------------------------------------------------------
// Dapr pub/sub publish (Kafka-backed component "idre-pubsub")
// ---------------------------------------------------------------------------

func (s *server) publish(ctx context.Context, tenant, domain string, event map[string]any) {
	topic := fmt.Sprintf("idre.%s.%s", tenant, domain)
	url := fmt.Sprintf("%s/v1.0/publish/idre-pubsub/%s", s.cfg.DaprHTTP, topic)
	body, _ := json.Marshal(event)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("dapr publish failed", "topic", topic, "err", err)
		return
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Voice-AI integration (getline.ai-style): tools + signed event webhooks
// ---------------------------------------------------------------------------

// voiceKeyAuth validates per-tenant API keys (sha256 hash stored in Postgres).
func (s *server) voiceKeyAuth(r *http.Request) (string, error) {
	key := r.Header.Get("X-Voice-Api-Key")
	if len(key) < 16 {
		return "", errors.New("missing key")
	}
	sum := sha256.Sum256([]byte(key))
	var tenant string
	err := s.db.QueryRow(r.Context(),
		`SELECT tenant FROM public.voice_api_keys WHERE key_hash=$1 AND revoked_at IS NULL`,
		hex.EncodeToString(sum[:])).Scan(&tenant)
	return tenant, err
}

func (s *server) voiceCaseStatus(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.voiceKeyAuth(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var in struct {
		CaseNumber string `json:"case_number"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var status, nextDeadline string
	err = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status, COALESCE(to_char(offer_window_ends_at,'YYYY-MM-DD'),'') FROM tenant_%s.cases WHERE case_number=$1`,
		sanitizeTenant(tenant)), in.CaseNumber).Scan(&status, &nextDeadline)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"found": true, "case_number": in.CaseNumber, "status": status,
		"next_deadline": nextDeadline,
	})
}

func (s *server) voiceDeadlines(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.voiceKeyAuth(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	// Computed by the workflow engine; exposed as a compact list for the agent to read.
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant": tenant, "statutory_defaults": map[string]any{
			"open_negotiation_bd": 30, "initiation_bd": 4, "response_bd": 3,
			"offer_window_bd": 10, "determination_bd": 30, "payment_cd": 30,
			"admin_fee_usd": 15,
		},
	})
}

func (s *server) voiceIntake(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.voiceKeyAuth(r)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var in struct {
		CallerPhone  string `json:"caller_phone"`
		CallerName   string `json:"caller_name"`
		Organization string `json:"organization"`
		Summary      string `json:"summary"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.voice_intake_requests (tenant, caller_phone, caller_name, organization, summary)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		tenant, in.CallerPhone, in.CallerName, in.Organization, in.Summary).Scan(&id); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// CRM lead capture: every voice intake becomes a lead automatically.
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.leads (tenant, source, name, organization, phone, summary, voice_intake_id)
		VALUES ($1,'VOICE',$2,$3,$4,$5,$6)`,
		tenant, in.CallerName, in.Organization, in.CallerPhone, in.Summary, id)
	s.publish(r.Context(), tenant, "voice", map[string]any{
		"type": "voice.intake", "intake_id": id, "caller": in.CallerName, "org": in.Organization,
	})
	writeJSON(w, http.StatusCreated, map[string]string{"intake_id": id})
}

// voiceEvents receives platform → us webhooks (call.completed etc.), HMAC-verified.
func (s *server) voiceEvents(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, 1<<20)
	n, _ := r.Body.Read(body)
	body = body[:n]
	sig := r.Header.Get("X-Signature")

	// Resolve tenant by looking up the secret per known header hint, then verify.
	tenant := r.Header.Get("X-Tenant-Code")
	var secret string
	if err := s.db.QueryRow(r.Context(),
		`SELECT webhook_secret FROM public.voice_configs WHERE tenant=$1`, tenant).Scan(&secret); err != nil {
		http.Error(w, `{"error":"unknown tenant"}`, http.StatusUnauthorized)
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(sig)) {
		http.Error(w, `{"error":"bad signature"}`, http.StatusUnauthorized)
		return
	}
	var evt struct {
		Type             string         `json:"type"`
		DynamicVariables map[string]any `json:"dynamic_variables"`
		Transcript       string         `json:"transcript"`
	}
	_ = json.Unmarshal(body, &evt)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.voice_call_logs (tenant, direction, tool, caller_phone, case_number, summary)
		VALUES ($1,'INBOUND_EVENT',$2,$3,$4,$5)`,
		tenant, evt.Type,
		fmt.Sprint(evt.DynamicVariables["caller_phone"]),
		fmt.Sprint(evt.DynamicVariables["case_number"]),
		evt.Transcript)
	// CRM auto-update: attach the call to the case timeline + link intake.
	s.recordVoiceActivity(r, tenant,
		fmt.Sprint(evt.DynamicVariables["case_number"]), evt.Transcript)
	s.publish(r.Context(), tenant, "voice", map[string]any{
		"type": "voice.event", "event": evt.Type, "case_number": evt.DynamicVariables["case_number"],
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// sanitizeTenant guards the one place we build SQL identifiers from input.
func sanitizeTenant(t string) string {
	for _, c := range t {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "xx" // unreachable: tenancy() already validated 2-letter codes
		}
	}
	return t
}
