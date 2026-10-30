// Author: Patrick Munis at newwave
// casemgmt.go — premier case-management layer: assignment/routing, escalations,
// relationships & batching, stage checklists, deadline calendar, notifications,
// duplicate detection, saved views, document generation, email-to-case.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// caseStaffRoles: the broad gate for case-specific staff actions (letter
// generation) that the portal's own UI already restricts to case-management
// roles -- as opposed to tighter gates for money (FINANCE/PM) or
// management-only actions (CASE_MANAGER/PM).
var caseStaffRoles = []string{"CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}

// crmStaffRoles: CRM (accounts/contacts/tasks/notes/leads) and case-relating
// actions -- the portal's nav shows Accounts/Leads/Tasks to every
// authenticated role with no restriction, so this is every real staff role
// (ARBITRATOR and FINANCE included, unlike caseStaffRoles) minus PARTY,
// which has no portal login to lose access to today but shouldn't get
// internal CRM write access if that ever changes.
var crmStaffRoles = []string{"CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "ARBITRATOR", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}

// financialReadRoles: checks/invoices/payments/financial-report reads --
// these were reachable by ANY authenticated tenant member via direct URL
// (only the Financials nav link hid them from the wrong roles, the backend
// enforced nothing), which meant a narrow specialist role with no financial
// remit at all -- ARBITRATOR, CODER, NURSE_PHYSICIAN, ATTORNEY -- could read
// check images' bank routing/account numbers just by typing the address.
// Matches the portal's own Financials nav gate exactly. CASE_MANAGER
// removed per the access-control follow-up: Financials is a standalone
// dashboard, not case-detail info, so reviewer loses it entirely rather
// than just the write actions the original audit restricted.
var financialReadRoles = []string{"FINANCE", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole}

// qaReadRoles: the QA review queue/detail (correspondence drafts awaiting
// approval) -- same gap as financialReadRoles, same fix. Matches the
// portal's own QA Gate nav gate.
var qaReadRoles = []string{"CASE_MANAGER", "ARBITRATOR", "PM", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}

// qaAgingHours: a PENDING draft older than this is stuck, not queued.
// 24h is deliberately shorter than the doc-findings 48h — a QA draft sits
// ON a statutory path (its case's clock is why it's in the queue at all),
// while a document finding is informational until reviewed.
const qaAgingHours = 24

// qaSendOverdueHours: an APPROVED email draft unsent this long after review
// is a statutory miss in the making — the reviewer did their half and the
// drafter never did theirs. Same 24h: the case clock doesn't pause between
// approval and send.
const qaSendOverdueHours = 24

// reportsReadRoles: the Reports page's summary/SLA endpoints. Matches the
// portal's own Reports nav gate (notably FINANCE and ARBITRATOR are NOT
// here, unlike financialReadRoles -- Reports nav never included them).
// CASE_MANAGER removed per the same access-control follow-up as
// financialReadRoles: reviewer loses Reports entirely, not just a subset.
// TENANT_ADMIN added back explicitly: it used to ride in on CASE_MANAGER's
// hasRole quirk, and removing CASE_MANAGER from this list silently took
// TENANT_ADMIN down with it -- confirmed live, not intended. Reviewer and
// tenant admin are different roles with different scopes going forward;
// every "CASE_MANAGER removed" change from the reviewer-only-reviews work
// gets this same explicit TENANT_ADMIN restoration.
var reportsReadRoles = []string{"PM", "FEDERAL_ADMIN", "STATE_AUDITOR", "PLATFORM_ADMIN", "TENANT_ADMIN", serviceRole}

// ---- Notifications -----------------------------------------------------------

func (s *server) notify(r *http.Request, tenant, userSub, typ, body, link string) {
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.notifications (tenant, user_sub, type, body, link)
		VALUES ($1,$2,$3,$4,$5)`, tenant, userSub, typ, body, link)
}

// logActivity appends to the unified CRM/case timeline used by caseDetail,
// account 360 and voice. Every subsystem (intake, documents, signals, notes,
// email, voice) writes through here so there is ONE activity stream per case.
func (s *server) logActivity(ctx context.Context, tenant, caseID, typ, body string) {
	logEvent(ctx, "activity", tenant, caseID, typ, nil)
	_, _ = s.db.Exec(ctx, `
		INSERT INTO public.case_activities (tenant, case_id, type, body)
		VALUES ($1,$2,$3,$4)`, tenant, caseID, typ, truncate(body, 2000))
}

// logAudit appends a real, properly hash-chained entry to public.audit_log
// (hash = sha256(prev_hash + canonical-JSON payload), chained per tenant by
// id order -- the exact scheme scripts/verify-audit-chain.py already
// expects and recomputes nightly). The two existing producers
// (onboarding_activities.py) don't actually do this -- prev_hash is
// hardcoded "" and "hash" is a human label, not a real hash -- so every
// pre-existing row already fails that verifier. This is the first real
// implementation; used going forward for case-lifecycle actions, which
// previously wrote to case_activities/notifications only and never
// appeared in the audit log at all despite the UI calling it "every
// tenant action" -- confirmed live: a just-created intake had zero
// audit_log rows.
//
// Not race-safe under concurrent writers for the same tenant (reads the
// latest hash, then inserts -- two simultaneous calls could both read the
// same prev_hash and fork the chain). Acceptable here: audit writes are
// low-frequency compared to case-mutation traffic, and the existing
// producers have the same gap. A SELECT ... FOR UPDATE would close it if
// write volume ever makes that race likely in practice.
func (s *server) logAudit(ctx context.Context, tenant, caseID, action string, payload map[string]any) {
	logEvent(ctx, "audit", tenant, caseID, action, eventDetail(payload))
	body, err := json.Marshal(payload) // Go sorts map keys on marshal -- matches Python's sort_keys=True
	if err != nil {
		return
	}
	// One writer at a time per tenant: two entries written at once both read
	// the same last hash, and the chain forked. Every outside organization's
	// action is audited now (orgaudit.go), so "at once" is common.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		slog.Error("audit", "action", action, "err", err)
		return
	}
	defer tx.Rollback(ctx)
	_, _ = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('audit_log:' || $1))`, tenant)
	var prevHash string
	if err := tx.QueryRow(ctx,
		`SELECT hash FROM public.audit_log WHERE tenant=$1 ORDER BY id DESC LIMIT 1`, tenant).
		Scan(&prevHash); err != nil {
		prevHash = "GENESIS"
	}
	sum := sha256.Sum256([]byte(prevHash + string(body)))
	hash := hex.EncodeToString(sum[:])
	if _, err := tx.Exec(ctx, `
		INSERT INTO public.audit_log (tenant, case_id, action, payload, prev_hash, hash, org, actor)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''))`, tenant, caseID, action, string(body), prevHash, hash, auditOrg(payload), auditActor(payload)); err != nil {
		slog.Error("audit", "action", action, "err", err)
		return
	}
	_ = tx.Commit(ctx)
}

// auditActor: the person an entry records ("by"), for their own trail
// (My activity); "" for the system, Stripe and other machine actors.
func auditActor(payload map[string]any) string {
	by, _ := payload["by"].(string)
	if by == "" || strings.HasPrefix(by, "system") || strings.HasPrefix(by, "stripe") {
		return ""
	}
	return by
}

// auditOrg: the outside organization an entry is about, so its own audit
// log (orgaudit.go) can show it -- "org" when the writer says so, else the
// tpa_id / plan_id the entry already names.
func auditOrg(payload map[string]any) string {
	if o, _ := payload["org"].(string); o != "" {
		return o
	}
	if id := fmt.Sprint(payload["tpa_id"]); payload["tpa_id"] != nil && id != "" {
		return "tpa:" + id
	}
	if id := fmt.Sprint(payload["plan_id"]); payload["plan_id"] != nil && id != "" {
		return "plan:" + id
	}
	return ""
}

// listAuditLog: GET /v1/tenants/{tenant}/audit-log — the hash-chained,
// tamper-evident platform audit trail (public.audit_log). Written to
// throughout the backend (NPI verification, onboarding decisions,
// escalations, intake sweeps) but had zero read access anywhere until now —
// confirmed live, not even FEDERAL_ADMIN could see it. tenancy()'s existing
// per-role scoping handles the rest: STATE_AUDITOR and FEDERAL_ADMIN/
// PLATFORM_ADMIN already get cross-tenant reads, everyone else only reaches
// their own tenant's rows.
func (s *server) listAuditLog(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	// The whole program's trail: admins and the state auditor. Everyone else
	// reads their own (GET /audit/mine) and, for a TPA or plan, its
	// organization's; a case's own history stays on the case page.
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", "TENANT_ADMIN") {
		http.Error(w, `{"error":"forbidden: the program audit log is for admins and the state auditor — your Audit log shows your own actions"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	q := r.URL.Query()
	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 500)
	}
	where, args := "tenant=$1", []any{tenant}
	// "filter by case id" read as a search box, but case_id here is always
	// the raw case UUID -- nobody searches by that, they search by the case
	// NUMBER they actually see everywhere else in the UI (e.g. FL26-012).
	// Typing a case number against an exact UUID match silently returned
	// nothing, which is almost certainly what "search doesn't work" was.
	// Resolve a non-UUID input as a case_number first.
	if caseID := strings.TrimSpace(q.Get("case_id")); caseID != "" {
		if _, err := uuid.Parse(caseID); err != nil {
			if cfg := s.loadProgram(r, tenant); cfg != nil {
				var resolved string
				_ = s.db.QueryRow(r.Context(),
					fmt.Sprintf(`SELECT id::text FROM tenant_%s.cases WHERE case_number=$1`, sanitizeTenant(tenant)),
					caseID).Scan(&resolved)
				caseID = resolved // "" if no match -- the WHERE clause below then correctly matches nothing
			}
		}
		args = append(args, caseID)
		where += fmt.Sprintf(" AND case_id=$%d", len(args))
	}
	// Exact match on action forced the user to know and type the literal
	// constant (e.g. "CASE_STATUS_CHANGED") -- partial, case-insensitive
	// match lets "status" or "escalat" actually find something.
	if action := strings.TrimSpace(q.Get("action")); action != "" {
		args = append(args, "%"+action+"%")
		where += fmt.Sprintf(" AND action ILIKE $%d", len(args))
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT id, case_id, action, payload, prev_hash, hash, created_at
		FROM public.audit_log WHERE %s ORDER BY created_at DESC LIMIT %d`, where, limit), args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// payload.by is a raw Keycloak subject on every logAudit call, by
	// design -- an audit trail should key on the stable subject, not a
	// mutable display name, so the write-time convention stays as-is.
	// Resolve it to a username for DISPLAY only, same as the Team/Time
	// fix (resolveSubjectNames), without touching what's actually stored.
	// pgx returns this jsonb column as raw JSON text (string/[]byte), not
	// a decoded map, via queryRows' generic .Values() path -- confirmed
	// live, so it needs its own json.Unmarshal here rather than a type
	// assertion straight to map[string]any.
	payloadBy := func(row map[string]any) string {
		var raw []byte
		switch v := row["payload"].(type) {
		case string:
			raw = []byte(v)
		case []byte:
			raw = v
		default:
			return ""
		}
		var payload map[string]any
		if json.Unmarshal(raw, &payload) != nil {
			return ""
		}
		by, _ := payload["by"].(string)
		return by
	}
	var subjects []string
	for _, row := range rows {
		if by := payloadBy(row); by != "" {
			subjects = append(subjects, by)
		}
	}
	names := s.resolveSubjectNames(r.Context(), subjects)
	for _, row := range rows {
		if by := payloadBy(row); by != "" {
			row["by_name"] = names[by]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows})
}

func (s *server) listNotifications(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	limit, offset := pageParams(r, 50, 500)
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT id, type, body, COALESCE(link,''), created_at
		FROM public.notifications
		WHERE tenant=$1 AND user_sub = ANY($2) AND read_at IS NULL
		ORDER BY created_at DESC, id DESC LIMIT %d OFFSET %d`, limit, offset), tenant, s.notifTargetsFor(r.Context(), tenant, p))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var typ, body, link string
		var at time.Time
		if rows.Scan(&id, &typ, &body, &link, &at) == nil {
			out = append(out, map[string]any{
				"id": id, "type": typ, "body": body, "link": link, "at": at,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// countNotifications: GET /notifications/count — the caller's unread total, for the bell's badge. The list is paged
// (50 at most per call), so counting its rows capped the badge at 50 and it never moved while more were unread.
func (s *server) countNotifications(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var n int
	if err := s.db.QueryRow(r.Context(), `
		SELECT count(*) FROM public.notifications
		WHERE tenant=$1 AND user_sub = ANY($2) AND read_at IS NULL`,
		tenant, s.notifTargetsFor(r.Context(), tenant, p)).Scan(&n); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"unread": n})
}

func (s *server) readNotification(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	// Scoped the same way listNotifications reads: own notifications plus
	// tenant-wide broadcasts (user_sub='*') -- previously any authenticated
	// tenant member could mark ANY other user's notification read by id.
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.notifications SET read_at=now()
		WHERE tenant=$1 AND id=$2 AND user_sub = ANY($3)`,
		tenant, chi.URLParam(r, "notifId"), s.notifTargetsFor(r.Context(), tenant, p))
	writeJSON(w, http.StatusOK, map[string]string{"status": "read"})
}

// readAllNotifications: POST /notifications/read-all — clears every unread
// notification visible to the caller (own + tenant-wide broadcasts), same
// scope as listNotifications.
func (s *server) readAllNotifications(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.notifications SET read_at=now()
		WHERE tenant=$1 AND user_sub = ANY($2) AND read_at IS NULL`,
		tenant, s.notifTargetsFor(r.Context(), tenant, p))
	writeJSON(w, http.StatusOK, map[string]string{"status": "read"})
}

// ---- Assignment & routing ------------------------------------------------------
// Workload-balanced: the case manager (or arbitrator) of this tenant with the
// fewest open assignments wins. Manual override via {"assignee": "<sub>"}.

// ---- Escalations (SLA breach -> supervisor chain) --------------------------------

// escalateCase: POST /cases/{caseId}/escalate {clock, detail} — also called by
// the workflow worker when a statutory breach fires (service token).
func (s *server) escalateCase(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	// RBAC floor (requirePerm below allows everyone when Permify is
	// undeployed). SERVICE_WORKER is the Temporal worker's own automated-
	// escalation call (WORKER_TOKEN auth in authn.middleware).
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	// ReBAC: escalation is a staff-only object-level permission.
	if !s.requirePerm(w, r, "dispute_case", caseID, "escalate") {
		return
	}
	var in struct {
		Clock  string `json:"clock"`
		Detail string `json:"detail"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Clock == "" {
		http.Error(w, `{"error":"clock required"}`, http.StatusBadRequest)
		return
	}
	// Programmed tenants (e.g. FL AHCA) route escalations to their own
	// configured role (PM, per program-rules.sql) instead of the federal
	// default -- previously hardcoded to 'FEDERAL_ADMIN' regardless of
	// tenant, so an AHCA PM would never see their own program's escalations.
	escalatedTo := "FEDERAL_ADMIN"
	if cfg := s.loadProgram(r, tenant); cfg != nil && cfg.Escalation.RouteRole != "" {
		escalatedTo = cfg.Escalation.RouteRole
	}
	// Human-readable case_number, not the raw UUID -- this lands verbatim in
	// the notification message and the calendar/tasks list, both read by
	// staff, not machines.
	var caseNumber string
	if err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&caseNumber); err != nil || caseNumber == "" {
		caseNumber = caseID
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.escalations (tenant, case_id, clock, escalated_to, detail)
		VALUES ($1,$2,$3,$4,$5)`, tenant, caseID, in.Clock, escalatedTo, in.Detail)
	s.notify(r, tenant, "*", "SLA_BREACH",
		fmt.Sprintf("SLA breach %s on case %s — escalated to %s", in.Clock, caseNumber, escalatedTo),
		"#/cases/"+caseID)
	// supervisor task so the escalation is owned, not just logged
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.tasks (tenant, subject, case_id, due_date, created_by)
		VALUES ($1,$2,$3, CURRENT_DATE + 2, 'system')`,
		tenant, fmt.Sprintf("Escalation: %s breach on %s", in.Clock, caseNumber), caseID)
	s.logAudit(r.Context(), tenant, caseID, "CASE_ESCALATED", map[string]any{
		"by": p.Subject, "clock": in.Clock, "escalated_to": escalatedTo, "detail": in.Detail,
	})
	writeJSON(w, http.StatusCreated, map[string]string{"status": "escalated"})
}

func (s *server) listEscalations(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	limit, offset := pageParams(r, 100, 500)
	out, _ := s.queryRows(r, fmt.Sprintf(`
		SELECT case_id, clock, level, escalated_to, detail, created_at
		FROM public.escalations WHERE tenant=$1 ORDER BY created_at DESC
		LIMIT %d OFFSET %d`, limit, offset), tenant)
	writeJSON(w, http.StatusOK, out)
}

// ---- Relationships & batching -----------------------------------------------------

func (s *server) relateCases(w http.ResponseWriter, r *http.Request) {
	// crmStaffRoles not caseStaffRoles: the GNN "confirm link" button
	// (views.js) renders with no role restriction at all on the case detail
	// page, so ARBITRATOR/FINANCE need to reach this same as CRM actions do.
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, crmStaffRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		CaseID    string `json:"case_id"`
		RelatedID string `json:"related_case_id"`
		RelType   string `json:"rel_type"` // BATCH | PARENT_CHILD | DUPLICATE
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(in.RelType != "BATCH" && in.RelType != "PARENT_CHILD" && in.RelType != "DUPLICATE") {
		http.Error(w, `{"error":"rel_type must be BATCH | PARENT_CHILD | DUPLICATE"}`, http.StatusBadRequest)
		return
	}
	for _, id := range []string{in.CaseID, in.RelatedID} {
		if lock := s.caseLockedForRequest(r, tenant, id); lock != "" {
			writeLockRefusal(w, lock)
			return
		}
	}
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.case_relationships (tenant, case_id, related_case_id, rel_type)
		VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		tenant, in.CaseID, in.RelatedID, in.RelType)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, in.CaseID, "CASES_RELATED", map[string]any{
		"by":              r.Context().Value(ctxPrincipal{}).(principal).Subject,
		"related_case_id": in.RelatedID, "rel_type": in.RelType,
	})
	writeJSON(w, http.StatusCreated, map[string]string{"status": "related"})
}

func (s *server) caseRelationships(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	out, _ := s.queryRows(r, `
		SELECT related_case_id, rel_type FROM public.case_relationships
		WHERE tenant=$1 AND case_id=$2
		UNION
		SELECT case_id, rel_type FROM public.case_relationships
		WHERE tenant=$1 AND related_case_id=$2`, tenant, id)
	writeJSON(w, http.StatusOK, out)
}

// ---- Stage checklists --------------------------------------------------------------

var stageChecklistDefaults = map[string][]string{
	"INTAKE":      {"claim docs uploaded", "parties verified", "plan type confirmed"},
	"ELIGIBILITY": {"federal vs SSL routing decided", "initiation window verified", "no active suspension"},
	"OFFERS":      {"both parties fees invoiced", "both sealed offers submitted", "IDRE selection finalized"},
	"DETERMINATION": {"offer amounts within evidence", "QPA credibility assessed", "all 12 notice elements present",
		"written rationale recorded", "no conflict of interest", "baseball rule: one offer selected",
		"IDRE fee split determined", "parties notified", "determination within 30bd",
		"payment terms stated", "reportable to CMS", "signature captured"},
	"PAYMENT": {"payment within 30cd confirmed", "escrow settled", "refunds processed if any"},
}

// ensureChecklist seeds the stage checklist for a case. Programmed tenants
// (e.g. FL AHCA) get their own checklist from program_rules.config.checklist
// -- the federal NSA items ("baseball rule", "reportable to CMS") don't apply
// to AHCA's claims-dispute process and would otherwise be seeded regardless.
func (s *server) ensureChecklist(r *http.Request, tenant, caseID string) {
	items := stageChecklistDefaults
	if cfg := s.loadProgram(r, tenant); cfg != nil && len(cfg.Checklist) > 0 {
		items = cfg.Checklist
	}
	for stage, its := range items {
		for _, item := range its {
			_, _ = s.db.Exec(r.Context(), `
				INSERT INTO public.case_checklists (tenant, case_id, stage, item)
				VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, tenant, caseID, stage, item)
		}
	}
}

// autoChecklist ticks checklist items whose truth is already in the
// database — case managers should only ever tick what the platform cannot
// know. done_by records the evidence ('auto: <reason>'), never a user.
// Idempotent: only OPEN items flip, and only when the predicate holds.
func (s *server) autoChecklist(r *http.Request, tenant, caseID string) {
	t := sanitizeTenant(tenant)
	rules := []struct {
		item string
		sql  string
		args []any
	}{
		{"claim docs uploaded",
			fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM tenant_%s.documents WHERE case_id=$1)`, t), []any{caseID}},
		{"both sealed offers submitted",
			fmt.Sprintf(`SELECT count(*) >= 2 FROM tenant_%s.sealed_offers WHERE case_id=$1`, t), []any{caseID}},
		{"parties notified",
			`SELECT EXISTS(SELECT 1 FROM public.qa_reviews WHERE tenant=$1 AND case_id=$2 AND status='SENT')`,
			[]any{tenant, caseID}},
		{"payment within 30cd confirmed",
			`SELECT EXISTS(SELECT 1 FROM public.invoices WHERE tenant=$1 AND case_id=$2 AND status='PAID')`,
			[]any{tenant, caseID}},
		{"both parties fees invoiced",
			`SELECT count(DISTINCT party) >= 2 FROM public.invoices WHERE tenant=$1 AND case_id=$2`,
			[]any{tenant, caseID}},
	}
	for _, rule := range rules {
		var ok bool
		if err := s.db.QueryRow(r.Context(), rule.sql, rule.args...).Scan(&ok); err != nil || !ok {
			continue
		}
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.case_checklists SET done=true, done_by=$4, done_at=now()
			WHERE tenant=$1 AND case_id=$2 AND item=$3 AND NOT done`,
			tenant, caseID, rule.item, "auto: evidence in platform records")
	}
}

// maybeAdvanceStatus moves a case forward when platform facts say so —
// the status column follows events, not manual edits. Forward-only along
// the canonical lifecycle; terminal/manual states (Ineligible, Dismissed,
// Withdrawn, Plan Opt-Out) are never touched, and Temporal's own
// transitions simply win the race when the workflow runs first.
func (s *server) maybeAdvanceStatus(r *http.Request, tenant, caseID string) {
	t := sanitizeTenant(tenant)
	// All case invoices PAID while awaiting payment => funds settled.
	res, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET status='CLOSED_PAID', updated_at=now()
		WHERE id::text=$1 AND status='PAYMENT_PENDING'
		  AND EXISTS(SELECT 1 FROM public.invoices WHERE tenant=$2 AND case_id=$1)
		  AND NOT EXISTS(SELECT 1 FROM public.invoices WHERE tenant=$2 AND case_id=$1 AND status='OPEN')`, t),
		caseID, tenant)
	if err == nil && res.RowsAffected() > 0 {
		s.recordTransition(r.Context(), tenant, caseID, axisLifecycle, "PAYMENT_PENDING", "CLOSED_PAID", "", "auto_advance")
	}
	// A generated determination letter while under review => determined.
	res2, err2 := s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET status='DETERMINED', updated_at=now()
		WHERE id=$1 AND status='IN_REVIEW'
		  AND EXISTS(SELECT 1 FROM tenant_%s.documents d
		             WHERE d.case_id=$1 AND d.object_key ILIKE '%%determination%%')`, t, t),
		caseID)
	if err2 == nil && res2.RowsAffected() > 0 {
		s.recordTransition(r.Context(), tenant, caseID, axisLifecycle, "IN_REVIEW", "DETERMINED", "", "auto_advance")
	}
}

func (s *server) getChecklist(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	s.autoChecklist(r, tenant, id)
	out, _ := s.queryRows(r, `
		SELECT id, stage, item, required, done, COALESCE(done_by,'') AS done_by, done_at
		FROM public.case_checklists WHERE tenant=$1 AND case_id=$2
		ORDER BY stage, id`, tenant, id)
	// Each item belongs to one role: say whose it is, and whether the
	// caller is that role (or an admin) so the page offers the tick only
	// to them.
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var owners map[string]string
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		owners = cfg.ChecklistOwners
	}
	for _, row := range out {
		role := owners[fmt.Sprint(row["item"])]
		row["owner_role"] = role
		row["owner"] = wfOwnerLabels[role]
		row["can_check"] = mayCheckItem(p, role)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) checkItem(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	// caseStaffRoles plus ARBITRATOR: the portal's own checklist UI
	// (views.js) has always shown the check button to ARBITRATOR -- a real,
	// actively-used federal NSA role -- so the gate matches what's already
	// exposed rather than silently 403ing a working interaction.
	if !hasAnyRole(p, "ARBITRATOR", "FINANCE") && !hasAnyRole(p, caseStaffRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	itemID := chi.URLParam(r, "itemId")
	// Role-based: an item with an owner in the program's checklist_owners
	// is ticked by that role (or an admin) and nobody else.
	if cfg := s.loadProgram(r, tenant); cfg != nil && len(cfg.ChecklistOwners) > 0 {
		var item string
		if err := s.db.QueryRow(r.Context(),
			`SELECT item FROM public.case_checklists WHERE tenant=$1 AND id=$2`, tenant, itemID).Scan(&item); err != nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if role := cfg.ChecklistOwners[item]; !mayCheckItem(p, role) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": fmt.Sprintf("forbidden: this checklist item is ticked by the %s", wfOwnerLabels[role])})
			return
		}
	}
	res, err := s.db.Exec(r.Context(), `
		UPDATE public.case_checklists SET done=true, done_by=$3, done_at=now()
		WHERE tenant=$1 AND id=$2`, tenant, itemID, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// Only audit when a real item was ticked; look up its case so the entry
	// is filterable per case.
	if res.RowsAffected() > 0 {
		var itemCaseID, stage, item string
		_ = s.db.QueryRow(r.Context(), `
			SELECT case_id, stage, item FROM public.case_checklists WHERE tenant=$1 AND id=$2`,
			tenant, itemID).Scan(&itemCaseID, &stage, &item)
		s.logAudit(r.Context(), tenant, itemCaseID, "CHECKLIST_ITEM_CHECKED", map[string]any{
			"by": p.Subject, "item_id": itemID, "stage": stage, "item": item,
		})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "checked"})
}

// ---- Deadline calendar (agenda feed) -------------------------------------------------

func (s *server) calendar(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	// A TPA sees only its own disputes' deadlines and its own tasks.
	if hasRole(r.Context().Value(ctxPrincipal{}).(principal), "TPA") {
		org, err := s.tpaForPrincipal(r, tenant)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
			return
		}
		// The deadlines on its own disputes and its own tasks (tpa_calendar.go).
		clientID, ok := s.tpaClientFilter(r, tenant, org)
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, s.tpaCalendarItems(r, tenant, org.ID, clientID))
		return
	}
	deadlines, _ := s.queryRows(r, fmt.Sprintf(`
		SELECT id::text AS ref, 'OFFER_WINDOW' AS kind, case_number AS label,
		       to_char(offer_window_ends_at,'YYYY-MM-DD"T"HH24:MI') AS due, id::text AS case_id
		FROM tenant_%s.cases
		WHERE offer_window_ends_at IS NOT NULL AND status='OFFER_WINDOW_OPEN'`, sanitizeTenant(tenant)))
	// case_id travels alongside the task id now so the calendar can link a
	// task straight to its case -- previously only OFFER_WINDOW rows linked
	// anywhere; every TASK row was plain, unclickable text.
	// Done tasks stay on the calendar, ticked, for a while (the system's own ones tick themselves: step_tasks.go).
	tasks, _ := s.queryRows(r, `
		SELECT id::text AS ref, 'TASK' AS kind, subject AS label,
		       to_char(due_date,'YYYY-MM-DD') AS due, coalesce(case_id,'') AS case_id,
		       status='DONE' AS done, to_char(completed_at,'YYYY-MM-DD"T"HH24:MI') AS done_at,
		       created_by IN ('system','system:deadlines') AS automatic
		FROM public.tasks WHERE tenant=$1 AND due_date IS NOT NULL
		  AND (status='OPEN' OR (status='DONE' AND completed_at > now() - interval '45 days'))`, tenant)
	writeJSON(w, http.StatusOK, append(deadlines, tasks...))
}

// ---- Duplicate detection (on initiation) ----------------------------------------------

func (s *server) findDuplicates(r *http.Request, tenant, providerID, payerID string, qpaCents int64) []string {
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT case_number FROM tenant_%s.cases
		WHERE provider_id=$1 AND payer_id=$2
		  AND ABS(qpa_cents - $3) < GREATEST(5000, $3/20)
		  AND opened_at > now() - interval '90 days'
		LIMIT 5`, sanitizeTenant(tenant)), providerID, payerID, qpaCents)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cn string
		if rows.Scan(&cn) == nil {
			out = append(out, cn)
		}
	}
	return out
}

// ---- Saved views ------------------------------------------------------------------------

func (s *server) listSavedViews(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	out, _ := s.queryRows(r, `
		SELECT id, object, name, filters, pinned FROM public.saved_views
		WHERE tenant=$1 AND user_sub=$2 ORDER BY pinned DESC, name`, tenant, p.Subject)
	writeJSON(w, http.StatusOK, out)
}

func (s *server) saveView(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		Object  string         `json:"object"`
		Name    string         `json:"name"`
		Filters map[string]any `json:"filters"`
		Pinned  bool           `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.Object == "" {
		http.Error(w, `{"error":"object and name required"}`, http.StatusBadRequest)
		return
	}
	filters, _ := json.Marshal(in.Filters)
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.saved_views (tenant, user_sub, object, name, filters, pinned)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (tenant, user_sub, object, name) DO UPDATE SET filters=$5, pinned=$6`,
		tenant, p.Subject, in.Object, in.Name, filters, in.Pinned)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "saved"})
}

// ---- Document generation (letters from templates) ----------------------------------------

var letterTemplates = map[string]string{
	"offer_window_notice": `NOTICE OF OFFER WINDOW — Federal IDR
Case: {{.CaseNumber}} ({{.Tenant}})
Service line: {{.ServiceLine}} | QPA: ${{printf "%.2f" .QPAUsd}}
The offer window opens under 45 CFR 149.510. Submit your sealed offer within
10 business days. Late or missing offers default per regulation.`,
	"determination_letter": `CERTIFIED IDRE DETERMINATION — Federal IDR
Case: {{.CaseNumber}} ({{.Tenant}})
Determination issued under 45 CFR 149.510(c)(4). The certified IDRE selected
the offer of: {{.WinningParty}}.
Rationale: {{.Rationale}}
Payment due within 30 calendar days per 45 CFR 149.510(c)(4)(vii).`,
}

func (s *server) generateLetter(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, caseStaffRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	name := chi.URLParam(r, "template")
	tmplSrc, ok := letterTemplates[name]
	if !ok {
		http.Error(w, `{"error":"unknown template"}`, http.StatusNotFound)
		return
	}
	var cn, sl string
	var qpa int64
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT case_number, COALESCE(service_line,''), COALESCE(qpa_cents,0)
		FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&cn, &sl, &qpa); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	tmpl, err := template.New("letter").Parse(tmplSrc)
	if err != nil {
		http.Error(w, `{"error":"template"}`, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	_ = tmpl.Execute(&buf, map[string]any{
		"CaseNumber": cn, "Tenant": strings.ToUpper(tenant), "ServiceLine": sl,
		"QPAUsd": float64(qpa) / 100, "WinningParty": r.URL.Query().Get("winning_party"),
		"Rationale": r.URL.Query().Get("rationale"),
	})
	// Store through the encrypted document pipeline (vault + MinIO + metadata).
	ct, err := s.vaultSealDoc(r, tenant, fmt.Sprintf("%s/cases/%s/gen-%s.txt", tenant, caseID, name), buf.Bytes())
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}
	key := fmt.Sprintf("%s/cases/%s/gen-%s.enc", tenant, caseID, name)
	_, err = s.docs.mc.PutObject(r.Context(), docBucket, key, bytes.NewReader(ct), int64(len(ct)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		http.Error(w, `{"error":"store failed"}`, http.StatusBadGateway)
		return
	}
	docID := newUUID()
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.documents (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version)
		VALUES ($1,$2,$3,$4,'text/plain',false,'system',1)`, sanitizeTenant(tenant)),
		docID, caseID, key, buf.Len())
	s.logAudit(r.Context(), tenant, caseID, "LETTER_GENERATED", map[string]any{
		"by": r.Context().Value(ctxPrincipal{}).(principal).Subject, "template": name,
		"doc_id": docID, "case_number": cn,
	})
	writeJSON(w, http.StatusCreated, map[string]string{"doc_id": docID, "object_key": key})
}

// ---- Email channel: inbound email -> lead/activity (email-to-case) ------------------------

// emailInbound receives provider webhooks (Mailgun/SES-style), authenticated by the shared EMAIL_INBOUND_TOKEN.
// Unset, the endpoint is closed. The mail is filed like every other inbound mail (mailfiling.go): under its dispute
// when it names one, otherwise in the mail log's inbound section, and as a lead.
func (s *server) emailInbound(w http.ResponseWriter, r *http.Request) {
	if s.cfg.EmailInboundToken == "" {
		http.Error(w, `{"error":"inbound email is not configured"}`, http.StatusServiceUnavailable)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Email-Token")), []byte(s.cfg.EmailInboundToken)) != 1 {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var in struct {
		Tenant    string `json:"tenant"`
		From      string `json:"from"`
		Subject   string `json:"subject"`
		Body      string `json:"body"`
		CaseRef   string `json:"case_number"` // optional: "Re: CMS-TX-2026-00001"
		MessageID string `json:"message_id"`
		InReplyTo string `json:"in_reply_to"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&in)
	tenant := strings.ToLower(in.Tenant)
	if len(tenant) != 2 {
		http.Error(w, `{"error":"tenant required"}`, http.StatusBadRequest)
		return
	}
	m := s.fileInbound(r.Context(), inboundMail{Tenant: tenant, From: in.From, Subject: in.Subject, Body: in.Body, MessageID: in.MessageID,
		Refs: messageIDsIn(in.InReplyTo), CaseHint: in.CaseRef, By: "email-inbound", NotifyUnmatched: true}, true)
	if m.CaseID != "" {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "attached", "case_id": m.CaseID, "case_number": m.CaseNumber, "reason": m.Reason})
		return
	}
	// otherwise: lead capture (email-to-lead)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.leads (tenant, source, name, email, summary)
		VALUES ($1,'EMAIL',$2,$3,$4)`, tenant, in.From, in.From,
		fmt.Sprintf("%s\n%s", in.Subject, truncate(in.Body, 400)))
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "lead_created", "reason": m.Reason})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// mayCheckItem: the item's owner role, or an admin. An item with no owner
// keeps the old rule (any case staff, already checked by the caller).
func mayCheckItem(p principal, ownerRole string) bool {
	if ownerRole == "" {
		return true
	}
	return hasAnyRole(p, ownerRole, "FEDERAL_ADMIN", "PLATFORM_ADMIN")
}
