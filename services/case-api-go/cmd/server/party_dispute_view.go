package main

// party_dispute_view.go — a dispute as its TPA or health plan sees it in the portal.
//
// GET /tpa/disputes/{caseId}/summary and /hp/disputes/{caseId}/summary: the dispute's facts a party may know --
// who it is between, what is disputed, its claims, where it stands and its dates, the fees billed to that side, and
// the outcome once the Final Order is issued. Nothing the office adds while working the case (its summary, notes,
// eligibility working, fraud flags, the other side's fees) is in it. The letters and documents sent to the party are
// party_documents.go's; the deadlines are the case's clocks.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// partyDates: the program dates a party sees, in the order they happen, with their names.
var partyDates = []struct{ key, label string }{
	{"received_at", "Received"},
	{"packet_complete_at", "Filing packet complete"},
	{"date_case_accepted", "Accepted for review"},
	{"estimate_sent_at", "Cost estimate sent"},
	{"date_provider_responded", "Provider's permission received"},
	{"plan_notified_at", "Health plan notified"},
	{"date_health_plan_response_due", "Health plan response due"},
	{"date_plan_responded", "Health plan responded"},
	{"date_recommendation_sent_to_agency", "Determination sent to AHCA"},
	{"date_final_order_sent", "Final Order issued"},
	{"withdrawal_requested_at", "Withdrawal requested"},
}

// orgOnCase: the caller's organization filed the dispute or answers it.
func (s *server) orgOnCase(ctx context.Context, tenant string, py payer, caseID string) (side string, ok bool) {
	var filer, respondent bool
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM public.case_origin o WHERE o.tenant=$1 AND o.case_id=$2 AND o.%s::text=$3)`,
		py.originCol()), tenant, caseID, py.id).Scan(&filer)
	col := "tpa_id"
	if py.kind == "plan" {
		col = "plan_id"
	}
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM public.case_respondents r WHERE r.tenant=$1 AND r.case_id=$2 AND r.%s::text=$3)`,
		col), tenant, caseID, py.id).Scan(&respondent)
	var filerType string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT upper(coalesce(details->>'filing_party_type','PROVIDER')) FROM tenant_%s.cases WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&filerType)
	if filerType != "HEALTH_PLAN" {
		filerType = "PROVIDER"
	}
	other := map[string]string{"PROVIDER": "HEALTH_PLAN", "HEALTH_PLAN": "PROVIDER"}[filerType]
	switch {
	case filer:
		return filerType, true
	case respondent:
		return other, true
	}
	return "", false
}

func (s *server) orgDisputeSummary(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"not linked to an organization"}`, http.StatusNotFound)
			return
		}
		caseID := chi.URLParam(r, "caseId")
		side, ok := s.orgOnCase(r.Context(), tenant, py, caseID)
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound) // not ours: say nothing about it
			return
		}
		t := sanitizeTenant(tenant)
		rows, err := s.queryRows(r, fmt.Sprintf(`
			SELECT c.case_number, c.status, coalesce(c.agency_status,'') AS agency_status, c.opened_at,
			       c.disputed_amount_cents, coalesce(c.program_dates,'{}'::jsonb) AS dates,
			       coalesce(c.details->>'initiating_party','') AS initiating_party,
			       coalesce(c.details->>'health_plan', rs.name, '') AS health_plan,
			       coalesce(c.details->>'disputed_issue','') AS disputed_issue,
			       upper(coalesce(c.details->>'filing_party_type','PROVIDER')) AS filing_party_type,
			       coalesce(c.details->>'case_outcome','') AS case_outcome,
			       nullif(c.details->>'final_amount_awarded_cents','')::bigint AS awarded_cents,
			       coalesce(c.details->>'final_order_number','') AS final_order_number,
			       coalesce(cl0.party_name,'') AS client, coalesce(g.name,'') AS provider_group
			FROM tenant_%s.cases c
			LEFT JOIN public.case_origin o ON o.tenant=$1 AND o.case_id=c.id::text
			LEFT JOIN public.tpa_clients cl0 ON cl0.id=o.client_id
			LEFT JOIN public.tpa_provider_groups g ON g.id=cl0.group_id
			LEFT JOIN public.case_respondents rs ON rs.tenant=$1 AND rs.case_id=c.id::text
			WHERE c.id::text=$2`, t), tenant, caseID)
		if err != nil || len(rows) == 0 {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		c := rows[0]
		dates := []map[string]any{}
		if pd, ok := c["dates"].(map[string]any); ok {
			for _, d := range partyDates {
				if v, ok := pd[d.key].(string); ok && v != "" {
					dates = append(dates, map[string]any{"key": d.key, "label": d.label, "date": v})
				}
			}
		}
		delete(c, "dates")
		// The outcome is the Agency's: shown once its Final Order is issued.
		if dateOf(dates, "date_final_order_sent") == "" {
			c["case_outcome"], c["awarded_cents"], c["final_order_number"] = "", nil, ""
		}
		if py.kind != "tpa" {
			delete(c, "client")
			delete(c, "provider_group")
		}
		claims, _ := s.queryRows(r, `
			SELECT claim_number, coalesce(date_of_service::text,'') AS date_of_service, coalesce(type_of_service,'') AS type_of_service,
			       coalesce(cpt,'') AS cpt, billed_cents, paid_cents, disputed_cents, coalesce(status,'') AS status
			FROM public.case_claims WHERE tenant=$1 AND case_id=$2 ORDER BY claim_number LIMIT 500`, tenant, caseID)
		fees, _ := s.queryRows(r, `
			SELECT coalesce(invoice_no,'') AS invoice_no, kind, amount_cents, status, coalesce(due_date::text,'') AS due_date,
			       coalesce(paid_at::text,'') AS paid_at
			FROM public.invoices WHERE tenant=$1 AND case_id=$2 AND party=$3 AND status <> 'VOID' ORDER BY created_at DESC`, tenant, caseID, side)
		writeJSON(w, http.StatusOK, map[string]any{"case": c, "side": side, "dates": dates, "claims": claims, "fees": fees})
	}
}

func dateOf(dates []map[string]any, key string) string {
	for _, d := range dates {
		if d["key"] == key {
			s, _ := d["date"].(string)
			return s
		}
	}
	return ""
}
