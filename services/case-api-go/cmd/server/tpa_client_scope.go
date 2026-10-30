// tpa_client_scope.go — a TPA's logins scoped to one of its clients, and its Finance role.
//
// public.tpa_users.client_id is the scope, read from the verified login and never from the request: NULL covers the
// whole TPA, set means that one client only. team_role FINANCE limits a login to payment operations, for the whole
// TPA or for its one client.
//
// The boundary is deny by default. accessGuard sends every request of a restricted login through tpaScopeGuard: a
// route not named in tpaScopedRoutes is refused, so a route added later stays closed to these logins until someone
// decides otherwise. A route naming one resource is checked against the login's client; a list is narrowed by its own
// handler through tpaClientFilter. Logins with no client and no FINANCE role are not touched by any of this.
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// tpaLogin is how a login relates to its TPA.
type tpaLogin struct {
	TPAID, ClientID, Role string
}

func (l tpaLogin) restricted() bool { return l.ClientID != "" || l.Role == "FINANCE" }

// tpaScopedRoute is one route open to restricted logins.
type tpaScopedRoute struct {
	method  string
	re      *regexp.Regexp
	check   string // what the id in the path must belong to the login's client: client, case, package, request, sentdoc, method, group, run; "" = the handler narrows it itself
	finance bool   // open to a FINANCE login
	client  bool   // open to a login scoped to one client
}

func tpaRoute(method, pattern, check string, finance, client bool) tpaScopedRoute {
	return tpaScopedRoute{method, regexp.MustCompile(pattern), check, finance, client}
}

const anyID = `([^/]+)`

var tpaScopedRoutes = []tpaScopedRoute{
	// Lists and pages: each handler narrows to the client itself (tpaClientFilter, payer.clientID).
	tpaRoute("GET", `^/tpa/me$`, "", true, true),
	tpaRoute("GET", `^/tpa/clients$`, "", true, true),
	tpaRoute("GET", `^/tpa/onboarding$`, "", true, true),
	tpaRoute("GET", `^/tpa/dashboard$`, "", true, true),
	tpaRoute("GET", `^/tpa/groups$`, "", true, true),
	tpaRoute("GET", `^/tpa/finance$`, "", true, true),
	tpaRoute("GET", `^/tpa/fees$`, "", true, true),
	tpaRoute("GET", `^/tpa/audit$`, "", true, true),
	tpaRoute("GET", `^/tpa/sent-documents$`, "", true, true),
	tpaRoute("GET", `^/tpa/documents\.zip$`, "", true, true),
	tpaRoute("GET", `^/tpa/document-requests$`, "", false, true),
	tpaRoute("GET", `^/calendar$`, "", true, true),
	// Paying.
	tpaRoute("POST", `^/tpa/fees/checkout$`, "", true, true),
	tpaRoute("POST", `^/tpa/fees/options$`, "", true, true),
	tpaRoute("GET", `^/tpa/fees/runs/`+anyID+`$`, "run", true, true),
	tpaRoute("POST", `^/tpa/fees/checkout/`+anyID+`/(confirm|cancel)$`, "group", true, true),
	tpaRoute("POST", `^/tpa/clients/`+anyID+`/fees/checkout$`, "client", true, true),
	tpaRoute("GET", `^/tpa/clients/`+anyID+`/payments$`, "client", true, true),
	tpaRoute("POST", `^/tpa/clients/`+anyID+`/payments$`, "client", true, true),
	tpaRoute("GET", `^/tpa/payment-methods$`, "", true, true),
	tpaRoute("POST", `^/tpa/payment-methods/setup$`, "", true, true),
	tpaRoute("POST", `^/tpa/payment-methods/setup/`+anyID+`/confirm$`, "", true, true),
	tpaRoute("DELETE", `^/tpa/payment-methods/`+anyID+`$`, "method", true, true),
	tpaRoute("POST", `^/tpa/disputes/`+anyID+`/pay$`, "case", true, true),
	tpaRoute("GET", `^/tpa/disputes/`+anyID+`/fee$`, "case", true, true),
	// Bulk filings' payment, for a whole-TPA Finance login (a batch can span clients).
	tpaRoute("GET", `^/tpa/disputes/bulk$`, "", true, false),
	tpaRoute("GET", `^/tpa/disputes/bulk/`+anyID+`$`, "", true, false),
	tpaRoute("POST", `^/tpa/disputes/bulk/`+anyID+`/checkout$`, "", true, false),
	// Reading one dispute.
	tpaRoute("GET", `^/tpa/disputes/`+anyID+`/summary$`, "case", true, true),
	tpaRoute("GET", `^/tpa/sent-documents/`+anyID+`$`, "sentdoc", true, true),
	tpaRoute("GET", `^/cases/`+anyID+`(?:/clocks|/documents|/documents/[0-9a-fA-F-]{36}/download)?$`, "case", true, true),
	// Filing and working one client's disputes: not for Finance.
	tpaRoute("POST", `^/tpa/intake$`, "", false, true), // the handler checks the client it names
	tpaRoute("GET", `^/tpa/prefill$`, "", false, true), // the handler checks the package it names
	tpaRoute("PATCH", `^/tpa/disputes/`+anyID+`$`, "case", false, true),
	tpaRoute("POST", `^/tpa/disputes/`+anyID+`/documents$`, "case", false, true),
	tpaRoute("GET", `^/tpa/disputes/`+anyID+`/reply-code$`, "case", false, true),
	tpaRoute("PUT", `^/tpa/clients/`+anyID+`/profile$`, "client", false, true),
	tpaRoute("POST", `^/tpa/clients/`+anyID+`/packages$`, "client", false, true),
	tpaRoute("GET", `^/tpa/packages/`+anyID+`$`, "package", false, true),
	tpaRoute("POST", `^/tpa/packages/`+anyID+`/docs$`, "package", false, true),
	tpaRoute("DELETE", `^/tpa/packages/`+anyID+`/docs/[^/]+$`, "package", false, true),
	tpaRoute("POST", `^/tpa/packages/`+anyID+`/merged$`, "package", false, true),
	tpaRoute("POST", `^/tpa/document-requests/`+anyID+`/(files|submit)$`, "request", false, true),
	// A client's own team: the handlers keep a client admin inside its client.
	tpaRoute("GET", `^/tpa/team$`, "", false, true),
	tpaRoute("POST", `^/tpa/team$`, "", false, true),
	tpaRoute("PATCH", `^/tpa/team/[^/]+$`, "", false, true),
	tpaRoute("DELETE", `^/tpa/team/[^/]+$`, "", false, true),
}

// tpaScopedDecision is the pure core: may this restricted login use this route, and what must the id in the path
// belong to? Pure, so every rule is tested without a database.
func tpaScopedDecision(l tpaLogin, method, rel string) (allowed bool, check, id string) {
	for _, re := range outsideShellPaths {
		if re.MatchString(rel) {
			return true, "", ""
		}
	}
	for _, rt := range tpaScopedRoutes {
		if rt.method != method {
			continue
		}
		m := rt.re.FindStringSubmatch(rel)
		if m == nil {
			continue
		}
		if (l.Role == "FINANCE" && !rt.finance) || (l.ClientID != "" && !rt.client) {
			return false, "", ""
		}
		if rt.check == "" || l.ClientID == "" { // a whole-TPA login: the handler's own TPA check is the boundary
			return true, "", ""
		}
		return true, rt.check, m[1]
	}
	return false, "", ""
}

// tpaLoginOf reads the login's relation to its TPA. found=false: not linked to one.
func (s *server) tpaLoginOf(ctx context.Context, tenant, sub string) (l tpaLogin, found bool, err error) {
	err = s.db.QueryRow(ctx, `SELECT tpa_id::text, coalesce(client_id::text,''), team_role FROM public.tpa_users WHERE tenant=$1 AND user_sub=$2`,
		tenant, sub).Scan(&l.TPAID, &l.ClientID, &l.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, false, nil
	}
	return l, err == nil, err
}

// tpaResourceInClient: the resource named in a path belongs to the login's client.
func (s *server) tpaResourceInClient(ctx context.Context, tenant, clientID, check, id string) bool {
	if check == "client" {
		return id == clientID
	}
	inClient := `(SELECT o.case_id FROM public.case_origin o WHERE o.tenant=$1 AND o.client_id::text=$2)`
	q := map[string]string{
		"case":    `SELECT EXISTS(SELECT 1 FROM public.case_origin o WHERE o.tenant=$1 AND o.client_id::text=$2 AND o.case_id=$3)`,
		"package": `SELECT EXISTS(SELECT 1 FROM public.tpa_packages k WHERE k.tenant=$1 AND k.client_id::text=$2 AND k.id::text=$3)`,
		"method":  `SELECT EXISTS(SELECT 1 FROM public.tpa_payment_methods m WHERE m.tenant=$1 AND m.client_id::text=$2 AND m.id::text=$3)`,
		"request": `SELECT EXISTS(SELECT 1 FROM public.document_requests q WHERE q.tenant=$1 AND q.id::text=$3 AND q.case_id IN ` + inClient + `)`,
		"sentdoc": `SELECT EXISTS(SELECT 1 FROM public.party_documents d WHERE d.tenant=$1 AND d.doc_id::text=$3 AND d.case_id IN ` + inClient + `)`,
		// A payment belongs to the client only if every invoice it covers does.
		"group": `SELECT EXISTS(SELECT 1 FROM public.payment_groups g WHERE g.tenant=$1 AND g.id::text=$3)
			AND NOT EXISTS(SELECT 1 FROM public.payment_groups g JOIN public.invoices i ON i.id::text = ANY(g.invoice_ids::text[])
			               WHERE g.tenant=$1 AND g.id::text=$3 AND i.case_id NOT IN ` + inClient + `)`,
		"run": `SELECT EXISTS(SELECT 1 FROM public.payment_groups g WHERE g.tenant=$1 AND g.run_id::text=$3)
			AND NOT EXISTS(SELECT 1 FROM public.payment_groups g JOIN public.invoices i ON i.id::text = ANY(g.invoice_ids::text[])
			               WHERE g.tenant=$1 AND g.run_id::text=$3 AND i.case_id NOT IN ` + inClient + `)`,
	}[check]
	if q == "" {
		return false // an unknown check never opens anything
	}
	var ok bool
	if err := s.db.QueryRow(ctx, q, tenant, clientID, id).Scan(&ok); err != nil {
		return false
	}
	return ok
}

// tpaScopeGuard applies the rules to a TPA login. false: the response is written. An error reading the login refuses
// rather than letting it through unscoped.
func (s *server) tpaScopeGuard(w http.ResponseWriter, r *http.Request, p principal, tenant, rel string) bool {
	l, found, err := s.tpaLoginOf(r.Context(), tenant, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"your access could not be checked — try again"}`, http.StatusServiceUnavailable)
		return false
	}
	if !found || !l.restricted() {
		return true
	}
	allowed, check, id := tpaScopedDecision(l, r.Method, rel)
	reason := "this login does not have that part of the TPA's account"
	status, body := http.StatusForbidden, fmt.Sprintf(`{"error":%q}`, "forbidden: "+reason)
	if allowed && check != "" && !s.tpaResourceInClient(r.Context(), tenant, l.ClientID, check, id) {
		allowed = false
		reason = "belongs to another client"
		// Not found, not forbidden: a client's login must not learn which ids exist.
		status, body = http.StatusNotFound, `{"error":"not found"}`
	}
	if allowed {
		return true
	}
	s.logAudit(r.Context(), tenant, "", "ACCESS_DENIED", map[string]any{"org": "tpa:" + l.TPAID, "by": p.Subject,
		"method": r.Method, "path": rel, "reason": reason, "client_id": l.ClientID, "team_role": l.Role})
	http.Error(w, body, status)
	return false
}

// tpaClientFilter is the client a list is narrowed to: a scoped login's own, whatever it asks for; otherwise the
// ?client_id= the caller chose, once it is known to be a client of this TPA. ok=false: not one of this TPA's clients.
func (s *server) tpaClientFilter(r *http.Request, tenant string, org *tpaOrg) (string, bool) {
	if org.ClientScope != "" {
		return org.ClientScope, true
	}
	return s.tpaClientParam(r, tenant, org.ID)
}

func (s *server) tpaClientParam(r *http.Request, tenant, tpaID string) (string, bool) {
	q := strings.TrimSpace(r.URL.Query().Get("client_id"))
	if q == "" {
		return "", true
	}
	var id string
	if s.db.QueryRow(r.Context(), `SELECT id::text FROM public.tpa_clients WHERE tenant=$1 AND tpa_id::text=$2 AND id::text=$3`,
		tenant, tpaID, q).Scan(&id) != nil {
		return "", false
	}
	return id, true
}

// ---- the Finance view ----

// tpaFinance: GET /tpa/finance[?client_id=][&format=csv] -- what the TPA's disputes owe and have paid, for its admins
// and Finance logins, narrowed to one client by tpaClientFilter.
func (s *server) tpaFinance(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
		return
	}
	if org.TeamRole != "ADMIN" && org.TeamRole != "FINANCE" && !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: finance is for your organization's admins and Finance logins"}`, http.StatusForbidden)
		return
	}
	clientID, ok := s.tpaClientFilter(r, tenant, org)
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	where, args := "", []any{tenant, org.ID}
	if clientID != "" {
		args = append(args, clientID)
		where = " AND o.client_id::text=$3"
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT i.id::text AS invoice_id, coalesce(i.invoice_no,'') AS invoice_no, i.kind, i.amount_cents, i.status,
		       coalesce(i.due_date::text,'') AS due_date, coalesce(i.paid_at::text,'') AS paid_at,
		       coalesce(i.remittance_ref,'') AS remittance_ref, i.created_at::date::text AS issued,
		       o.case_id, coalesce(c.case_number,'') AS case_number, coalesce(c.agency_status,'') AS agency_status,
		       o.client_id::text AS client_id, coalesce(cl.party_name,'') AS client_name,
		       coalesce((SELECT py.status FROM public.payments py WHERE py.tenant=i.tenant AND py.invoice_id=i.id ORDER BY py.created_at DESC LIMIT 1),'') AS payment_status,
		       coalesce((SELECT py.provider FROM public.payments py WHERE py.tenant=i.tenant AND py.invoice_id=i.id ORDER BY py.created_at DESC LIMIT 1),'') AS payment_provider,
		       -- Money on its way: a saved-method charge running. A checkout someone opened and left is not.
		       EXISTS(SELECT 1 FROM public.payment_groups g WHERE g.tenant=i.tenant AND g.status IN ('PROCESSING','REVIEW') AND i.id = ANY(g.invoice_ids)) AS clearing
		FROM public.invoices i
		JOIN public.case_origin o ON o.tenant=i.tenant AND o.case_id=i.case_id
		JOIN public.tpa_clients cl ON cl.id=o.client_id
		JOIN tenant_%s.cases c ON c.id::text=i.case_id
		WHERE i.tenant=$1 AND o.tpa_id::text=$2 AND (i.kind='INITIAL_FEE' OR i.party='PROVIDER')%s
		ORDER BY i.created_at DESC LIMIT 5000`, sanitizeTenant(tenant), where), args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		s.logAudit(r.Context(), tenant, "", "TPA_FINANCE_EXPORT", map[string]any{"org": "tpa:" + org.ID, "by": p.Subject, "client_id": clientID, "rows": len(rows)})
		tpaFinanceCSV(w, rows)
		return
	}
	writeJSON(w, http.StatusOK, tpaFinanceSummary(rows, time.Now().Format("2006-01-02")))
}

// tpaFinanceSummary rolls the invoices up: the totals, and each client's share.
func tpaFinanceSummary(rows []map[string]any, today string) map[string]any {
	type share struct {
		ClientID    string `json:"client_id"`
		Name        string `json:"name"`
		Outstanding int64  `json:"outstanding_cents"`
		Overdue     int64  `json:"overdue_cents"`
		Paid        int64  `json:"paid_cents"`
		Invoices    int    `json:"invoices"`
	}
	var outstanding, overdue, paid int64
	var nOpen, nOverdue, nPaid, nPending, nFailed int
	byClient := map[string]*share{}
	order := []string{}
	for _, x := range rows {
		id := fmt.Sprint(x["client_id"])
		sh := byClient[id]
		if sh == nil {
			sh = &share{ClientID: id, Name: fmt.Sprint(x["client_name"])}
			byClient[id] = sh
			order = append(order, id)
		}
		sh.Invoices++
		cents := toInt64(x["amount_cents"])
		switch fmt.Sprint(x["status"]) {
		case "OPEN":
			nOpen++
			outstanding += cents
			sh.Outstanding += cents
			if due := fmt.Sprint(x["due_date"]); due != "" && due < today {
				nOverdue++
				overdue += cents
				sh.Overdue += cents
			}
			if x["clearing"] == true {
				nPending++
			}
			if x["payment_status"] == "FAILED" {
				nFailed++
			}
		case "PAID":
			nPaid++
			paid += cents
			sh.Paid += cents
		}
	}
	clients := make([]*share, 0, len(order))
	for _, id := range order {
		clients = append(clients, byClient[id])
	}
	return map[string]any{"outstanding_cents": outstanding, "overdue_cents": overdue, "paid_cents": paid,
		"open": nOpen, "overdue": nOverdue, "paid": nPaid, "pending": nPending, "failed": nFailed,
		"clients": clients, "invoices": rows}
}

// csvNoFormula keeps a spreadsheet from running a cell as a formula (a client's name is the TPA's own free text).
func csvNoFormula(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func tpaFinanceCSV(w http.ResponseWriter, rows []map[string]any) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tpa-finance.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Client", "Case", "Invoice", "Kind", "Amount (USD)", "Status", "Payment", "Issued", "Due", "Paid", "Reference"})
	for _, x := range rows {
		cell := func(k string) string { return csvNoFormula(fmt.Sprint(x[k])) }
		payment := cell("payment_status")
		if payment == "PENDING" {
			payment = "Checkout opened, not paid"
		}
		if x["clearing"] == true {
			payment = "Clearing"
		}
		_ = cw.Write([]string{cell("client_name"), cell("case_number"), cell("invoice_no"), cell("kind"),
			strconv.FormatFloat(float64(toInt64(x["amount_cents"]))/100, 'f', 2, 64), cell("status"), payment,
			cell("issued"), cell("due_date"), cell("paid_at"), cell("remittance_ref")})
	}
	cw.Flush()
}
