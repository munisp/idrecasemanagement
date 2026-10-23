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
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"
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
	TBClusterID    string // decimal cluster ID this TigerBeetle deployment was formatted with
	DaprHTTP       string // http://localhost:3500
	// TopicPrefix namespaces every Kafka topic this deployment publishes
	// ("<prefix>.<tenant>.<domain>"). Two deployments on one broker MUST
	// use different prefixes or each consumes the other's events.
	TopicPrefix string
	// RedisPrefix starts every Redis key this deployment writes. Deployments
	// sharing one Redis keep their rate limits, idempotency keys and mail
	// pacing apart by setting it (REDIS_KEY_PREFIX); dev and idre-prod keep
	// the default and so share mail pacing, as they share one mail account.
	RedisPrefix     string
	VaultURL        string // http://vault:8081 (mTLS via Dapr in k8s)
	GraphIntelURL   string // http://graph-intel:8082 ("" = graph features disabled)
	CopilotEndpoint string // OpenAI-compatible /v1 of the LOCAL ollama ("" = copilot disabled)
	CopilotModel    string // local model name, e.g. qwen2.5:7b-instruct
	OpenSearchURL   string // http://opensearch:9200 ("" = document full-text search disabled)
	OpenSearchUser  string // basic-auth user ("" = no auth, dev only)
	OpenSearchPass  string // basic-auth password
	StripeSecret    string // sk_live_… / sk_test_… ("" = card payments disabled)
	StripeWebhook   string // whsec_… signing secret for /api/webhooks/stripe
	// MailWebhookSecret (whsec_…) signs the mail provider's delivery reports
	// (bounced/delivered/complained). Empty = the endpoint is closed.
	MailWebhookSecret string
	MojaloopAdapter   string // SDK scheme-adapter base URL ("" = mojaloop provider disabled)
	MojaloopSecret    string // HMAC secret shared with the scheme adapter
	PortalBaseURL     string // https://portal.example.gov — Stripe success/cancel return
	ClamdAddr         string // clamd:3310 — ClamAV INSTREAM target (uploads fail-closed if down)
	SMTPHost          string // outbound mail relay (state SMTP / SES / Mailgun); empty = delivery skipped
	SMTPPort          int
	SMTPUser          string
	SMTPPass          string
	SMTPFrom          string // e.g. flcdr@example.org
	WorkerToken       string // shared secret the Temporal worker (idre-workflows) authenticates service-to-service calls with
	KeycloakURL       string // http://keycloak:8080 (base, no /realms/... suffix) -- admin API calls
	KCAdmin           string // Keycloak master-realm admin username, for tenant provisioning
	KCAdminPassword   string
}

func configFromEnv() Config {
	get := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	getInt := func(k string, d int) int {
		if v := os.Getenv(k); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
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
		// No safe default: this value is fixed by whichever `tigerbeetle format
		// --cluster=N` created the replicas' on-disk state, and a wrong ID
		// doesn't error -- the client just hangs forever with every connection
		// silently rejected ("invalid header_cluster" on the replica side).
		TBClusterID:   get("TIGERBEETLE_CLUSTER_ID", ""),
		DaprHTTP:      get("DAPR_HTTP_ENDPOINT", "http://localhost:3500"),
		TopicPrefix:   get("TOPIC_PREFIX", "idre"),
		RedisPrefix:   get("REDIS_KEY_PREFIX", "idre"),
		VaultURL:      get("VAULT_URL", "http://localhost:8081"),
		GraphIntelURL: get("GRAPH_INTEL_URL", "http://localhost:8082"),
		// The copilot shares the in-cluster ollama already serving doc-intel
		// extraction and graph-intel KGQA — a shared LOCAL instance with a
		// known OOM history, so briefs are single bounded calls, never loops.
		CopilotEndpoint:   get("COPILOT_ENDPOINT", get("VLM_ENDPOINT", "http://ollama.ollama.svc.cluster.local:11434/v1")),
		OpenSearchURL:     get("OPENSEARCH_URL", ""),
		OpenSearchUser:    get("OPENSEARCH_USER", ""),
		OpenSearchPass:    get("OPENSEARCH_PASSWORD", ""),
		CopilotModel:      get("COPILOT_MODEL", get("VLM_MODEL", "qwen2.5:7b-instruct")),
		StripeSecret:      get("STRIPE_SECRET_KEY", ""),
		StripeWebhook:     get("STRIPE_WEBHOOK_SECRET", ""),
		MailWebhookSecret: get("MAIL_WEBHOOK_SECRET", ""),
		MojaloopAdapter:   get("MOJALOOP_ADAPTER_URL", ""),
		MojaloopSecret:    get("MOJALOOP_WEBHOOK_SECRET", ""),
		PortalBaseURL:     get("PORTAL_BASE_URL", "http://localhost:8080"),
		ClamdAddr:         get("CLAMD_ADDR", "localhost:3310"),
		SMTPHost:          get("SMTP_HOST", ""),
		SMTPPort:          getInt("SMTP_PORT", 587),
		SMTPUser:          get("SMTP_USER", ""),
		SMTPPass:          get("SMTP_PASS", ""),
		SMTPFrom:          get("SMTP_FROM", "idre@localhost"),
		// No default: an empty WorkerToken disables the service-auth path
		// entirely rather than accepting a guessable default as a credential.
		WorkerToken: get("WORKER_TOKEN", ""),
		// Same KEYCLOAK_URL/KC_ADMIN/KC_ADMIN_PASSWORD env vars idre-workflows
		// already uses for provision_keycloak_account (onboarding_activities.py).
		KeycloakURL:     get("KEYCLOAK_URL", "http://localhost:8085"),
		KCAdmin:         get("KC_ADMIN", ""),
		KCAdminPassword: get("KC_ADMIN_PASSWORD", ""),
	}
}

// ---------------------------------------------------------------------------
// Domain types (contracts mirror contracts/idr.ts from the spec)
// ---------------------------------------------------------------------------

type Case struct {
	ID               string    `json:"id"`
	CaseNumber       string    `json:"case_number"`
	Tenant           string    `json:"tenant"`
	Status           string    `json:"status"`
	InternalStatus   *string   `json:"internal_status,omitempty"`
	AgencyStatus     *string   `json:"agency_status,omitempty"`
	ServiceLine      string    `json:"service_line"`
	QPA              int64     `json:"qpa_cents"`
	DisputedAmount   int64     `json:"disputed_amount_cents"`
	OpenedAt         time.Time `json:"opened_at"`
	OfferWindowEnds  time.Time `json:"offer_window_ends_at"`
	BatchID          *string   `json:"batch_id,omitempty"`
	PayerID          string    `json:"payer_id"`
	TriageLane       string    `json:"triage_lane"`        // AUTO_REVIEW | STANDARD | COMPLEX
	SLADaysRemaining int       `json:"sla_days_remaining"` // 30 statutory business days, approximated
	AssignedTo       *string   `json:"assigned_to"`        // Keycloak user id; null = unassigned
	AssignedRole     *string   `json:"assigned_role,omitempty"`
	AssignedName     string    `json:"assigned_name,omitempty"` // the assignee's name, for the list
}

type InitiateRequest struct {
	CaseNumber          string `json:"case_number"` // optional when the tenant program defines a numbering pattern
	ServiceLine         string `json:"service_line"`
	PlanType            string `json:"plan_type"` // FULLY_INSURED | SELF_FUNDED
	QPACents            int64  `json:"qpa_cents"`
	ProviderID          string `json:"provider_id"`
	PayerID             string `json:"payer_id"`
	OpenNegotiationEnd  string `json:"open_negotiation_end"`  // YYYY-MM-DD
	DisputedAmountCents int64  `json:"disputed_amount_cents"` // program disputes: drives thresholds + escalation
	NumClaims           int    `json:"num_claims"`
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
	Subject  string
	Username string // preferred_username claim; "" for the worker-token service account
	Roles    []string
	Tenants  []string // from Keycloak group claim, e.g. /tenant/tx
}

// displayName prefers the human-readable username over the raw Keycloak
// subject UUID for anything that gets shown to a reviewer (QA queue's
// "Drafted by" column, audit trails) -- p.Subject alone is a bare UUID with
// no way for staff to know who actually drafted something. Falls back to
// Subject for the worker-token service principal, which carries no JWT.
func displayName(p principal) string {
	if p.Username != "" {
		return p.Username
	}
	return p.Subject
}

type authn struct {
	keys        jwk.Set
	issuer      string
	workerToken string // shared secret for service-to-service calls; "" disables this path
}

// serviceRole is the synthetic role a valid WORKER_TOKEN caller gets. Never a
// real Keycloak realm role, so nothing issued by Keycloak can collide with
// it. idre-workflows sends `Authorization: Bearer $WORKER_TOKEN` on its own
// service-to-service calls (e.g. the automated SLA-breach escalate call) --
// nothing here ever validated that token, so that path always 401'd.
const serviceRole = "SERVICE_WORKER"

func (a *authn) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if raw == "" || raw == r.Header.Get("Authorization") {
			http.Error(w, `{"error":"missing bearer token"}`, http.StatusUnauthorized)
			return
		}
		if a.workerToken != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(a.workerToken)) == 1 {
			p := principal{Subject: "service:idre-workflows", Roles: []string{serviceRole}}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal{}, p)))
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
		// preferred_username isn't guaranteed present on every token (realm/
		// client mapper config varies) -- confirmed live: a real staff
		// member's draft showed up in the QA queue as a bare UUID, which is
		// exactly what displayName()'s fallback was meant to reserve for the
		// worker-token service principal (the one caller that truly has no
		// JWT at all), not for a human whose token just omitted one claim.
		// name/email are the next-most-standard OIDC claims before falling
		// all the way back to the subject.
		for _, claim := range []string{"preferred_username", "name", "email"} {
			if u, ok := tok.Get(claim); ok {
				if s := fmt.Sprint(u); s != "" {
					p.Username = s
					break
				}
			}
		}
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
// token's tenant group claim. Access matrix:
//   - PLATFORM_ADMIN / FEDERAL_ADMIN: read+write in every state tenant.
//   - STATE_AUDITOR: read-only (GET/HEAD/OPTIONS) in every state tenant;
//     writes are rejected even in the auditor's home tenant — audit is
//     observation, not operation. An auditor who also holds an operational
//     role (e.g. CASE_MANAGER) can still write in tenants where they are a
//     group member.
//   - everyone else: only tenants present in the /tenant/<st> group claim.
func tenancy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.Context().Value(ctxPrincipal{}).(principal)
		tenant := strings.ToLower(chi.URLParam(r, "tenant"))
		if len(tenant) != 2 {
			http.Error(w, `{"error":"tenant required"}`, http.StatusBadRequest)
			return
		}
		readOnly := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		allowed := false
		for _, role := range p.Roles {
			if role == "PLATFORM_ADMIN" || role == "FEDERAL_ADMIN" || role == serviceRole {
				allowed = true
			}
			if role == "STATE_AUDITOR" && readOnly {
				allowed = true
			}
		}
		for _, t := range p.Tenants {
			if t == tenant && (readOnly || !isAuditorOnly(p)) {
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

// isAuditorOnly reports whether the principal's only elevated role is
// STATE_AUDITOR (pure auditors never write, anywhere).
func isAuditorOnly(p principal) bool {
	for _, r := range p.Roles {
		switch r {
		case "CASE_MANAGER", "ARBITRATOR", "FINANCE", "PARTY", "FEDERAL_ADMIN", "PLATFORM_ADMIN":
			return false
		}
	}
	for _, r := range p.Roles {
		if r == "STATE_AUDITOR" {
			return true
		}
	}
	return false
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
	// graphBackends: the Entity 360 relationship-graph provider chain
	// (GRAPH_BACKENDS — sql | age | falkordb; sql always last). See
	// graph_provider.go.
	graphBackends []graphProvider
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// resolveTBAddresses turns host:port entries into ip:port before the
// TigerBeetle client is constructed. No TigerBeetle client binding resolves
// DNS internally -- it needs raw IPs -- so entries that are StatefulSet
// per-pod headless-service hostnames (stable across a pod reschedule, unlike
// the pod's own IP) are resolved here instead of being frozen as IPs at
// deploy time.
func resolveTBAddresses(addrs []string) ([]string, error) {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			return nil, fmt.Errorf("bad tigerbeetle address %q: %w", a, err)
		}
		if net.ParseIP(host) != nil {
			out[i] = a
			continue
		}
		ips, err := net.LookupIP(host)
		if err != nil {
			return nil, fmt.Errorf("resolving tigerbeetle address %q: %w", a, err)
		}
		var ip net.IP
		for _, c := range ips {
			if v4 := c.To4(); v4 != nil {
				ip = v4
				break
			}
		}
		if ip == nil {
			return nil, fmt.Errorf("tigerbeetle address %q has no IPv4 record", a)
		}
		out[i] = net.JoinHostPort(ip.String(), port)
	}
	return out, nil
}

// enforceProductionSecrets refuses to boot with any known development-default
// secret when ENV/APP_ENV=production. Compose ships dev defaults for laptop
// parity; this guard makes a misconfigured prod deploy fail loudly at startup
// instead of silently serving with credentials an attacker can guess.
func enforceProductionSecrets(cfg Config) {
	env := strings.ToLower(os.Getenv("ENV"))
	if v := os.Getenv("APP_ENV"); v != "" {
		env = strings.ToLower(v)
	}
	if env != "production" && env != "prod" {
		return
	}
	var bad []string
	if strings.Contains(cfg.DatabaseURL, "idre:idre@") {
		bad = append(bad, "DATABASE_URL uses the dev idre:idre credentials")
	}
	if cfg.KCAdminPassword == "admin" || cfg.KCAdminPassword == "" {
		bad = append(bad, "KC_ADMIN_PASSWORD is unset or the dev default")
	}
	if cfg.WorkerToken == "" || cfg.WorkerToken == "dev-worker-token" {
		bad = append(bad, "WORKER_TOKEN is unset or the dev default")
	}
	if t := os.Getenv("VAULT_INTERNAL_TOKEN"); t == "" || t == "dev-internal-token" {
		bad = append(bad, "VAULT_INTERNAL_TOKEN is unset or the dev default")
	}
	if len(bad) > 0 {
		slog.Error("refusing to start in production with development-default secrets", "findings", bad)
		os.Exit(1)
	}
}

func main() {
	cfg := configFromEnv()
	enforceProductionSecrets(cfg)
	ctx := context.Background()
	stopTelemetry := initTelemetry(ctx)

	// Connection pool sized for throughput: 50 in-flight queries per replica
	// (PgBouncer txn-pooling fronts Postgres in-cluster, so 50/replica is safe).
	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	must(err)
	if v := os.Getenv("DB_POOL_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pcfg.MaxConns = int32(n)
		}
	} else {
		pcfg.MaxConns = 50
	}
	pcfg.MinConns = 10
	pcfg.MaxConnLifetime = 30 * time.Minute
	pcfg.MaxConnIdleTime = 5 * time.Minute
	pcfg.HealthCheckPeriod = 30 * time.Second
	tracePool(pcfg)
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	must(err)
	defer pool.Close()

	tc, err := temporalclient.Dial(temporalclient.Options{
		HostPort:  cfg.TemporalHost,
		Namespace: cfg.TemporalNS,
	})
	must(err)
	defer tc.Close()

	if cfg.TBClusterID == "" {
		panic("TIGERBEETLE_CLUSTER_ID is required -- must match the decimal cluster ID " +
			"the TigerBeetle replicas were formatted with (tigerbeetle format --cluster=N); " +
			"a wrong value does not error, it hangs every ledger connection forever")
	}
	clusterID, ok := new(big.Int).SetString(cfg.TBClusterID, 10)
	if !ok {
		panic("TIGERBEETLE_CLUSTER_ID is not a valid decimal integer: " + cfg.TBClusterID)
	}
	tbAddrs, err := resolveTBAddresses(strings.Split(cfg.TBAddresses, ","))
	must(err)
	tbc, err := tb.NewClient(tb_types.BigIntToUint128(*clusterID), tbAddrs)
	must(err)
	defer tbc.Close()

	rds := newRedis(envOr("REDIS_ADDR", ""), envOr("REDIS_PASSWORD", ""))

	// JWKS via Redis: all replicas share one cache; Keycloak key rotation
	// propagates within the TTL instead of hammering the certs endpoint.
	var keys jwk.Set
	// The cache is per signing-key URL: deployments on one Redis sign in on
	// different realms, and a shared key handed a starting pod the other
	// realm's keys (every login then failed until the cache expired).
	sum := sha256.Sum256([]byte(cfg.KeycloakJWKS))
	jwksKey := cfg.RedisPrefix + ":jwks:" + hex.EncodeToString(sum[:8])
	if raw := rds.get(jwksKey); raw != "" {
		keys, err = jwk.ParseString(raw)
	}
	if keys == nil {
		keys, err = jwk.Fetch(ctx, cfg.KeycloakJWKS)
		must(err)
		if buf, merr := json.Marshal(keys); merr == nil {
			rds.setex(jwksKey, 300, string(buf))
		}
	}
	a := &authn{keys: keys, issuer: cfg.KeycloakIssuer, workerToken: cfg.WorkerToken}

	docs, err := newDocStore(
		envOr("MINIO_ENDPOINT", "localhost:9000"),
		envOr("MINIO_USER", "idre"), envOr("MINIO_PASSWORD", "idre-secret"))
	must(err)

	s := &server{cfg: cfg, db: pool, tc: tc, tb: tbc, docs: docs, rds: rds,
		permify: envOr("PERMIFY_URL", "")}
	s.graphBackends = buildGraphBackends(pool, s)

	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, traceRequests, actionLog, chimw.Logger, chimw.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// Authenticated, tenant-scoped API.
	r.Route("/v1/tenants/{tenant}", func(r chi.Router) {
		r.Use(a.middleware, noteActor, tenancy, s.orgAudit, s.accessGuard, s.caseLockGuard, s.resumedUpload)
		r.Post("/cases/initiate", s.initiateCase)
		r.Get("/cases", s.listCases)
		r.Get("/cases/pipeline/counts", s.pipelineCounts) // per-stage totals for the Pipeline board (pages via /cases)
		r.Get("/cases/{caseId}", s.getCase)
		r.Get("/cases/{caseId}/summary", s.caseSummary) // the dispute in words, from current data (casesummary.go)
		r.Post("/fees/transfer", s.postFeeTransfer)     // escrow/admin/IDRE fee double-entry
		r.Post("/cases/{caseId}/signal", s.signalCase)  // e.g. response filed, fees paid

		// Documents: encrypted upload, authorized download, analysis status.
		r.Post("/cases/{caseId}/documents", s.uploadDocument)
		r.Get("/cases/{caseId}/documents", s.listDocuments)
		r.Get("/cases/{caseId}/documents/{docId}/download", s.downloadDocument)
		r.Patch("/cases/{caseId}/documents/{docId}", s.moveDocument) // re-file to folder (staff only)
		r.Get("/cases/{caseId}/documents/{docId}/analysis", s.documentAnalysis)
		r.Get("/cases/{caseId}/documents/{docId}/835.csv", s.documentAs835) // an 835 ERA text file as a table
		r.Post("/cases/{caseId}/documents/{docId}/analysis/retry", s.retryDocumentAnalysis)
		r.Post("/cases/{caseId}/documents/{docId}/analysis/feedback", s.submitDocFeedback)

		// Stakeholder onboarding: applications, decisions, status.
		r.Patch("/cases/{caseId}/details", s.setCaseDetails)
		r.Put("/cases/{caseId}/filing-packet", s.putFilingPacket) // the filing packet form (filing_packet.go)
		r.Post("/cases/{caseId}/lettergen/{templateKey}", s.requestLetterGen)
		r.Get("/cases/{caseId}/documents.zip", s.zipCaseDocuments)
		r.Post("/deliverables/request", s.requestAdhocDeliverable)
		r.Post("/onboarding/applications", s.submitApplication)
		r.Get("/onboarding/applications", s.listApplications)
		r.Post("/onboarding/applications/{appId}/decision", s.decideApplication)
		r.Post("/onboarding/applications/{appId}/documents", s.uploadApplicationDocument)
		r.Post("/onboarding/applications/{appId}/signal", s.signalApplication) // doc-intel -> StakeholderOnboardingWorkflow, SERVICE_WORKER only

		// Voice console + compliance reports (JWT-authenticated reads).
		r.Get("/voice/intake", s.listVoiceIntake)
		r.Get("/voice/logs", s.listVoiceLogs)
		r.Post("/voice/outbound", s.outboundCall)             // trigger outbound calls
		r.Get("/cases/{caseId}/activities", s.listActivities) // CRM record timeline

		// CRM core: accounts, contacts, leads, tasks, notes, search.
		r.Get("/accounts", s.listAccounts)
		r.Post("/accounts", s.createAccount)
		r.Get("/accounts/{accountId}/360", s.account360)
		// Entity 360: cross-rail organization aggregate (entity360.go).
		r.Get("/entities", s.entitySearch)
		r.Post("/entities", s.entityCreate)
		r.Post("/entities/{entityId}/links", s.entityConfirmLink)
		r.Delete("/entities/{entityId}/links/{linkId}", s.entityUnlink)
		r.Get("/entities/sources/search", s.entitySourceSearch)
		r.Post("/entities/{entityId}/merge", s.entityMerge)
		r.Get("/entity360/lookup", s.entityLookupRef)
		r.Get("/entity360/outreach", s.entityOutreach)
		r.Get("/entity360/{entityId}/graph", s.entityGraph)
		r.Get("/entity360/{entityId}", s.entity360View)
		r.Post("/contacts", s.createContact)
		r.Get("/leads", s.listLeads)
		r.Post("/leads", s.createLead)
		r.Post("/leads/{leadId}/convert", s.convertLead)
		r.Get("/tasks", s.listTasks)
		r.Post("/tasks", s.createTask)
		r.Post("/tasks/{taskId}/complete", s.completeTask)
		r.Post("/notes", s.addNote)
		r.Get("/notes", s.listNotes)
		r.Get("/search", s.globalSearch)

		// Case management: assignment, escalation, relationships, checklists,
		// calendar, notifications, saved views, letters.
		r.Post("/cases/{caseId}/assign", s.assignCase)
		r.Get("/cases/{caseId}/assignment", s.caseAssignment) // who holds the case, by name
		r.Get("/cases/assignees", s.listAssignees)            // the PM's picker: eligible staff, least loaded first
		r.Get("/assignment/auto", s.getAutoAssign)            // new cases to Reviewers in turn: on/off, the rotation, who is next
		r.Put("/assignment/auto", s.setAutoAssign)
		r.Post("/assignment/auto/assign-open", s.assignOpenCases) // the unassigned backlog, in turn (autoassign.go)
		r.Post("/cases/{caseId}/escalate", s.escalateCase)
		r.Get("/internal/ledger/balances", s.ledgerBalances)            // worker-token: reconciliation job
		r.Post("/internal/ledger/backfill", s.ledgerBackfill)           // replay past payments into the ledger (?apply=true)
		r.Post("/internal/entities/backfill", s.entityBackfill)         // link historical CRM records to entities
		r.Post("/internal/entities/graph/sync", s.entityGraphSync)      // rebuild AGE/FalkorDB entity-graph projections
		r.Post("/checks", s.uploadCheck)                                // physical check photo/scan intake
		r.Get("/checks", s.listChecks)                                  // review queue
		r.Get("/checks/{checkId}/image", s.checkImage)                  // vault-unsealed scan for portal preview
		r.Post("/checks/{checkId}/clear", s.clearCheck)                 // funds-cleared settlement
		r.Post("/checks/{checkId}/match", s.matchCheck)                 // manual link to invoice/case when auto-match (checkResult) landed it in REVIEW
		r.Post("/checks/{checkId}/feedback", s.submitCheckFeedback)     // extraction corrections -> learning loop
		r.Post("/internal/checks/{checkId}/result", s.checkResult)      // worker-token: doc-intel OCR
		r.Post("/internal/copilot/actions/apply", s.copilotApplyAction) // worker-token: Phase 3 batch executor
		r.Post("/cases/relate", s.relateCases)
		r.Get("/cases/{caseId}/relationships", s.caseRelationships)
		r.Get("/cases/{caseId}/checklist", s.getChecklist)
		r.Post("/checklists/{itemId}/check", s.checkItem)
		r.Get("/calendar", s.calendar)
		r.Get("/audit-log", s.listAuditLog)
		r.Get("/audit/mine", s.myAuditLog) // everyone's own trail (orgaudit.go)
		// Uploads (portal_uploads.go): accepted types and limits, and big files in resumable chunks.
		r.Get("/upload-policy", s.uploadPolicy)
		r.Post("/uploads", s.createUpload)
		r.Get("/uploads/{uploadId}", s.uploadStatus)
		r.Head("/uploads/{uploadId}", s.uploadStatus)
		r.Patch("/uploads/{uploadId}", s.uploadChunk)
		r.Delete("/uploads/{uploadId}", s.deleteUpload)
		// Bulk download of an organization's own disputes' documents (org_documents_zip.go).
		r.Get("/tpa/documents.zip", s.orgDocumentsZip("tpa"))
		r.Get("/hp/documents.zip", s.orgDocumentsZip("hp"))
		r.Get("/notifications", s.listNotifications)
		r.Get("/notifications/count", s.countNotifications) // the bell's unread total
		r.Post("/notifications/{notifId}/read", s.readNotification)
		r.Post("/notifications/read-all", s.readAllNotifications)
		r.Get("/views", s.listSavedViews)
		r.Post("/views", s.saveView)
		r.Get("/prefs", s.getPrefs) // server-side user preferences (source of truth)
		r.Put("/prefs", s.putPref)
		r.Get("/cases/clocks", s.casesClocks)                                          // batch statutory-clock projection (grids)
		r.Get("/cases/{caseId}/clocks", s.caseClocks)                                  // per-case projection (workspace header)
		r.Post("/cases/{caseId}/clocks/{clock}/extend", s.extendDeadline)              // PM deadline extension (deadline_extensions.go)
		r.Post("/cases/{caseId}/clocks/{clock}/extension-request", s.requestExtension) // ask the TPA / plan to agree to more time
		r.Post("/cases/bulk", s.bulkCases)                                             // bulk assign / status with per-item results
		r.Post("/queues/grab-next", s.grabNext)                                        // atomic queue claim (triage fast lane)

		// Graph intelligence (proxied to graph-intel: FalkorDB + GraphSAGE + EPR-KGQA).
		r.Post("/graph/ask", s.graphAsk)                  // EPR-KGQA natural-language query
		r.Get("/graph/hints", s.graphHints)               // real names for the sample questions
		r.Post("/graph/feedback", s.graphFeedback)        // thumbs up/down -> ART-ready log
		r.Post("/graph/sync", s.graphSyncNow)             // Postgres -> FalkorDB -> lakehouse
		r.Post("/graph/to-lakehouse", s.graphToLakehouse) // FalkorDB -> gold-zone export
		r.Post("/graph/train", s.graphTrain)              // GraphSAGE training round
		r.Get("/cases/{caseId}/related", s.caseRelated)   // GNN link predictions
		r.Get("/cases/{caseId}/graph-neighbors", s.caseGraphNeighbors)
		r.Post("/cases/{caseId}/letters/{template}", s.generateLetter)
		r.Get("/reports/sla", s.slaReport)
		r.Get("/reports/summary", s.summaryReport)

		// Operations dashboard: live presence heartbeat + manager KPI board.
		r.Post("/presence/ping", s.presencePing) // any authenticated user
		r.Get("/ops/dashboard", s.opsDashboard)  // staff roles only

		// Program rules (per-state customization; federal NSA is the no-config default).
		r.Get("/program", s.getProgram)
		r.Get("/program/configuration", s.getConfiguration)           // the Configuration page (configuration.go)
		r.Put("/program/configuration/{section}", s.putConfiguration) // fees | eligibility | clocks | billing
		r.Put("/features", s.setOwnTenantFeatures)                    // a tenant admin's own toggles (features.go)
		r.Put("/program/label", s.setTenantLabel)                     // FEDERAL_ADMIN/PLATFORM_ADMIN: rename "FL" -> "AHCA" etc.
		r.Put("/program/brand", s.setTenantBrand)                     // program admins: the portal's brand name for this tenant
		r.Get("/admin/tenant-labels", s.listTenantLabels)             // cross-tenant switcher label map
		r.Post("/cases/{caseId}/program-date", s.setProgramDate)      // record clock-basis events
		r.Post("/cases/{caseId}/status", s.setDualStatus)             // dual internal/agency status (G5)
		r.Post("/cases/{caseId}/time", s.addTimeEntry)                // per-role effort on the dispute
		r.Get("/cases/{caseId}/time", s.listTimeEntries)
		r.Get("/time/mine", s.myTimeEntries)           // personal timesheet (all disputes)
		r.Get("/time/summary", s.myTimeSummary)        // today / this week / this month totals
		r.Get("/time/prompts", s.timePrompts)          // disputes I worked on with no time recorded that day
		r.Put("/time/{entryId}", s.updateTimeEntry)    // editable entries, audited before/after
		r.Delete("/time/{entryId}", s.deleteTimeEntry) // deletion keeps the audit snapshot
		r.Get("/reports/time", s.timeReport)           // weekly per-dispute + monthly team rollup
		r.Post("/time/report/send", s.timeReportSend)  // email the team-hours report out (PM/FINANCE/admin)
		r.Get("/reports/time/rates", s.getTimeRates)   // role billable rates (view: PM/FINANCE/admin)
		r.Put("/reports/time/rates", s.putTimeRate)    // set rates (PM/admin only)
		// Service-fee invoicing engine (time ledger x rates -> invoices)
		r.Post("/billing/invoices", s.generateInvoice) // generate DRAFT for week/month/custom range
		r.Get("/billing/invoices", s.listBillingInvoices)
		r.Get("/billing/invoices/{invoiceId}", s.getBillingInvoice)
		r.Post("/billing/invoices/{invoiceId}/{action}", s.transitionInvoice)    // approve|issue|void
		r.Post("/billing/invoices/{invoiceId}/payments", s.recordInvoicePayment) // FINANCE
		r.Get("/billing/invoices/{invoiceId}/export", s.exportInvoiceCSV)        // accounting import CSV
		// Reconciliation engine + accounting adapters
		r.Post("/recon/import", s.reconImport)                    // CSV upload / configured http_json feed
		r.Post("/recon/batches/{batchId}/match", s.reconRunMatch) // deterministic auto-match
		r.Get("/recon/batches", s.reconListBatches)
		r.Get("/recon/batches/{batchId}", s.reconGetBatch)
		r.Post("/recon/items/{itemId}/resolve", s.reconResolveItem) // manual match|exception|ignore|unmatch
		r.Get("/recon/summary", s.reconSummary)                     // platform vs external drift for a period
		// AR/AP subledger (arap.go)
		r.Get("/arap/receivables", s.listReceivables) // open AR derived live from invoice stores
		r.Get("/arap/payables", s.listPayables)
		r.Post("/arap/payables", s.createPayable)                      // FINANCE records an obligation
		r.Post("/arap/payables/{payableId}/{action}", s.settlePayable) // settle|void
		r.Get("/arap/summary", s.arapSummary)                          // AR vs AP net position
		r.Post("/arap/payouts/nacha", s.createNachaPayout)             // FINANCE: ACH file from open payables
		r.Get("/arap/payouts", s.listPayoutBatches)
		r.Get("/arap/payouts/{batchId}/file", s.payoutBatchFile)
		// Payment rails (moov-io stack): vault-sealed accounts, ACH debits/returns,
		// Fedwire, X9.37 image cash letters, Metro 2 furnishing, OFAC screening.
		r.Post("/payments/accounts", s.createPaymentAccount)
		r.Get("/payments/accounts", s.listPaymentAccounts)
		r.Delete("/payments/accounts/{accountId}", s.deletePaymentAccount)
		r.Post("/payments/accounts/{accountId}/authorization", s.attachPaymentAuthorization) // vault-sealed NACHA proof-of-authorization
		r.Get("/payments/routing/{routing}", s.lookupRoutingNumber)
		r.Post("/payments/ach/debits", s.createACHDebitBatch)
		r.Post("/payments/ach/returns", s.ingestACHReturns)
		r.Post("/payments/wire", s.createWireFile)
		r.Post("/payments/icl", s.createImageCashLetter)
		r.Post("/payments/metro2", s.createMetro2File)
		r.Get("/payments/files", s.listPaymentFiles)
		r.Get("/payments/files/{fileId}/items", s.paymentFileItems)
		r.Get("/payments/files/{fileId}/file", s.paymentFileBody)
		r.Get("/screening/status", s.screeningStatus)
		r.Post("/screening/check", s.screenSubject)
		r.Get("/screening/checks", s.screeningChecks)         // the record of past screenings
		r.Get("/cases/{caseId}/financials", s.caseFinancials) // every money record tied to the dispute

		// Third-party administrators (tpa.go): file/pay/track on behalf of
		// multiple initiating parties (providers or health plans).
		r.Post("/tpa/claim", s.claimTPA)             // burn a one-time claim code, bind user -> TPA
		r.Post("/tpa/unlink", s.unlinkTPA)           // staff-only: undo "Act as this TPA"
		r.Post("/tpa/invite-code", s.tpaInviteCode)  // mint a fresh claim code to add a teammate
		r.Get("/tpa/team", s.tpaTeamList)            // the TPA's employees (tpa_team.go)
		r.Post("/tpa/team", s.tpaTeamAdd)            // add an employee: their own login, emailed to them
		r.Patch("/tpa/team/{sub}", s.tpaTeamSetRole) // ADMIN | STAFF
		r.Delete("/tpa/team/{sub}", s.tpaTeamRemove) // unlink + disable the login
		r.Get("/tpa/groups", s.tpaGroupsList)        // provider groups: TPA -> groups -> providers
		r.Post("/tpa/groups", s.tpaGroupAdd)
		r.Patch("/tpa/groups/{groupId}", s.tpaGroupUpdate)
		r.Put("/tpa/clients/{clientId}/group", s.tpaClientSetGroup)
		r.Get("/tpa/me", s.tpaMe)              // my TPA org + clients
		r.Post("/tpa/clients", s.tpaAddClient) // add an initiating party I represent
		r.Get("/tpa/clients", s.tpaListClients)
		r.Post("/tpa/clients/{clientId}/status", s.tpaSetClientStatus)
		r.Post("/tpa/intake", s.tpaIntake)                                           // file a dispute on behalf of a client
		r.Get("/tpa/dashboard", s.tpaDashboard)                                      // status of all my clients' disputes
		r.Patch("/tpa/disputes/{caseId}", s.tpaEditDispute)                          // correct my own filing while it awaits payment
		r.Get("/admin/tpas", s.adminListTPAs)                                        // state oversight
		r.Post("/admin/tpas/{tpaId}/status", s.adminSetTPAStatus)                    // suspend|reactivate
		r.Post("/cases/{caseId}/eligibility", s.checkEligibility)                    // threshold matrix + filing window (G2)
		r.Get("/cases/{caseId}/eligibility", s.eligibilityHistory)                   // past reviews (G2)
		r.Post("/cases/{caseId}/eligibility/{reviewId}/notes", s.addEligibilityNote) // notes on a review (Reviewer, PM, Attorney)
		r.Get("/cases/{caseId}/eligibility/checklist", s.eligibilityChecklist)       // printable Eligibility Checklist
		r.Get("/cases/{caseId}/workflow", s.caseWorkflow)                            // the SOP steps: done, next, upcoming
		r.Post("/cases/{caseId}/outcome", s.recordOutcome)                           // Reviewer: outcome, claims reviewed, award, party billed
		r.Get("/cases/{caseId}/decision-report", s.getDecisionReport)                // the written decision: sections, amounts, claims tables
		r.Put("/cases/{caseId}/decision-report", s.putDecisionReport)
		r.Post("/cases/{caseId}/decision-report/file", s.attachDecisionReport) // the finished report, uploaded
		r.Get("/letter-settings", s.getLetterSettings)                         // letterhead, Agency addressee, invoice terms, remit-to
		r.Put("/letter-settings", s.putLetterSettings)
		r.Get("/template-variables", s.getTemplateVariables) // the office's values and its own {variables}
		r.Put("/template-variables", s.putTemplateVariables)
		r.Post("/cases/{caseId}/fwa", s.fwaAction)                  // fraud/waste/abuse: report, assign reviewer, conclude
		r.Put("/program/contract-dates", s.setContractDates)        // admin: contract execution/effective/end dates
		r.Get("/program/recipients", s.getCorrespondenceRecipients) // agency contacts + PM copy addresses
		r.Put("/program/recipients", s.setCorrespondenceRecipients) // admin
		r.Get("/program/templates", s.listTemplates)                // the Templates screen
		r.Post("/program/templates/preview", s.previewTemplate)     // placeholders filled from a real case
		r.Put("/program/templates/usage", s.putTemplateUsage)       // which template is used when (PM/admin)
		r.Delete("/program/templates/usage/{id}", s.deleteTemplateUsage)
		r.Get("/program/templates/{key}/history", s.templateHistory)
		r.Put("/program/templates/{key}", s.putTemplate)       // add or change a template (PM/admin)
		r.Delete("/program/templates/{key}", s.deleteTemplate) // remove one, or restore the standard wording
		r.Get("/cases/{caseId}/correspondence/templates", s.caseTemplates)
		r.Put("/cases/{caseId}/review-notes", s.setReviewNote)        // per-role notes fields
		r.Put("/cases/{caseId}/dispute-summary", s.setDisputeSummary) // Coder's Dispute Summary + cost estimate
		r.Post("/cases/{caseId}/aor-confirmation", s.confirmAOR)      // Attorney rules on a flagged AOR
		// TPA onboarding (tpa_onboarding.go): profile -> payment -> 6-doc package.
		r.Put("/tpa/clients/{clientId}/profile", s.tpaUpdateClientProfile)
		r.Post("/tpa/clients/import", s.tpaImportClientsCSV)        // bulk roster (CSV)
		r.Post("/tpa/clients/{clientId}/payments", s.tpaAddPayment) // CARD/ACH rail
		r.Get("/tpa/clients/{clientId}/payments", s.tpaListPayments)
		r.Post("/tpa/clients/{clientId}/packages", s.tpaCreatePackage)
		r.Post("/tpa/packages/{packageId}/docs", s.tpaAttachDoc)           // one slot
		r.Delete("/tpa/packages/{packageId}/docs/{slot}", s.tpaRemoveDoc)  // take a file out of a set not yet filed
		r.Post("/tpa/packages/{packageId}/merged", s.tpaUploadMerged)      // one giant PDF
		r.Get("/tpa/packages/{packageId}", s.tpaPackageStatus)             // slot verdict grid
		r.Post("/tpa/packages/bulk", s.tpaUploadBulkZip)                   // ZIP of client folders
		r.Post("/tpa/disputes/bulk", s.tpaBulkDisputes)                    // many disputes, each with its own six documents (ZIP)
		r.Get("/tpa/disputes/bulk/sample.zip", s.tpaBulkSample)            // a sample ZIP to copy (tpa_bulk_sample.go)
		r.Get("/tpa/disputes/bulk", s.tpaBulkDisputeBatches)               // my bulk dispute batches
		r.Get("/tpa/disputes/bulk/{batchId}", s.tpaBulkDisputeBatch)       // per-row report: documents, case number
		r.Post("/tpa/disputes/bulk/{batchId}/checkout", s.tpaBulkCheckout) // one payment for the batch's filed, unpaid disputes
		r.Get("/tpa/fees", s.orgFees("tpa"))                               // what this TPA owes now
		r.Post("/tpa/fees/checkout", s.tpaFeesCheckout)                    // one payment for all unpaid fees
		// Saved cards and bank accounts (ACH), the TPA's own and each client's;
		// a Pay with {"source":"CLIENT"|"TPA"} charges them (tpa_saved_payments.go).
		r.Post("/tpa/payment-methods/setup", s.tpaPaymentSetup)
		r.Post("/tpa/payment-methods/setup/{sessionId}/confirm", s.tpaPaymentSetupConfirm)
		r.Get("/tpa/payment-methods", s.tpaSavedMethodsList)
		r.Delete("/tpa/payment-methods/{methodId}", s.tpaSavedMethodRemove)
		r.Post("/tpa/fees/options", s.tpaFeeOptions)
		r.Post("/tpa/tasks", s.tpaTaskCreate)
		r.Post("/tpa/tasks/{taskId}/done", s.tpaTaskDone)
		r.Get("/tpa/fees/runs/{runId}", s.tpaFeeRun)
		r.Get("/tpa/audit", s.orgAuditLog("tpa")) // the TPA's own audit trail (orgaudit.go)
		r.Get("/hp/audit", s.orgAuditLog("hp"))   // the plan's own audit trail
		// Document requests (document_requests.go): asked on the case, answered in the portal.
		r.Get("/tpa/sent-documents", s.orgSentDocs("tpa")) // what was sent to the TPA, by dispute (party_documents.go)
		r.Get("/tpa/sent-documents/{docId}", s.orgSentDocDownload("tpa"))
		r.Get("/hp/sent-documents", s.orgSentDocs("hp"))
		r.Get("/hp/sent-documents/{docId}", s.orgSentDocDownload("hp"))
		r.Post("/party-documents/backfill", s.backfillPartyDocs)
		r.Get("/tpa/document-requests", s.orgDocRequests("tpa"))
		r.Post("/tpa/document-requests/{reqId}/files", s.orgDocRequestUpload("tpa"))
		r.Post("/tpa/document-requests/{reqId}/submit", s.orgDocRequestSubmit("tpa"))
		r.Get("/hp/document-requests", s.orgDocRequests("hp"))
		r.Post("/hp/document-requests/{reqId}/files", s.orgDocRequestUpload("hp"))
		r.Post("/hp/document-requests/{reqId}/submit", s.orgDocRequestSubmit("hp"))
		r.Get("/cases/{caseId}/document-requests", s.listCaseDocRequests)
		r.Post("/cases/{caseId}/document-requests", s.createDocRequest)
		r.Post("/cases/{caseId}/document-requests/{reqId}/close", s.closeDocRequest)
		// Fee payments (fee_payments.go): one dispute, one provider, a bulk upload, or everything.
		r.Post("/tpa/clients/{clientId}/fees/checkout", s.tpaClientCheckout)
		r.Post("/tpa/disputes/{caseId}/pay", s.disputePay("tpa"))
		r.Get("/tpa/disputes/{caseId}/fee", s.disputeFee("tpa"))
		r.Get("/tpa/disputes/{caseId}/summary", s.orgDisputeSummary("tpa")) // the dispute as the TPA sees it (party_dispute_view.go)
		r.Get("/hp/disputes/{caseId}/summary", s.orgDisputeSummary("hp"))
		r.Post("/tpa/fees/checkout/{groupId}/confirm", s.checkoutConfirm("tpa"))
		r.Post("/tpa/fees/checkout/{groupId}/cancel", s.checkoutCancel("tpa"))
		r.Get("/hp/fees", s.orgFees("hp"))
		r.Post("/hp/fees/checkout", s.hpFeesCheckout)
		r.Post("/hp/disputes/{caseId}/pay", s.disputePay("hp"))
		r.Get("/hp/disputes/{caseId}/fee", s.disputeFee("hp"))
		r.Post("/hp/fees/checkout/{groupId}/confirm", s.checkoutConfirm("hp"))
		r.Post("/hp/fees/checkout/{groupId}/cancel", s.checkoutCancel("hp"))
		r.Get("/tpa/onboarding", s.tpaOnboardingBoard) // roster readiness
		// Health plan onboarding (healthplan_onboarding.go): payer-side mirror.
		r.Post("/hp/claim", s.claimHealthPlan)
		r.Post("/hp/unlink", s.hpUnlink) // staff: stop acting for a plan
		// The other side of disputes (respondents.go): a plan answers the
		// disputes filed against it (a TPA files; it does not answer).
		r.Get("/hp/responding", s.respondingList("plan"))
		r.Get("/hp/responding/{caseId}", s.respondingCase("plan"))
		r.Get("/hp/responding/{caseId}/documents/{docId}/file", s.respondingDocFile("plan"))
		r.Post("/hp/responding/{caseId}/documents", s.respondingUpload("plan"))
		r.Post("/hp/responding/{caseId}/submit", s.respondingSubmit("plan"))
		r.Post("/hp/responding/{caseId}/opt-out", s.respondingOptOut)
		r.Get("/onboarding/verification-reviews", s.verificationReviews) // exclusion-list (HHS-OIG) matches
		r.Post("/onboarding/verification-reviews/{reviewId}", s.verificationReviewDecide)
		r.Get("/onboarding/npi-reviews", s.npiReviews) // client NPIs the registry holds under another name
		r.Post("/onboarding/npi-reviews/{kind}/{id}", s.npiReviewDecide)
		r.Get("/onboarding/reviews", s.onboardingReviews) // documents doc-intel left for a person (both rails)
		r.Get("/onboarding/reviews/{kind}/{docId}/file", s.onboardingReviewFile)
		r.Post("/onboarding/reviews/{kind}/{docId}", s.onboardingReviewDecide)
		r.Get("/hp/me", s.hpMe)
		r.Post("/hp/parties", s.hpAddParty)
		r.Put("/hp/parties/{partyId}/profile", s.hpUpdatePartyProfile)
		r.Post("/hp/parties/import", s.hpImportPartiesCSV)
		r.Post("/hp/parties/{partyId}/payments", s.hpAddPayment)
		r.Get("/hp/parties/{partyId}/payments", s.hpListPayments)
		r.Post("/hp/parties/{partyId}/packages", s.hpCreatePackage)
		r.Post("/hp/packages/{packageId}/docs", s.hpAttachDoc)
		r.Post("/hp/packages/{packageId}/merged", s.hpUploadMerged)
		r.Get("/hp/packages/{packageId}", s.hpPackageStatus)
		r.Post("/hp/packages/bulk", s.hpUploadBulkZip)
		r.Get("/hp/onboarding", s.hpOnboardingBoard)
		r.Post("/hp/intake", s.hpIntake)
		r.Get("/admin/healthplans", s.adminListHealthPlans)
		r.Post("/admin/healthplans/{planId}/status", s.adminSetHealthPlanStatus)
		r.Post("/cases/{caseId}/eligibility/auto", s.autoEligibility) // auto-adjudicate from case+doc data (Lever 1)
		r.Post("/cases/{caseId}/copilot/brief", s.copilotBrief)       // grounded advisory brief (Phase 1 copilot)
		r.Get("/cases/{caseId}/copilot/brief", s.copilotBriefLatest)  // latest persisted brief
		r.Post("/cases/{caseId}/copilot/draft", s.copilotDraft)       // Phase 2: QA-gated determination/correspondence drafts
		r.Get("/cases/{caseId}/copilot/chat", s.copilotChatHistory)   // Assistant thread (conversational surface, private per worker)
		r.Post("/cases/{caseId}/copilot/chat", s.copilotChat)
		r.Post("/cases/{caseId}/copilot/chat/log", s.copilotChatLog) // fast-path intents: persist without calling the model
		r.Get("/cases/{caseId}/copilot/chat/shares", s.copilotListThreadShares)
		r.Post("/cases/{caseId}/copilot/chat/shares", s.copilotShareThread)
		r.Delete("/cases/{caseId}/copilot/chat/shares/{userId}", s.copilotUnshareThread)
		r.Get("/copilot/chat", s.copilotGeneralChatHistory) // floating-widget thread, no case open
		r.Post("/copilot/chat", s.copilotGeneralChat)
		r.Post("/copilot/chat/log", s.copilotGeneralChatLog)               // fast-path intents: persist without calling the model
		r.Get("/assistant/briefing", s.briefing)                           // worker-scoped morning digest (conversation-first step 2)
		r.Post("/cases/{caseId}/copilot/actions", s.copilotProposeActions) // Phase 3: bounded action-batch proposal
		r.Get("/cases/{caseId}/copilot/actions", s.copilotListActionBatches)
		r.Post("/cases/{caseId}/copilot/actions/{batchId}/decision", s.copilotDecideActions) // human gate -> Temporal signal
		r.Post("/cases/{caseId}/correspondence", s.draftCorrespondence)                      // template draft / send (G3)
		r.Post("/cases/{caseId}/correspondence/preview", s.previewCorrespondence)            // how it will read, before it is sent (mail_preview.go)
		r.Get("/cases/{caseId}/correspondence", s.listCorrespondence)
		r.Get("/cases/{caseId}/mail-tracker", s.caseMailTracker) // every email of the case, tracked (mail_tracker.go)
		r.Get("/correspondence", s.listMailJournal)              // tenant-wide mail journal (IN+OUT, incl. pre-case and unmatched)
		r.Post("/cases/{caseId}/share-links", s.createShareLink) // tokenized upload/download (G9)

		// Double-blind offers (45 CFR 149.520(b)) + specialty review queues +
		// authenticated party workspace.
		r.Post("/cases/{caseId}/offers", s.submitOffer)               // vault-sealed; window-gated
		r.Get("/cases/{caseId}/offers", s.listOffers)                 // metadata for all; amounts post-reveal
		r.Post("/cases/{caseId}/offers/reveal", s.revealOffers)       // ARBITRATOR; vault re-verifies
		r.Post("/cases/{caseId}/determination", s.issueDetermination) // baseball-style: select one offer
		r.Post("/cases/{caseId}/reviews", s.requestReview)            // CODING|CLINICAL|LEGAL worklist entry
		r.Get("/queues/{discipline}", s.reviewQueue)                  // specialist worklist
		r.Post("/reviews/{reviewId}/claim", s.claimReview)
		r.Post("/reviews/{reviewId}/complete", s.completeReview)   // fires the workflow signal
		r.Get("/queues/pm", s.pmQueue)                             // escalations needing PM attention
		r.Get("/cases/{caseId}/benchmarks", s.caseBenchmarks)      // per-code QPA distribution (arbitrator/staff)
		r.Get("/cases/{caseId}/batchable", s.batchableCases)       // 45 CFR 149.510(c) batching suggestions
		r.Get("/cases/{caseId}/win-probability", s.winProbability) // same-payer/code outcomes (n>=5)
		r.Get("/analytics/payers", s.payerAnalytics)               // denial-pattern profile per payer
		r.Get("/analytics/fraud-signals", s.fraudSignals)          // oversight: provider tripwire clusters
		r.Get("/analytics/network-adequacy", s.networkAdequacy)    // OON concentration evidence
		r.Post("/cases/{caseId}/party-invites", s.inviteParty)     // provision Keycloak party account
		r.Get("/cases/{caseId}/party-accounts", s.listPartyAccounts)
		r.Get("/party/cases", s.partyCases) // party workspace
		r.Get("/party/cases/{caseId}", s.partyCaseDetail)
		r.Get("/party/cases/{caseId}/respond", s.respondingCase("party")) // an invited responding party answers
		r.Get("/party/cases/{caseId}/respond/documents/{docId}/file", s.respondingDocFile("party"))
		r.Post("/party/cases/{caseId}/respond/documents", s.respondingUpload("party"))
		r.Post("/party/cases/{caseId}/respond/submit", s.respondingSubmit("party"))
		r.Get("/qa", s.qaQueue) // QA gate queue (G4)
		r.Get("/qa/{qaId}", s.qaGet)
		r.Post("/qa/{qaId}/decision", s.qaDecision)
		r.Post("/qa/{qaId}/send", s.qaSend)                // the drafter sends a draft that has passed QA
		r.Post("/cases/{caseId}/invoices", s.issueInvoice) // dual-party receivables (G8)
		r.Get("/cases/{caseId}/invoices", s.listInvoices)
		r.Get("/invoices", s.listInvoices)
		r.Post("/invoices/{invId}/settle", s.settleInvoice) // PAY|REFUND|VOID
		r.Get("/reports/receivables", s.receivablesReport)
		r.Post("/invoices/{invId}/checkout", s.createCheckout) // Stripe Checkout session
		r.Get("/payments", s.listPayments)                     // payment history (tenant)
		r.Get("/cases/{caseId}/payments", s.listPayments)      // payment history (case)
		r.Get("/reports/financial", s.financialReport)         // finance dashboard aggregate
		r.Get("/reports/unapplied", s.listUnapplied)           // money received that pays no fee (unapplied.go)
		r.Post("/finance/unapplied/{id}/resolve", s.resolveUnapplied)
		r.Get("/reports/refunds-due", s.refundsDue)      // refunds the workflow ordered, not yet made (refunds_due.go)
		r.Get("/reports/fl/weekly", s.flWeeklyReport)    // contract 2.4.2: 16-col A-P CSV
		r.Get("/reports/fl/monthly", s.flMonthlyReport)  // contract 2.4.3: 33-field CSV
		r.Get("/reports/fl/adhoc", s.flAdhocReport)      // contract 2.4.4: chosen columns, period, statuses
		r.Get("/reports/fl/final", s.flFinalReport)      // contract 2.4.5: every case, monthly layout
		r.Get("/reports/fl/columns", s.flReportColumns)  // what an ad hoc report can contain
		r.Post("/cases/{caseId}/claims", s.importClaims) // bulk claim lines (G10)
		r.Get("/cases/{caseId}/claims", s.listClaims)
		r.Put("/cases/{caseId}/claims/eligibility", s.setClaimEligibility) // a partially accepted case's ineligible claims
		r.Post("/intake/converse", s.intakeConverse)                       // conversational intake (step 5): extraction only — filing stays on /intake
		r.Post("/intake", s.createIntake)                                  // pre-case intake (G12)
		r.Post("/intake/bulk", s.bulkIntake)                               // third-party batch filing (idempotent by batch_ref)
		r.Get("/intake/bulk", s.listIntakeBatches)                         // filer status board (own batches; staff may filter)
		r.Get("/intake/bulk/{batchId}", s.getIntakeBatch)                  // batch receipt / reconciliation
		r.Get("/intake", s.listIntake)
		r.Post("/intake/{intakeId}/advance", s.advanceIntake) // refund window enforced

		// Rule engine administration (admin roles only; every write audited).
		r.Get("/rules", s.listRules)
		r.Put("/rules", s.putRules)
		r.Get("/rules/audit", s.rulesAudit)
		r.Get("/deliverables", s.listDeliverables) // contract schedule (G7)
		r.Post("/deliverables", s.submitDeliverable)
		r.Post("/cases/{caseId}/opt-out", s.recordOptOut) // plan opt-out adjudication (G14)

		// Phone call tracker + inquiries log: audit trail independent of
		// formal templated correspondence; tenant-level routes cover
		// pre-case contacts (no case_id yet).
		r.Post("/calls", s.logCall)
		r.Get("/calls", s.listCalls)
		r.Post("/cases/{caseId}/calls", s.logCall)
		r.Get("/cases/{caseId}/calls", s.listCalls)
		r.Post("/inquiries", s.logInquiry)
		r.Get("/inquiries", s.listInquiries)
		r.Post("/cases/{caseId}/inquiries", s.logInquiry)
		r.Get("/cases/{caseId}/inquiries", s.listInquiries)
	})

	// Platform admin: not tenant-scoped (tenancy() requires an existing
	// /tenant/<st> group membership, which is exactly what this creates) --
	// auth only, then an inline FEDERAL_ADMIN/PLATFORM_ADMIN role check.
	r.Route("/v1/admin", func(r chi.Router) {
		r.Use(a.middleware, noteActor)
		r.Post("/tenants", s.createTenant)
		r.Put("/tenants/{tenant}/features", s.setTenantFeatures)
		r.Post("/federal-admins", s.createFederalAdmin)
		r.Get("/federal-admins", s.listFederalAdmins)
		r.Patch("/federal-admins/{username}", s.updateFederalAdminStatus)
		r.Delete("/federal-admins/{username}", s.deleteFederalAdmin)
		r.Post("/tenant-staff", s.createTenantStaff)
		r.Get("/tenant-staff", s.listTenantStaff)
		r.Patch("/tenant-staff/{username}", s.updateTenantStaffStatus)
		r.Put("/tenant-staff/{username}/roles", s.setTenantStaffRoles) // what a staff member does (staffroles.go)
		r.Delete("/tenant-staff/{username}", s.deleteTenantStaff)
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

	// ShareBox (no OIDC; token + expiry + use-count is the auth):
	// HTML landing page + real no-login upload/download endpoints.
	r.Get("/api/share/{token}", s.shareLanding)
	r.Head("/api/share/{token}", s.resolveShareLink)
	r.Get("/api/share/{token}/meta", s.resolveShareLink)
	r.Post("/api/share/{token}/chat", s.partyChat)                       // party voice (step 4): token-gated, no use consumed
	r.Post("/api/share/{token}/upload", s.shareUpload)                   // one-shot, small files
	r.Post("/api/share/{token}/uploads", s.shareCreateUpload)            // resumable: create
	r.Head("/api/share/{token}/uploads/{uploadId}", s.shareUploadOffset) // resume probe
	r.Patch("/api/share/{token}/uploads/{uploadId}", s.shareUploadChunk) // next chunk
	r.Post("/api/share/{token}/uploads/{uploadId}/complete", s.shareCompleteUpload)
	r.Get("/api/share/{token}/download", s.shareDownload) // Range/resume supported

	// Vault reveal-check oracle (no OIDC; X-Internal-Auth == VAULT_INTERNAL_TOKEN
	// is the auth). The vault calls this before decrypting any offer — reveals
	// only a boolean + reason header, no case data.
	r.Get("/internal/reveal-check/{tenant}/{caseId}", s.revealCheck)
	// The FeePaymentWorkflow's activities (same internal token; tpa_saved_payments.go).
	r.Post("/internal/payments/{tenant}/groups/{groupId}/charge", s.internalChargeGroup)
	r.Post("/internal/payments/{tenant}/groups/{groupId}/check", s.internalCheckGroup)
	r.Post("/internal/payments/{tenant}/groups/{groupId}/give-up", s.internalGiveUpGroup)
	r.Post("/internal/payments/{tenant}/runs/{runId}/summary", s.internalRunSummary)

	// Stripe webhook (no OIDC; HMAC-SHA256 signature against STRIPE_WEBHOOK_SECRET is the auth).
	r.Post("/api/webhooks/stripe", s.stripeWebhook)
	r.Post("/api/webhooks/mail", s.mailWebhook)                // provider delivery reports -> mail log status
	r.Post("/api/webhooks/lockbox/{tenant}", s.lockboxWebhook) // bank deposit confirmation (HMAC)
	r.Post("/api/webhooks/mojaloop", s.mojaloopWebhook)

	// Public stakeholder application (no OIDC; per-IP throttled, tenant + type validated).
	r.Post("/api/public/apply", s.publicApply)

	// Public pre-case intake for programmed tenants (no OIDC; per-IP throttled).
	r.Post("/api/public/ahca-intake", s.publicAhcaIntake)
	r.Post("/api/public/tpa/register", s.registerTPA)
	r.Get("/api/public/brand", s.publicBrand)
	r.Get("/api/public/email-check", s.emailCheck) // does this address take mail (emailcheck.go)
	r.Post("/api/public/healthplan/register", s.registerHealthPlan)

	// Day-13 intake completeness gate (AHCA 2026): hourly sweep flips stale
	// intakes to INELIGIBLE and raises staff notifications for the letters.
	// Idempotent (status-transition guarded); safe across replicas.
	sweepStop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepStop:
				return
			default:
			}
			if n := s.sweepIntakeDay13(); n > 0 {
				slog.Info("day-13 sweep", "ineligible", n)
			}
			if n := s.sweepReportDeliverables(); n > 0 {
				slog.Info("report deliverables sweep", "opened", n)
			}
			if n := s.sweepPaymentReminders(); n > 0 {
				slog.Info("payment reminders sweep", "drafted", n)
			}
			if n := s.sweepLateFees(); n > 0 {
				slog.Info("late fee sweep", "charged", n)
			}
			s.sweepStepTasks()
			select {
			case <-sweepStop:
				return
			case <-t.C:
			}
		}
	}()

	// Mail the provider turned away for now (mailqueue.go) goes out again as soon as it may.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepStop:
				return
			case <-t.C:
				if n := s.sweepMailQueue(); n > 0 {
					slog.Info("mail queue sweep", "sent", n)
				}
			}
		}
	}()

	// No upload left unread (docrequeue.go): every few minutes, documents the
	// reader should have read and has not are announced to it again; one it
	// still cannot read is flagged to the people on the case.
	// Bulk TPA disputes (tpa_bulk_disputes.go): a row is filed once its own
	// six documents are accepted.
	go s.runBulkDisputeFiler(sweepStop)
	go s.catchUpStepQueues(sweepStop) // cases already at a specialist's step get their queue item (helpers.go)
	go s.backfillPackageAnalyses()
	go s.backfillPartyDocsAll() // letters sent before the parties' record existed (party_documents.go)

	go func() {
		t := time.NewTicker(docSweepEvery)
		defer t.Stop()
		for {
			select {
			case <-sweepStop:
				return
			case <-t.C:
			}
			if requeued, stalled := s.sweepUnreadDocuments(); requeued+stalled > 0 {
				slog.Info("unread documents sweep", "requeued", requeued, "stalled", stalled)
			}
		}
	}()

	srv := &http.Server{Addr: cfg.Addr, Handler: r,
		ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second}
	// Graceful shutdown: drain in-flight requests on SIGTERM/SIGINT so a
	// rolling deploy never truncates an upload or a payment webhook.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		close(sweepStop)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		stopTelemetry(ctx)
	}()

	s.startWatchmanRefresher() // OFAC SDN list: refresh now + every 24h (best-effort)

	slog.Info("case-api listening", "addr", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		must(err)
	}
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
	// PARTY included deliberately: the portal's "New dispute" nav link (and
	// this exact form -- CMS case number, QPA, provider/payer IDs) is shown
	// to PARTY as self-service federal NSA filing, not just staff. Caught by
	// cross-checking the UI's own nav gate against this handler's roles
	// after first writing it without PARTY.
	// ARBITRATOR removed (access-control audit, AC-5 separation of duties):
	// the role that decides a dispute shouldn't also be able to file one --
	// undermines arbitrator impartiality regardless of whether it's misused.
	// CASE_MANAGER removed for the same reason, per the "reviewer only
	// reviews" follow-up: filing is a creation action, deciding is a review
	// action, and reviewer keeps only the latter. PM takes over this duty.
	// TENANT_ADMIN added back -- see reportsReadRoles (casemgmt.go) for why.
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "PARTY", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires PARTY, PM, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}

	// Idempotency (Redis): mobile/PWA retries replay the same key and get the
	// original case back instead of a duplicate dispute.
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		rkey := fmt.Sprintf("%s:%s:idem:%s", s.cfg.RedisPrefix, tenant, key)
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

	// Program numbering (G11): when the tenant runs a custom program with a
	// numbering pattern and the caller leaves case_number blank, generate it
	// (e.g. FL26-042). Explicit case numbers are still honored.
	prog := s.loadProgram(r, tenant)
	if req.CaseNumber == "" && prog != nil && prog.CaseNumber.Pattern != "" {
		req.CaseNumber = s.nextCaseNumber(r, tenant, prog)
	}

	var caseID string
	err = tx.QueryRow(r.Context(), `
		INSERT INTO cases (case_number, status, service_line, plan_type, qpa_cents,
		                   provider_id, payer_id, open_negotiation_end,
		                   disputed_amount_cents, num_claims)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		req.CaseNumber, initialStatus, req.ServiceLine, req.PlanType, req.QPACents,
		req.ProviderID, req.PayerID, req.OpenNegotiationEnd,
		req.DisputedAmountCents, req.NumClaims).Scan(&caseID)
	if err != nil {
		// A duplicate case_number under concurrent requests correctly hits
		// the unique constraint (verified elsewhere: exactly 1 row survives
		// a 10-way race) -- but that's a client error (retry won't help with
		// the same number), not a server fault, so it gets its own status
		// instead of an indistinguishable-from-a-real-bug 500.
		if isUniqueViolation(err) {
			http.Error(w, `{"error":"case_number already exists"}`, http.StatusConflict)
			return
		}
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
		fmt.Sprintf("%s.%s.cases", s.cfg.TopicPrefix, tenant), caseID, payload); err != nil {
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
	s.ensureChecklist(r, tenant, caseID)
	s.autoAssignNewCase(r, tenant, caseID, req.CaseNumber) // the next Reviewer in turn (autoassign.go)
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

	// Program escalation trigger (G6): auto-route over-threshold disputes.
	s.escalationTrigger(r, tenant, caseID, req.DisputedAmountCents)

	dups := s.findDuplicates(r, tenant, req.ProviderID, req.PayerID, req.QPACents)
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		s.rds.setex(fmt.Sprintf("%s:%s:idem:%s", s.cfg.RedisPrefix, tenant, key), 86400, caseID)
	}
	s.graphSync(tenant, caseID) // the new case in the graph (best-effort)
	s.logAudit(r.Context(), tenant, caseID, "CASE_INITIATED", map[string]any{
		"by": r.Context().Value(ctxPrincipal{}).(principal).Subject, "case_number": req.CaseNumber,
		"status": initialStatus, "workflow_id": wfID, "late_initiation": lateInitiation,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"case_id": caseID, "workflow_id": wfID, "status": initialStatus,
		"late_initiation_flagged": lateInitiation,
		"possible_duplicates":     dups, // non-blocking warning, triage via /cases/relate
	})
}

// listCases: keyset-paginated dispute list. Enterprise tenants hold thousands
// of disputes (FL alone projects ~3k/yr), so a hard LIMIT 200 silently hid
// cases once a tenant outgrew it. Params:
//
//	?limit=N      page size, default 50, max 200
//	?cursor=ts|id keyset position from a previous page's next_cursor
//	?status=S     exact status filter (saved views push filtering server-side
//	              so a filter never applies to a partial page)
//	?q=text       case_number ILIKE search
//
// Response: {"cases": [...], "next_cursor": "...", "total": N}
// caseServiceSQL is a case's service line as people see it: the one set on the case, else the packet's type of
// service, else its claim lines' (a dispute filed through the filing packet records the service there, and its
// service_line column stays empty). c = the case row; tenant = $1.
const caseServiceSQL = `coalesce(nullif(c.service_line,''), nullif(c.details->>'type_of_service',''),
	(SELECT cl.type_of_service FROM public.case_claims cl WHERE cl.tenant=$1 AND cl.case_id=c.id::text AND coalesce(cl.type_of_service,'') <> ''
	 GROUP BY 1 ORDER BY count(*) DESC, 1 LIMIT 1), '')`

// caseSearchSQL is the ?q= filter on cases aliased c, the term (a LIKE pattern) being $n and the tenant $1. The
// Disputes list and the Pipeline board's counts both use it, so a search finds the same cases in both.
func caseSearchSQL(n int) string {
	return fmt.Sprintf(` AND (c.case_number ILIKE $%[1]d OR `+caseServiceSQL+` ILIKE $%[1]d
			OR coalesce(c.internal_status,'') ILIKE $%[1]d OR coalesce(c.agency_status,'') ILIKE $%[1]d
			OR coalesce(c.payer_id,'') ILIKE $%[1]d OR coalesce(c.details::text,'') ILIKE $%[1]d
			OR EXISTS (SELECT 1 FROM public.case_origin o WHERE o.tenant=$1 AND o.case_id=c.id::text
			           AND (coalesce(o.initiating_party_name,'') ILIKE $%[1]d OR coalesce(o.filed_by_email,'') ILIKE $%[1]d))
			OR EXISTS (SELECT 1 FROM public.case_respondents rp WHERE rp.tenant=$1 AND rp.case_id=c.id::text
			           AND (coalesce(rp.name,'') ILIKE $%[1]d OR coalesce(rp.email,'') ILIKE $%[1]d OR coalesce(rp.npi,'') ILIKE $%[1]d)))`, n)
}

func (s *server) listCases(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	// TPA deliberately excluded: this is the tenant's whole case list, no
	// per-caller scoping -- a TPA only has standing to see the disputes IT
	// filed, which is exactly what #/tpa's own "Tracking" card already
	// reads from case_origin. Letting TPA hit this generic endpoint (via
	// the Disputes/Pipeline nav, which until now showed for every role)
	// would leak every other client's and other TPAs' cases to them.
	if hasRole(r.Context().Value(ctxPrincipal{}).(principal), "TPA") {
		http.Error(w, `{"error":"forbidden: TPA accounts see their own disputes on the TPA page"}`, http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	limit := 50
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 500)
	}
	// Sort: default opened_at DESC uses keyset pagination; any explicit column
	// sort switches to offset mode (keyset over arbitrary columns isn't stable).
	// sort=sla is the deadline-driven order (Lever 4): closest to statutory
	// breach first = oldest opened_at ASC.
	sortCol, sortDir := "c.opened_at", "DESC"
	if s := q.Get("sort"); s != "" {
		// Status sorts by what the list shows: a programmed case's Internal
		// Status (its basic status stays PENDING_INTAKE), else the status.
		whitelist := map[string]string{"opened_at": "c.opened_at", "case_number": "c.case_number",
			"status": "coalesce(c.internal_status, c.status)", "qpa_cents": "c.qpa_cents", "service_line": caseServiceSQL,
			"assigned":              "NULLIF(NULLIF(c.assigned_to,''),'unassigned-pool')",
			"disputed_amount_cents": "c.disputed_amount_cents", "sla": "c.opened_at"}
		col, dir := s, "ASC"
		if strings.HasPrefix(s, "-") {
			col, dir = strings.TrimPrefix(s, "-"), "DESC"
		}
		if col == "sla" {
			dir = "ASC" // sla sort ignores direction: breach proximity only
		}
		if c, ok := whitelist[col]; ok {
			sortCol, sortDir = c, dir
		}
	}
	offsetMode := sortCol != "c.opened_at" || sortDir != "DESC"
	offset := 0
	if offsetMode {
		if n, err := strconv.Atoi(q.Get("offset")); err == nil && n >= 0 {
			offset = n
		}
	}
	tbl := sanitizeTenant(tenant)
	// One FROM clause for count + select: the escalations join feeds the
	// triage-lane expression (any escalation history -> COMPLEX). $1 = tenant.
	// public.escalations.case_id is text (every other write site inserts a
	// Go string, e.g. casemgmt.go's escalateCase), while cases.id is uuid on
	// every tenant schema -- confirmed live, "operator does not exist: text
	// = uuid" on literally every listCases call before this cast was added.
	fromClause := fmt.Sprintf(`FROM tenant_%s.cases c
		LEFT JOIN (SELECT DISTINCT case_id FROM public.escalations WHERE tenant=$1) esc
		  ON esc.case_id = c.id::text`, tbl)
	where, args := "", []any{tenant}
	add := func(clause string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND "+clause, len(args))
	}
	if st := q.Get("status"); st != "" {
		add("c.status = $%d", st)
	}
	// Pipeline columns (pipelineCounts): one stage of the board, paged like any other list.
	if st := q.Get("internal_status"); st != "" {
		add("c.internal_status = $%d", st)
	}
	switch q.Get("unset") { // the board's "Not yet reviewed" column
	case "internal_status":
		where += " AND coalesce(c.internal_status,'') = ''"
	case "status":
		where += " AND coalesce(c.status,'') = ''"
	}
	// Search (the Disputes page's search box): case number, service line, statuses, payer, anything in the case's
	// details (provider org, requester, health plan, contacts, notes, CPT codes...), and the filing and responding
	// parties by name, email or NPI. LIKE wildcards in the term are taken literally.
	if qs := strings.TrimSpace(q.Get("q")); qs != "" {
		if len(qs) > 100 {
			qs = qs[:100]
		}
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(qs) + "%"
		args = append(args, like)
		n := len(args)
		where += caseSearchSQL(n)
	}
	// mine=true: same convention as listTasks (crm.go) -- assigned_to is the
	// Keycloak sub assignCase/the auto-assign queue already writes, so this
	// is a real, populated filter, not a new concept.
	caller := r.Context().Value(ctxPrincipal{}).(principal)
	if q.Get("mine") == "true" {
		add("c.assigned_to = $%d", caller.Subject)
	}
	// A Reviewer sees only unassigned cases and their own (casescope.go).
	if cond, ok := caseScopeSQL(caller, "c", len(args)+1); ok {
		args = append(args, caller.Subject)
		where += cond
	}
	// Assignment filters: assigned=mine|unassigned|assigned, and (for those who see every case) assignee=<user id>.
	switch q.Get("assigned") {
	case "mine":
		add("c.assigned_to = $%d", caller.Subject)
	case "unassigned":
		where += " AND (c.assigned_to IS NULL OR c.assigned_to = 'unassigned-pool')"
	case "assigned":
		where += " AND c.assigned_to IS NOT NULL AND c.assigned_to <> 'unassigned-pool'"
	}
	if who := strings.TrimSpace(q.Get("assignee")); who != "" && !reviewerScoped(caller) {
		add("c.assigned_to = $%d", who)
	}
	// Triage lane filter (Lever 2): whitelisted, never interpolated raw.
	if lane := q.Get("lane"); lane == "AUTO_REVIEW" || lane == "STANDARD" || lane == "COMPLEX" {
		where += fmt.Sprintf(" AND %s = '%s'", triageLaneSQL, lane)
	}
	if cur := q.Get("cursor"); cur != "" && !offsetMode {
		parts := strings.SplitN(cur, "|", 2)
		if len(parts) == 2 {
			if ts, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil {
				args = append(args, ts, parts[1])
				where += fmt.Sprintf(" AND (c.opened_at, c.id) < ($%d, $%d::uuid)", len(args)-1, len(args))
			}
		}
	}
	var total int
	if err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT count(*) %s WHERE true%s`, fromClause, where), args...).Scan(&total); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	query := fmt.Sprintf(`SELECT c.id, c.case_number, c.status, c.internal_status, c.agency_status,
		                    `+caseServiceSQL+`, coalesce(c.qpa_cents,0), coalesce(c.disputed_amount_cents,0), c.opened_at,
		                    c.batch_id::text, coalesce(c.payer_id,''), %s AS triage_lane, %s AS sla_days_remaining,
		                    NULLIF(NULLIF(c.assigned_to,''),'unassigned-pool'), c.assigned_role
		             %s WHERE true%s
		             ORDER BY %s %s NULLS LAST, c.id DESC LIMIT %d`, triageLaneSQL, slaDaysSQL, fromClause, where, sortCol, sortDir, limit+1)
	if offsetMode {
		query += fmt.Sprintf(" OFFSET %d", offset)
	}
	rows, err := s.db.Query(r.Context(), query, args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []Case{}
	for rows.Next() {
		var c Case
		var batchID *string
		if err := rows.Scan(&c.ID, &c.CaseNumber, &c.Status, &c.InternalStatus, &c.AgencyStatus, &c.ServiceLine, &c.QPA, &c.DisputedAmount, &c.OpenedAt, &batchID, &c.PayerID, &c.TriageLane, &c.SLADaysRemaining, &c.AssignedTo, &c.AssignedRole); err != nil {
			slog.Error("listCases scan failed — row silently dropped from the list", "tenant", tenant, "err", err)
			continue
		}
		if batchID != nil && *batchID != "" {
			c.BatchID = batchID
		}
		c.Tenant = tenant
		out = append(out, c)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		if offsetMode {
			next = fmt.Sprintf("offset:%d", offset+limit)
		} else {
			last := out[len(out)-1]
			next = last.OpenedAt.UTC().Format(time.RFC3339Nano) + "|" + last.ID
		}
	}
	// The assignee's name on every row (one lookup per distinct person on the page).
	var subs []string
	for _, c := range out {
		if c.AssignedTo != nil && !contains(subs, *c.AssignedTo) {
			subs = append(subs, *c.AssignedTo)
		}
	}
	if len(subs) > 0 {
		names := s.resolveSubjectNames(r.Context(), subs)
		for i := range out {
			if out[i].AssignedTo != nil {
				out[i].AssignedName = names[*out[i].AssignedTo]
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": out, "next_cursor": next, "total": total})
}

func (s *server) getCase(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	s.maybeAdvanceStatus(r, tenant, id) // status follows platform facts
	var c Case
	var details, programDates []byte
	var internal, agency *string
	err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT id, case_number, status, coalesce(nullif(service_line,''), nullif(details->>'type_of_service',''),
		                    (SELECT cl.type_of_service FROM public.case_claims cl WHERE cl.case_id=cases.id::text AND coalesce(cl.type_of_service,'') <> ''
		                     GROUP BY 1 ORDER BY count(*) DESC, 1 LIMIT 1), ''), coalesce(qpa_cents,0), coalesce(disputed_amount_cents,0), opened_at,
		                    internal_status, agency_status, details, COALESCE(program_dates,'{}'::jsonb)
		             FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), id).
		Scan(&c.ID, &c.CaseNumber, &c.Status, &c.ServiceLine, &c.QPA, &c.DisputedAmount, &c.OpenedAt, &internal, &agency, &details, &programDates)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	c.Tenant = tenant
	var dj map[string]any
	_ = json.Unmarshal(details, &dj)
	var pd map[string]any
	_ = json.Unmarshal(programDates, &pd)
	if p, ok := r.Context().Value(ctxPrincipal{}).(principal); ok && isExternalOnly(p) {
		dj, internal = filingDetails(dj), nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": c.ID, "case_number": c.CaseNumber, "tenant": c.Tenant, "status": c.Status,
		"service_line": c.ServiceLine, "qpa_cents": c.QPA, "disputed_amount_cents": c.DisputedAmount, "opened_at": c.OpenedAt,
		"internal_status": internal, "agency_status": agency, "details": dj, "program_dates": pd,
		// "" | AWAITING_PAYMENT | INELIGIBLE -- the portal freezes everything
		// but correspondence on a locked case (caselock.go).
		"lock": s.caseLock(r.Context(), c.Tenant, c.ID),
	})
}

// signalCase forwards business signals into the running workflow.
// signalRoleFloors: signalCase forwards WHATEVER signal name the caller
// sends straight to the Temporal workflow (federal NSA's IdrCaseWorkflow or
// FL AHCA's AhcaDisputeWorkflow, whichever wfID resolves to) -- there was no
// server-side differentiation at all, despite the portal's own UI only ever
// showing e.g. "Finalize IDRE selection" (SELECTION_FINALIZED) to
// CASE_MANAGER/FEDERAL_ADMIN and "Issue determination" (DETERMINATION_ISSUED)
// to ARBITRATOR. Mirrors each signal's UI-implied caller, with admins always
// allowed. AHCA entries match the equivalent REST endpoint's own gate where
// one exists (PLAN_OPT_OUT -> recordOptOut's ATTORNEY-only floor;
// ELIGIBILITY_RESULT -> checkEligibility's floor) rather than inventing a
// separate rule for the same decision reaching the workflow two ways.
var signalRoleFloors = map[string][]string{
	"RESPONSE_FILED":       {"PARTY", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"OFFER_SUBMITTED":      {"PARTY", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"FEES_PAID":            {"PARTY", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"SELECTION_FINALIZED":  {"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"DETERMINATION_ISSUED": {"ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"PAYMENT_RECORDED":     {"FINANCE", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"SETTLED_OR_WITHDRAWN": {"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"PLAN_OPT_OUT":         {"ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	// Sent by case-api itself when a letter really goes out; a person
	// sending it by hand would walk a case past its QA gate.
	"LETTER_SENT":        {"FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"ELIGIBILITY_RESULT": {"CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	// Each SOP step belongs to one role (workflowsteps.go); admins stand in.
	"CODING_REVIEW_COMPLETE":   {"CODER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"CLINICAL_REVIEW_COMPLETE": {"NURSE_PHYSICIAN", "DOCTOR", "NURSE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"ATTORNEY_REVIEW_COMPLETE": {"ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"PACKET_COMPLETE":          {"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"AOR_REVISED":              {"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"ESTIMATE_PERMISSION":      {"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"PLAN_RESPONSE_RECEIVED":   {"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"FINAL_ORDER_ISSUED":       {"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"PROVIDER_WITHDRAW":        {"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"OUTREACH_DONE":            {"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	"RFI_RESPONSE_RECEIVED":    {"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	// A refund is a person's decision, never the clock's (the workflow only flags a potential refund).
	"REFUND_APPROVED": {"CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	// Capitol Bridge verifies a withdrawal request before the Withdrawal letter goes out.
	"WITHDRAWAL_CONFIRMED": {"CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
	// Whether the filer asked for a cost estimate: the step after the case is deemed eligible.
	"ESTIMATE_CONFIRMED": {"CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
}

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
	// Unmapped signals (PACKET_COMPLETE, OUTREACH_DONE, PROVIDER_WITHDRAW,
	// AOR_REVISED, ESTIMATE_PERMISSION, PLAN_RESPONSE_RECEIVED, RFI_SENT,
	// RFI_RESPONSE_RECEIVED, FINAL_ORDER_ISSUED) default to the broad
	// case-staff floor -- excludes PARTY/unauthenticated misuse without
	// requiring every one of these general case-tracking signals to be
	// individually enumerated above.
	if s.deliverCaseSignal(w, r, tenant, id, body.Signal, body.Data, "") {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "signaled"})
	}
}

// deliverCaseSignal is the one path a workflow signal takes into a case,
// from the case's Steps panel and from a specialty review queue alike: the
// signal's role floor, the step-order guard, the Temporal signal, the side
// effects, the activity record Steps reads its answers from, and the audit
// entry. A review-complete signal also closes that discipline's open queue
// item (reviewNote, when given, is the reviewer's completion note), and a
// coder asking for clinical review opens a clinical one. It writes the error
// response and returns false when it refuses.
func (s *server) deliverCaseSignal(w http.ResponseWriter, r *http.Request, tenant, id, signal string, data map[string]any, reviewNote string) bool {
	defer s.settleStepTasks(r.Context(), tenant, id) // a recorded step may settle its task (step_tasks.go)
	gate := signalRoleFloors[signal]
	if gate == nil {
		gate = caseStaffRoles
	}
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, append(append([]string{}, gate...), serviceRole)...) {
		http.Error(w, fmt.Sprintf(`{"error":"forbidden: signal %q is not permitted for your role"}`, signal), http.StatusForbidden)
		return false
	}
	var caseNumber, status string
	if err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT case_number, status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), id).Scan(&caseNumber, &status); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return false
	}
	// Only the case's next step may be signalled, and only by its owner.
	if !s.stepGuard(w, r, tenant, id, "signal:"+signal) {
		return false
	}
	// Workflow IDs are never persisted to the cases table (confirmed: no
	// INSERT or UPDATE anywhere writes it) -- initiateCase/the AHCA intake
	// endpoint both already compute it deterministically from tenant+case
	// number, so recompute it the same way here rather than trust a column
	// that was always empty. Reading a program row (not the much heavier
	// loadProgram-dependent callers elsewhere) is enough to tell which
	// workflow this case belongs to.
	wfID := s.caseWorkflowID(r, tenant, caseNumber)
	// The day the step actually happened (its dialog's date, today when none): checked here and passed on to the
	// workflow as given. The readable record of the dialog's answers is kept on the case, not in the workflow.
	stepDay, bad := stepDayOf(data, s.programToday(r.Context(), tenant))
	if bad != "" {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, bad), http.StatusBadRequest)
		return false
	}
	record, _ := data["record"].(string)
	delete(data, "record")
	// AHCA's Final Order is its own document: it is uploaded with the step that records it, never made here.
	if signal == "FINAL_ORDER_ISSUED" {
		docID, _ := data["doc_id"].(string)
		var fn string
		if docID == "" || s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT filename FROM tenant_%s.documents WHERE id::text=$1 AND case_id::text=$2`,
			sanitizeTenant(tenant)), docID, id).Scan(&fn) != nil {
			http.Error(w, `{"error":"upload AHCA's Final Order (the PDF the Agency issued) to record it"}`, http.StatusBadRequest)
			return false
		}
		_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`UPDATE tenant_%s.documents SET folder='DETERMINATION',
			filename = CASE WHEN filename ILIKE 'Final Order%%' THEN filename ELSE 'Final Order ' || $3 || ' — ' || filename END
			WHERE id::text=$1 AND case_id::text=$2`, sanitizeTenant(tenant)), docID, id, caseNumber)
	}
	// Close and refund is for a paid filing whose packet never completed; once the packet is in, the case goes on.
	if signal == "REFUND_APPROVED" {
		var packetIn bool
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce(program_dates->>'packet_complete_at','') <> '' FROM tenant_%s.cases WHERE id=$1`,
			sanitizeTenant(tenant)), id).Scan(&packetIn)
		if packetIn {
			http.Error(w, `{"error":"the filing packet is complete, so this case goes on to its review — it can't be closed for a refund"}`, http.StatusConflict)
			return false
		}
	}
	// Statutory offer window (45 CFR 149.520(b)(2)): sealed offers are only
	// accepted while the 10-business-day window is open — no late offers.
	if signal == "OFFER_SUBMITTED" && status != "OFFER_WINDOW_OPEN" {
		s.logActivity(r.Context(), tenant, id, "LATE_OFFER_REJECTED",
			fmt.Sprintf("Offer rejected: case status is %s, offer window is not open", status))
		http.Error(w, `{"error":"offer window is not open — late offers are not accepted"}`, http.StatusConflict)
		return false
	}
	// An open case whose workflow has ended gets it back (or, once its stages are done, the step is recorded without
	// it): workflow_alive.go. Only a closed case is refused.
	if _, err := s.signalCaseWorkflow(r.Context(), tenant, id, wfID, signal, data); err != nil {
		if workflowEnded(err) {
			forgetWorkflowRunning(wfID)
			writeJSON(w, http.StatusConflict, map[string]any{"error": endedWorkflowMsg, "workflow_ended": true})
			return false
		}
		http.Error(w, `{"error":"signal failed"}`, http.StatusBadGateway)
		return false
	}
	// The Reviewer tells the Coder and the Attorney the packet is in: the
	// Coder's Dispute Summary is next, and the Attorney may be needed on the AOR.
	if signal == "PACKET_COMPLETE" {
		for _, role := range []string{"CODER", "ATTORNEY"} {
			s.notify(r, tenant, "role:"+role, "PACKET_COMPLETE",
				fmt.Sprintf("Case %s: the filing packet is complete and has been checked by the Reviewer", caseNumber), "#/cases/"+id)
		}
	}
	// The filing packet's claim count is recorded with the step that
	// confirms the packet (report column E).
	if signal == "PACKET_COMPLETE" {
		if n := toInt64(data["claims_submitted"]); n > 0 {
			_, _ = s.db.Exec(r.Context(), fmt.Sprintf(
				`UPDATE tenant_%s.cases SET num_claims=$2, details = details || jsonb_build_object('claims_submitted', $2::int) WHERE id=$1`,
				sanitizeTenant(tenant)), id, n)
		}
	}
	if signal == "RFI_RESPONSE_RECEIVED" {
		s.setProgramDateOn(r.Context(), tenant, id, "rfi_response_received_at", stepDay)
	}
	if signal == "ESTIMATE_PERMISSION" { // recorded by the Reviewer: the portal request is answered
		s.closePermissionRequests(r.Context(), tenant, id, "COMPLETE")
	}
	// The answer is the case's too: the acceptance letter and email drop the estimate language without one.
	if signal == "ESTIMATE_CONFIRMED" {
		req, _ := data["requested"].(bool)
		_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`UPDATE tenant_%s.cases SET details = coalesce(details,'{}'::jsonb) ||
			jsonb_build_object('fee_estimate_requested', $2::boolean) WHERE id::text=$1`, sanitizeTenant(tenant)), id, req)
	}
	if signal == "WITHDRAWAL_CONFIRMED" {
		s.markProgramDateOn(r.Context(), tenant, id, "withdrawal_confirmed_at", stepDay)
	}
	// The outreach to a filer whose packet didn't come starts the refund window.
	if signal == "OUTREACH_DONE" {
		s.markProgramDateOn(r.Context(), tenant, id, "outreach_at", stepDay)
	}
	// When the withdrawal was asked for, and by whom (the provider or the health plan).
	if signal == "PROVIDER_WITHDRAW" {
		s.setProgramDateOn(r.Context(), tenant, id, "withdrawal_requested_at", stepDay)
		if by, _ := data["by"].(string); by == "provider" || by == "health_plan" {
			s.setCaseDetail(r.Context(), tenant, id, "withdrawal_requested_by", by)
		}
	}
	// AHCA's own number for its Final Order, when the order has one.
	if signal == "FINAL_ORDER_ISSUED" {
		if n, _ := data["order_number"].(string); strings.TrimSpace(n) != "" {
			s.setCaseDetail(r.Context(), tenant, id, "final_order_number", truncate(strings.TrimSpace(n), 100))
		}
	}
	detail, _ := json.Marshal(data)
	s.logActivity(r.Context(), tenant, id, signal,
		fmt.Sprintf("Workflow signal %s delivered to %s — %s", signal, wfID, truncate(string(detail), 500)))
	s.graphSync(tenant, id) // graph reflects status transitions (best-effort)
	s.logAudit(r.Context(), tenant, id, "CASE_SIGNALED", map[string]any{
		"by": r.Context().Value(ctxPrincipal{}).(principal).Subject, "signal": signal, "workflow_id": wfID,
	})
	// What the step's dialog recorded, kept on the case as a note; a review's findings go on its review record.
	if record = strings.TrimSpace(record); record != "" && reviewDisciplineOf(signal) == "" {
		s.addCaseNote(r.Context(), tenant, id, r.Context().Value(ctxPrincipal{}).(principal).Subject, truncate(record, 4000))
	}
	if reviewNote == "" {
		n, _ := data["note"].(string)
		reviewNote = truncate(strings.TrimSpace(n), 4000)
	}
	if disc := reviewDisciplineOf(signal); disc != "" {
		// A review done without being claimed first started when it was done.
		s.markProgramDate(r.Context(), tenant, id, reviewStartedKey[disc])
		s.markProgramDate(r.Context(), tenant, id, reviewDoneKey[disc])
		p := r.Context().Value(ctxPrincipal{}).(principal)
		s.closeOpenReview(r, tenant, id, disc, displayName(p), reviewNote)
		if disc == "CODING" && data["clinical_requested"] == true {
			s.openReview(r, tenant, id, caseNumber, "CLINICAL",
				"Clinical review asked for by the coder ("+displayName(p)+")"+map[bool]string{true: ": " + truncate(reviewNote, 300), false: "."}[reviewNote != ""], displayName(p))
		}
	}
	s.ensureStepHelper(r, tenant, id) // a specialist's step next: the next one in turn (helpers.go)
	return true
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
	acctSvcReceivable    uint32 = 7000 // service-fee invoices issued, awaiting payment (AR)
	acctSvcRevenue       uint32 = 7001 // IDRE professional-services revenue
	acctOperatingCash    uint32 = 7002 // invoice payments received (operating, not escrow)
	acctAccountsPayable  uint32 = 7003 // open obligations (awards, vendor, refunds)
	acctProgramExpense   uint32 = 7004 // expense recognition when a payable is recorded
	ledgerCodeIDRE       uint16 = 700  // platform code; ledger id = tenantLedgerID(tenant)
)

func tenantLedgerID(tenant string) uint32 {
	// Deterministic FIPS-style map: 'tx' -> 48, etc. (kept in public.state_config.tb_ledger_id).
	// Fallback: stable hash into [100..199].
	h := sha256.Sum256([]byte(tenant))
	return 100 + uint32(h[0])%100
}

func acctID(tenant string, code uint32, party string) tb_types.Uint128 {
	h := sha256.Sum256([]byte(ledgerKey(fmt.Sprintf("%s:%d:%s", tenant, code, party))))
	var b [16]byte
	copy(b[:], h[:16])
	return tb_types.BytesToUint128(b)
}

// hex128 renders a TigerBeetle Uint128 ID as hex (Bytes() returns an
// unaddressable array — copy first).
func hex128(v tb_types.Uint128) string {
	b := v.Bytes()
	return hex.EncodeToString(b[:])
}

// u128lo returns the low 64 bits (balances are well within uint64).
func u128lo(v tb_types.Uint128) uint64 {
	b := v.Bytes()
	return binary.LittleEndian.Uint64(b[0:8])
}

func hash16(parts ...string) tb_types.Uint128 {
	h := sha256.Sum256([]byte(ledgerKey(strings.Join(parts, ":"))))
	var b [16]byte
	copy(b[:], h[:16])
	return tb_types.BytesToUint128(b)
}

// transferIDs returns (id, pendingID) for a fee-transfer request. id is this
// transfer's own TigerBeetle ID; pendingID is what to set on the Transfer's
// PendingID field (zero for a PENDING transfer, which doesn't reference one).
//
// The original code here used ONE id, hashed without postKind, for all three
// postKind values, and never set PendingID. A POST or VOID call therefore
// tried to create a second transfer under the SAME id as the original
// pending one, with PendingID left at zero -- TigerBeetle requires PendingID
// on a post/void transfer (zero -> immediate TransferPendingIDMustNotBeZero)
// and separately rejects a second transfer at an id that already exists
// under different flags (TransferExistsWithDifferentFlags, confirmed live
// on the sibling deployment of this same endpoint on the main branch) --
// every two-phase transfer was permanently stuck pending. Fix: a PENDING
// transfer's id IS the reference other calls hash back to (unchanged);
// POST/VOID hash postKind in too for their own distinct id and set
// PendingID to the pending transfer's id so TigerBeetle can find it.
func transferIDs(caseID, kind, partyID string, amount uint64, postKind string) (id, pendingID tb_types.Uint128) {
	pendingID = hash16(caseID, kind, partyID, fmt.Sprint(amount))
	if postKind == "POST" || postKind == "VOID" {
		return hash16(caseID, kind, partyID, fmt.Sprint(amount), postKind), pendingID
	}
	return pendingID, tb_types.Uint128{}
}

func (s *server) postFeeTransfer(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	// RBAC floor: requirePerm (below) is the real fine-grained gate, but it
	// unconditionally allows everyone when Permify isn't deployed (permify.go:
	// "disabled (allow) in dev when unset"). This role check is the floor
	// that holds even with Permify absent -- confirmed on this same endpoint
	// in the sibling deployment (main branch) that an ARBITRATOR reached
	// TigerBeetle and posted a transfer with no role check at all in front of it.
	// CASE_MANAGER removed (access-control audit, AC-5 separation of
	// duties): the same role that decides a dispute's eligibility/outcome
	// shouldn't also be able to post the ledger transfer that moves its
	// money -- matches the floor settleInvoice/clearCheck already use.
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	// ReBAC: object-level permit on this tenant's ledger (fail-closed).
	if !s.requirePerm(w, r, "ledger", tenant, "transact") {
		return
	}
	var req FeeTransfer
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Amount == 0 {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	var debit, credit ledgerAcct
	switch req.Kind {
	case "ADMIN_FEE": // party pays admin fee into remittance account via escrow
		debit, credit = ledgerAcct{acctEscrowTrustHeld, req.PartyID}, ledgerAcct{acctAdminRemittance, ""}
	case "IDRE_FEE_RESERVE":
		debit, credit = ledgerAcct{acctEscrowTrustHeld, req.PartyID}, ledgerAcct{acctIdreCompensation, ""}
	case "REFUND":
		debit, credit = ledgerAcct{acctRefundPayable, ""}, ledgerAcct{acctEscrowTrustHeld, req.PartyID}
	default: // SETTLEMENT
		debit, credit = ledgerAcct{acctEscrowTrustHeld, req.PartyID}, ledgerAcct{acctRefundPayable, ""}
	}
	id, transferPendingID := transferIDs(req.CaseID, req.Kind, req.PartyID, req.Amount, req.PostKind)
	var flags uint16
	switch req.PostKind {
	case "PENDING":
		flags = tb_types.TransferFlags{Pending: true}.ToUint16()
	case "POST":
		flags = tb_types.TransferFlags{PostPendingTransfer: true}.ToUint16()
	case "VOID":
		flags = tb_types.TransferFlags{VoidPendingTransfer: true}.ToUint16()
	}
	res, err := s.postLedger(tenant, debit, credit, tb_types.Transfer{
		ID:        id,
		Amount:    tb_types.ToUint128(req.Amount),
		PendingID: transferPendingID,
		Flags:     flags,
	})
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
	s.logAudit(r.Context(), tenant, req.CaseID, "FEE_TRANSFER_POSTED", map[string]any{
		"by": p.Subject, "kind": req.Kind, "party": req.PartyID, "amount_cents": req.Amount, "post_kind": req.PostKind,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "recorded"})
}

// ---------------------------------------------------------------------------
// Dapr pub/sub publish (Kafka-backed component "idre-pubsub")
// ---------------------------------------------------------------------------

// publish announces an event. Callers do not wait on the outcome, so a
// failure is retried here and logged; an upload whose announcement is still
// lost after that is found and re-announced by sweepUnreadDocuments.
func (s *server) publish(ctx context.Context, tenant, domain string, event map[string]any) {
	if err := s.publishChecked(ctx, tenant, domain, event); err != nil {
		slog.Error("event not accepted by the message bus", "tenant", tenant, "domain", domain,
			"type", event["type"], "doc_id", event["doc_id"], "err", err)
	}
}

// publishOnce makes one attempt. A response the bus did not accept (it was
// silently ignored before: only a transport error was ever noticed) is an error.
func (s *server) publishOnce(ctx context.Context, tenant, domain string, event map[string]any) error {
	topic := fmt.Sprintf("%s.%s.%s", s.cfg.TopicPrefix, tenant, domain)
	if tp := traceparentOf(ctx); tp != "" {
		event["traceparent"] = tp
	}
	url := fmt.Sprintf("%s/v1.0/publish/idre-pubsub/%s", s.cfg.DaprHTTP, topic)
	body, _ := json.Marshal(event)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("message bus answered %d for %s: %s", resp.StatusCode, topic, strings.TrimSpace(string(msg)))
	}
	return nil
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
	// A single net.Conn Read() is not guaranteed to return the whole body
	// (plain io.Reader semantics, not io.ReadAll). Confirmed on the sibling
	// deployment of this same endpoint: a genuinely, correctly HMAC-signed
	// ~300KB webhook (a realistic call transcript) got silently truncated
	// here, hashed as a partial body, and rejected as "bad signature" -- a
	// small test payload passed, which is exactly how this went unnoticed.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"body too large or unreadable"}`, http.StatusRequestEntityTooLarge)
		return
	}
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

// isUniqueViolation reports whether err is Postgres error code 23505
// (unique_violation) -- a client-error condition (the row already exists),
// distinct from an actual server fault.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
