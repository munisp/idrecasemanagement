// orgaudit.go — the audit trail of the organizations outside the office: a
// TPA (and the providers it files for) and a health plan.
//
// Everything an outside account does is written to the tamper-evident audit
// log (public.audit_log, hash-chained), and so is everything anyone does in a
// TPA's or a plan's area -- staff acting for it included. Each entry carries
// the organization ("tpa:<id>" / "plan:<id>", audit_log.org), so the
// organization reads its own trail on its dashboard: GET /tpa/audit and
// GET /hp/audit. Business events about it (a dispute filed by the bulk filer,
// a payment settled, a document decided by staff, a notice sent) carry the
// organization too (auditOrg, notifyOrg).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// orgAuditQuiet: what the portal asks for on a timer or to draw itself --
// nobody did anything.
var orgAuditQuiet = map[string]bool{
	"POST /presence/ping": true, "GET /notifications": true, "GET /program": true, "GET /prefs": true, "PUT /prefs": true,
}

// orgActionLabels say in words what an action was, for the organization's log.
var orgActionLabels = map[string]string{
	"GET /tpa/me": "Opened the TPA page", "GET /tpa/dashboard": "Viewed the disputes list", "GET /tpa/clients": "Viewed the clients",
	"GET /tpa/onboarding": "Viewed client onboarding", "GET /tpa/fees": "Viewed fees owed",
	"GET /tpa/disputes/bulk": "Viewed bulk filings", "GET /tpa/disputes/bulk/{batchId}": "Opened a bulk filing",
	"GET /tpa/packages/{packageId}": "Viewed a document package", "GET /tpa/clients/{clientId}/payments": "Viewed a client's payment methods",
	"GET /tpa/responding": "Viewed disputes against clients", "GET /tpa/responding/{caseId}": "Opened a dispute against a client",
	"GET /tpa/responding/{caseId}/documents/{docId}/file": "Opened a document of a dispute against a client",
	"POST /tpa/claim": "Linked the account to the TPA", "POST /tpa/unlink": "Unlinked the account from the TPA",
	"POST /tpa/invite-code": "Generated a teammate invite code", "POST /tpa/clients": "Added a client",
	"POST /tpa/clients/import": "Imported clients from CSV", "PUT /tpa/clients/{clientId}/profile": "Updated a client's profile",
	"POST /tpa/clients/{clientId}/payments": "Added a client payment method", "POST /tpa/clients/{clientId}/status": "Changed a client's status",
	"POST /tpa/clients/{clientId}/packages": "Started a document package", "POST /tpa/packages/{packageId}/docs": "Uploaded a package document",
	"POST /tpa/packages/{packageId}/merged": "Uploaded a merged package PDF", "POST /tpa/packages/bulk": "Uploaded packages in bulk",
	"POST /tpa/intake": "Filed a dispute", "PATCH /tpa/disputes/{caseId}": "Corrected a dispute",
	"POST /tpa/disputes/bulk": "Uploaded a bulk filing", "POST /tpa/disputes/bulk/{batchId}/checkout": "Started payment for a bulk filing",
	"POST /tpa/fees/checkout":                 "Started payment for all unpaid fees",
	"POST /tpa/responding/{caseId}/documents": "Uploaded a response document", "POST /tpa/responding/{caseId}/submit": "Submitted a response",
	"GET /hp/me": "Opened the health plan page", "GET /hp/onboarding": "Viewed party onboarding",
	"GET /hp/packages/{packageId}": "Viewed a document package", "GET /hp/parties/{partyId}/payments": "Viewed a party's payment methods",
	"GET /hp/responding": "Viewed disputes against the plan", "GET /hp/responding/{caseId}": "Opened a dispute against the plan",
	"GET /hp/responding/{caseId}/documents/{docId}/file": "Opened a document of a dispute against the plan",
	"POST /hp/claim": "Linked the account to the plan", "POST /hp/unlink": "Unlinked the account from the plan",
	"POST /hp/parties": "Added a party", "POST /hp/parties/import": "Imported parties from CSV",
	"PUT /hp/parties/{partyId}/profile": "Updated a party's profile", "POST /hp/parties/{partyId}/payments": "Added a party payment method",
	"POST /hp/parties/{partyId}/packages": "Started a document package", "POST /hp/packages/{packageId}/docs": "Uploaded a package document",
	"POST /hp/packages/{packageId}/merged": "Uploaded a merged package PDF", "POST /hp/packages/bulk": "Uploaded packages in bulk",
	"POST /hp/intake": "Filed a dispute", "POST /hp/responding/{caseId}/documents": "Uploaded a response document",
	"POST /hp/responding/{caseId}/submit": "Submitted a response", "POST /hp/responding/{caseId}/opt-out": "Asked to opt out of a dispute",
	"GET /cases/{caseId}": "Opened a filed dispute", "GET /cases/{caseId}/clocks": "Viewed a dispute's deadlines",
	"GET /cases/{caseId}/documents": "Viewed a dispute's documents", "GET /cases/{caseId}/documents/{docId}/download": "Downloaded a dispute document",
	"GET /calendar": "Viewed the calendar", "POST /intake/bulk": "Uploaded a batch filing", "GET /intake/bulk": "Viewed batch filings",
	"POST /tpa/disputes/{caseId}/pay": "Started payment for a dispute", "POST /hp/disputes/{caseId}/pay": "Started payment for a dispute",
	"POST /tpa/clients/{clientId}/fees/checkout": "Started payment for a provider's disputes", "POST /hp/fees/checkout": "Started payment for all unpaid fees",
	"GET /tpa/disputes/{caseId}/fee": "Viewed a dispute's fee", "GET /hp/disputes/{caseId}/fee": "Viewed a dispute's fee", "GET /hp/fees": "Viewed fees owed",
	"POST /tpa/fees/checkout/{groupId}/confirm": "Came back from a payment", "POST /hp/fees/checkout/{groupId}/confirm": "Came back from a payment",
	"POST /tpa/fees/checkout/{groupId}/cancel": "Cancelled a payment", "POST /hp/fees/checkout/{groupId}/cancel": "Cancelled a payment",
	"GET /tpa/document-requests": "Viewed document requests", "GET /hp/document-requests": "Viewed document requests",
	"POST /tpa/document-requests/{reqId}/files": "Uploaded a requested document", "POST /hp/document-requests/{reqId}/files": "Uploaded a requested document",
	"POST /tpa/document-requests/{reqId}/submit": "Sent the requested documents", "POST /hp/document-requests/{reqId}/submit": "Sent the requested documents",
	"GET /tpa/audit": "Viewed the audit log", "GET /hp/audit": "Viewed the audit log",
	"POST /admin/tpas/{tpaId}/status": "Office changed the TPA's status", "POST /admin/healthplans/{planId}/status": "Office changed the plan's status",
}

// orgEventLabels: the business events about an organization, in words.
var orgEventLabels = map[string]string{
	"TPA_REGISTERED": "TPA registered", "TPA_CLAIMED": "Account linked to the TPA", "TPA_UNLINKED": "Account unlinked",
	"TPA_INVITE_CODE_GENERATED": "Teammate invite code generated", "TPA_CLIENT_ADDED": "Client added", "TPA_STATUS": "TPA status changed",
	"HEALTH_PLAN_REGISTERED": "Health plan registered", "HEALTH_PLAN_CLAIMED": "Account linked to the plan", "HEALTH_PLAN_STATUS": "Plan status changed",
	"HP_PARTY_ADDED": "Party added", "HP_PARTY_PROFILE": "Party profile updated", "HP_PARTIES_IMPORTED": "Parties imported",
	"HP_PAYMENT_ADDED": "Party payment method added", "HP_PACKAGE_CREATED": "Document package started", "HP_BULK_PACKAGES": "Packages uploaded in bulk",
	"HP_INTAKE": "Dispute filed", "ONBOARDING_DOC_REVIEWED": "Office decided a document", "ONBOARDING_DOC_VIEWED": "Office opened a document",
	"INVOICE_SETTLED": "Fee paid", "BULK_DISPUTE_FILED": "Dispute filed from a bulk filing",
	"DISPUTE_AGAINST_YOU": "Notice: a dispute against you", "DISPUTE_AGAINST_CLIENT": "Notice: a dispute against a client",
	"RESPONDENT_RESPONDED": "Notice: the other side responded", "PLAN_OPT_OUT_REQUESTED": "Notice: opt-out requested",
	"PAYMENT_RECEIVED": "Notice: payment received", "PAYMENT_REFUNDED": "Notice: a fee paid twice was refunded",
	"DOCUMENTS_REQUESTED": "Office requested documents", "DOCUMENT_REQUEST_CANCELLED": "Office withdrew a document request",
	"DOCUMENT_REQUEST_COMPLETE": "Office marked a document request complete", "DOCUMENTS_SUBMITTED": "Requested documents sent",
	"FEE_CHECKOUT": "Payment page opened", "FEE_CHECKOUT_CANCELLED": "Payment cancelled", "DUPLICATE_PAYMENT_REFUNDED": "Fee paid twice — refunded",
	"TPA_FEE_CHECKOUT": "Payment page opened", "BATCH_FILED": "Notice: bulk filing filed", "PACKAGE_STATUS": "Package checked",
}

// orgAudit sits after the access guard: by then the caller, the tenant and the
// guard's decision are known, and after the handler the route and outcome are.
func (s *server) orgAudit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := r.Context().Value(ctxPrincipal{}).(principal)
		tenant, _ := r.Context().Value(ctxTenant{}).(string)
		rel := r.URL.Path
		if i := strings.Index(rel, "/v1/tenants/"+tenant); i >= 0 {
			rel = rel[i+len("/v1/tenants/"+tenant):]
		}
		outside := isExternalOnly(p)
		orgArea := strings.HasPrefix(rel, "/tpa/") || strings.HasPrefix(rel, "/hp/") || strings.HasPrefix(rel, "/admin/tpas/") ||
			strings.HasPrefix(rel, "/admin/healthplans/") || strings.HasPrefix(rel, "/intake/bulk")
		if tenant == "" || (!outside && !orgArea) {
			next.ServeHTTP(w, r)
			return
		}
		before := s.callerOrgs(r.Context(), tenant, p.Subject) // unlink removes the link the entry belongs to
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)

		route := ""
		var rctx *chi.Context
		if rctx = chi.RouteContext(r.Context()); rctx != nil {
			route = rctx.RoutePattern()
		}
		if route == "" || strings.HasSuffix(route, "/*") {
			route = withoutIDs(r.URL.Path)
		}
		action := actionName(r.Method, route)
		if orgAuditQuiet[action] {
			return
		}
		orgs := before
		if len(orgs) == 0 {
			orgs = s.callerOrgs(r.Context(), tenant, p.Subject) // claim creates it
		}
		org := ""
		switch {
		case strings.HasPrefix(rel, "/admin/tpas/") && rctx != nil:
			org = "tpa:" + rctx.URLParam("tpaId")
		case strings.HasPrefix(rel, "/admin/healthplans/") && rctx != nil:
			org = "plan:" + rctx.URLParam("planId")
		case strings.HasPrefix(rel, "/hp/"):
			org = orgs["plan"]
		case strings.HasPrefix(rel, "/tpa/"), strings.HasPrefix(rel, "/intake/bulk"):
			org = orgs["tpa"]
		case hasRole(p, "TPA"):
			org = orgs["tpa"]
		default:
			org = orgs["plan"]
		}
		if org == "" || strings.HasSuffix(org, ":") {
			return // not about an organization (e.g. a staff list of all TPAs)
		}
		caseID := ""
		if rctx != nil {
			caseID = rctx.URLParam("caseId")
		}
		label := orgActionLabels[action]
		if label == "" {
			label = action
		}
		payload := map[string]any{
			"org": org, "by": p.Subject, "by_name": displayName(p), "roles": roleList(p.Roles), "office_staff": !outside,
			"action": action, "label": label, "method": r.Method, "status": sw.code, "outcome": outcomeOf(sw.code), "ip": clientIP(r),
		}
		for _, k := range []string{"batchId", "packageId", "clientId", "partyId", "docId"} {
			if rctx != nil && rctx.URLParam(k) != "" {
				payload[k] = rctx.URLParam(k)
			}
		}
		// Written after the answer, off the request: the chain lock must not
		// hold up the caller.
		go s.logAudit(context.WithoutCancel(r.Context()), tenant, caseID, "PORTAL_ACTION", payload)
	})
}

// callerOrgs: the TPA and the plan the account is linked to (staff acting for one included).
func (s *server) callerOrgs(ctx context.Context, tenant, sub string) map[string]string {
	out := map[string]string{}
	var t, p string
	_ = s.db.QueryRow(ctx, `SELECT coalesce((SELECT tpa_id::text FROM public.tpa_users WHERE tenant=$1 AND user_sub=$2 LIMIT 1),''),
		coalesce((SELECT plan_id::text FROM public.health_plan_users WHERE tenant=$1 AND user_sub=$2 LIMIT 1),'')`, tenant, sub).Scan(&t, &p)
	if t != "" {
		out["tpa"] = "tpa:" + t
	}
	if p != "" {
		out["plan"] = "plan:" + p
	}
	return out
}

// orgAuditLog: GET /tpa/audit, /hp/audit -- the organization's own trail,
// newest first. ?views=1 includes page views (left out by default: opening a
// page is recorded, but a reader looks for what was done); ?q= searches.
func (s *server) orgAuditLog(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		var orgID string
		if kind == "tpa" {
			org, err := s.tpaForPrincipal(r, tenant)
			if err != nil {
				http.Error(w, `{"error":"not linked to a TPA"}`, http.StatusForbidden)
				return
			}
			orgID = org.ID
		} else {
			org, err := s.healthPlanForPrincipal(r, tenant)
			if err != nil {
				http.Error(w, `{"error":"not linked to a health plan"}`, http.StatusForbidden)
				return
			}
			orgID = org.ID
		}
		q := r.URL.Query()
		limit, offset := pageParams(r, 100, 500)
		prefix := "tpa"
		if kind == "hp" {
			prefix = "plan" // entries name the organization as "plan:<id>" (auditOrg, payer.org)
		}
		where, args := "a.tenant=$1 AND a.org=$2", []any{tenant, prefix + ":" + orgID}
		if q.Get("views") != "1" {
			// Reads that only drew a page are left out; opening a document stays in.
			where += ` AND NOT (a.action='PORTAL_ACTION' AND a.payload::jsonb->>'method'='GET'
			            AND a.payload::jsonb->>'action' NOT LIKE '%/file' AND a.payload::jsonb->>'action' NOT LIKE '%/download')`
		}
		if term := strings.TrimSpace(q.Get("q")); term != "" {
			args = append(args, "%"+term+"%")
			where += fmt.Sprintf(" AND (a.payload ILIKE $%d OR a.action ILIKE $%d OR c.case_number ILIKE $%d)", len(args), len(args), len(args))
		}
		rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
			SELECT a.id, a.created_at, a.action, a.payload, coalesce(a.case_id,''), coalesce(c.case_number,''), a.hash
			FROM public.audit_log a
			LEFT JOIN tenant_%s.cases c ON c.id::text = a.case_id
			WHERE %s ORDER BY a.id DESC LIMIT %d OFFSET %d`, sanitizeTenant(tenant), where, limit, offset), args...)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		type entry struct {
			ID         int64     `json:"id"`
			At         time.Time `json:"at"`
			Who        string    `json:"who"`
			Office     bool      `json:"office"`
			What       string    `json:"what"`
			Detail     string    `json:"detail,omitempty"`
			CaseID     string    `json:"case_id,omitempty"`
			CaseNumber string    `json:"case_number,omitempty"`
			Outcome    string    `json:"outcome"`
			IP         string    `json:"ip,omitempty"`
			Hash       string    `json:"hash"`
			by         string
		}
		out := []*entry{}
		subjects := []string{}
		for rows.Next() {
			var e entry
			var action, payload string
			if rows.Scan(&e.ID, &e.At, &action, &payload, &e.CaseID, &e.CaseNumber, &e.Hash) != nil {
				continue
			}
			var pl map[string]any
			_ = json.Unmarshal([]byte(payload), &pl)
			str := func(k string) string { v, _ := pl[k].(string); return v }
			e.Outcome, e.IP = "ok", str("ip")
			if len(e.Hash) > 12 {
				e.Hash = e.Hash[:12]
			}
			if action == "PORTAL_ACTION" {
				e.What, e.Who, e.Outcome = str("label"), str("by_name"), str("outcome")
				e.Office, _ = pl["office_staff"].(bool)
				if e.Outcome != "ok" {
					e.Detail = "refused (" + strconv.Itoa(int(toInt64(pl["status"]))) + ")"
				}
			} else {
				e.What = orgEventLabels[action]
				if e.What == "" {
					e.What = strings.ReplaceAll(strings.ToLower(action), "_", " ")
				}
				e.Detail = str("notice")
				if d := str("decision"); d != "" {
					e.Detail = strings.ToLower(d) + " — " + str("slot")
				}
				if st := str("status"); action == "PACKAGE_STATUS" && st != "" {
					e.Detail = str("party") + ": " + strings.ToLower(st)
				}
				switch by := str("by"); {
				case by == "" || strings.HasPrefix(by, "system") || strings.HasPrefix(by, "stripe"):
					e.Who = "system"
				default:
					e.by = by
					subjects = append(subjects, by)
				}
				e.Office = strings.HasPrefix(action, "ONBOARDING_DOC_") || strings.HasSuffix(action, "_STATUS") && action != "PACKAGE_STATUS"
			}
			out = append(out, &e)
		}
		names := s.resolveSubjectNames(r.Context(), subjects)
		for _, e := range out {
			if e.by != "" {
				e.Who = names[e.by]
				if e.Who == "" {
					e.Who = "a user"
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": out})
	}
}
