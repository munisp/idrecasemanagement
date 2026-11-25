// Author: Patrick Munis at newwave
package main

// tpa_calendar.go — a TPA's calendar: the deadlines on the disputes it filed,
// and its own tasks.
//
//   FEE_DUE         an unpaid initial fee (the dispute opens when it is paid)
//   INVOICE_DUE     an unpaid review invoice billed to the provider side
//   DOCS_DUE        documents the review team asked the TPA for
//   PERMISSION_DUE  the provider's permission to proceed after a cost estimate
//   PLAN_RESPONSE   the health plan's response deadline (to watch, not to act on)
//   DECISION_DUE    the recommendation to AHCA (60 days from receipt)
//   TASK            the TPA's own tasks
//
//   GET  /calendar                  (a TPA gets these items)
//   POST /tpa/tasks                 {subject, due_date, case_id?}
//   POST /tpa/tasks/{taskId}/done
//
// A TPA's task is a public.tasks row assigned to "tpa:<id>": shared by
// everyone in the organization, invisible to every other organization.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type calItem struct {
	Ref        string `json:"ref"`
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	Due        string `json:"due"`
	CaseID     string `json:"case_id"`
	CaseNumber string `json:"case_number,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

func (s *server) tpaCalendarItems(r *http.Request, tenant, tpaID, clientID string) []calItem {
	ctx, t := r.Context(), sanitizeTenant(tenant)
	var out []calItem
	add := func(it calItem) {
		if len(it.Due) >= 10 {
			it.Due = it.Due[:10]
			out = append(out, it)
		}
	}

	// Unpaid invoices on its disputes: the initial fee, and review invoices billed to the provider.
	if rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT f.id::text, f.kind, f.amount_cents, f.due_date::text, f.case_id, c.case_number
		FROM public.invoices f
		JOIN public.case_origin o ON o.tenant=f.tenant AND o.case_id=f.case_id
		JOIN tenant_%s.cases c ON c.id::text=f.case_id
		WHERE f.tenant=$1 AND o.tpa_id::text=$2 AND ($3::text='' OR o.client_id::text=$3) AND f.status='OPEN' AND f.due_date IS NOT NULL
		  AND (f.kind='INITIAL_FEE' OR f.party='PROVIDER')
		  AND c.status NOT IN ('WITHDRAWN','CLOSED','INELIGIBLE')`, t), tenant, tpaID, clientID); err == nil {
		for rows.Next() {
			var id, kind, due, caseID, num string
			var cents int64
			if rows.Scan(&id, &kind, &cents, &due, &caseID, &num) == nil {
				if kind == "INITIAL_FEE" {
					add(calItem{id, "FEE_DUE", "Initial fee due — " + num, due, caseID, num, usd(cents) + "; the dispute opens when it is paid"})
				} else {
					add(calItem{id, "INVOICE_DUE", "Review invoice due — " + num, due, caseID, num, usd(cents)})
				}
			}
		}
		rows.Close()
	}

	// Documents the review team asked the TPA for.
	if rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT q.id::text, q.due_date::text, q.case_id, c.case_number, jsonb_array_length(coalesce(q.items,'[]'::jsonb))
		FROM public.document_requests q JOIN tenant_%s.cases c ON c.id::text=q.case_id
		WHERE q.tenant=$1 AND q.org=$2 AND q.status='OPEN' AND q.due_date IS NOT NULL`+inClientSQL("q.case_id", clientID), t), tenant, "tpa:"+tpaID); err == nil {
		for rows.Next() {
			var id, due, caseID, num string
			var n int
			if rows.Scan(&id, &due, &caseID, &num, &n) == nil {
				add(calItem{id, "DOCS_DUE", "Documents due — " + num, due, caseID, num,
					fmt.Sprintf("%d document%s requested; answer in Document requests", n, map[bool]string{true: "", false: "s"}[n == 1])})
			}
		}
		rows.Close()
	}

	// Program clocks on its open disputes.
	var rules []ClockRule
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		rules = cfg.Clocks
	}
	type openCase struct {
		id, num        string
		dates          map[string]string
		hasPermission  bool
		recommendation bool
	}
	var cases []openCase
	if rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT c.id::text, c.case_number, coalesce(c.program_dates,'{}'::jsonb),
		       EXISTS (SELECT 1 FROM public.case_activities a WHERE a.tenant=$1 AND a.case_id=c.id::text AND a.type='ESTIMATE_PERMISSION')
		FROM public.case_origin o JOIN tenant_%s.cases c ON c.id::text=o.case_id
		WHERE o.tenant=$1 AND o.tpa_id::text=$2 AND ($3::text='' OR o.client_id::text=$3)
		  AND c.status NOT IN ('WITHDRAWN','CLOSED','INELIGIBLE','AWAITING_PAYMENT')
		  AND coalesce(c.internal_status,'') NOT IN ('Provider Closure Letter Issued','Ineligible','Dismissed','Withdrawn',
		      'Provider - Withdrawal','Plan Opt-Out','Invoice Paid','Decided - Invoice Paid','Closed-Refunded')`, t), tenant, tpaID, clientID); err == nil {
		for rows.Next() {
			var oc openCase
			var raw map[string]any
			if rows.Scan(&oc.id, &oc.num, &raw, &oc.hasPermission) == nil {
				oc.dates = map[string]string{}
				for k, v := range raw {
					oc.dates[k] = fmt.Sprint(v)
				}
				oc.recommendation = oc.dates["date_recommendation_sent_to_agency"] != ""
				cases = append(cases, oc)
			}
		}
		rows.Close()
	}
	for _, oc := range cases {
		if !oc.hasPermission {
			if due, _, ok := clockDue(rules, oc.dates, "PROVIDER_PERMISSION"); ok {
				add(calItem{"perm:" + oc.id, "PERMISSION_DUE", "Permission to proceed due — " + oc.num, due.Format("2006-01-02"), oc.id, oc.num,
					"The cost estimate was sent; without the provider's permission by this date the dispute is dismissed"})
			}
		}
		if !oc.recommendation {
			if due, _, ok := clockDue(rules, oc.dates, "AGENCY_RECOMMENDATION"); ok {
				add(calItem{"rec:" + oc.id, "DECISION_DUE", "Recommendation due to AHCA — " + oc.num, due.Format("2006-01-02"), oc.id, oc.num,
					"60 calendar days from receipt; AHCA then issues the Final Order"})
			}
		}
	}

	// The health plan's response deadline on disputes the TPA filed.
	if rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT rs.case_id, c.case_number,
		       coalesce(c.program_dates->>'date_health_plan_response_due','') AS response_due,
		       coalesce(c.program_dates->>'plan_notified_at','') AS notified_at,
		       coalesce(least(rs.notified_at, rs.emailed_at)::text,'') AS told_at, rs.name
		FROM public.case_respondents rs
		JOIN public.case_origin o ON o.tenant=rs.tenant AND o.case_id=rs.case_id
		JOIN tenant_%s.cases c ON c.id::text=rs.case_id
		WHERE rs.tenant=$1 AND o.tpa_id::text=$2 AND ($3::text='' OR o.client_id::text=$3) AND rs.response_status='AWAITING'
		  AND (rs.notified_at IS NOT NULL OR rs.emailed_at IS NOT NULL)`, t), tenant, tpaID, clientID); err == nil {
		s.fillResponseDue(r, tenant, rows)
		for _, x := range rows {
			due, _ := x["response_due"].(string)
			cid, _ := x["case_id"].(string)
			num, _ := x["case_number"].(string)
			name, _ := x["name"].(string)
			add(calItem{"plan:" + cid, "PLAN_RESPONSE", "Health plan's response due — " + num, due, cid, num,
				name + " has until this date; no response becomes a default determination for your client"})
		}
	}

	// Its own tasks: the whole organization's, so not in one client's calendar.
	if clientID != "" {
		if out == nil {
			out = []calItem{}
		}
		return out
	}
	if rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT k.id::text, k.subject, k.due_date::text, coalesce(k.case_id,''), coalesce(c.case_number,''),
		       k.status='DONE', coalesce(to_char(k.completed_at,'YYYY-MM-DD"T"HH24:MI'),'')
		FROM public.tasks k LEFT JOIN tenant_%s.cases c ON c.id::text=k.case_id
		WHERE k.tenant=$1 AND k.assignee=$2 AND k.due_date IS NOT NULL
		  AND (k.status='OPEN' OR (k.status='DONE' AND k.completed_at > now() - interval '45 days'))`, t), tenant, "tpa:"+tpaID); err == nil {
		for rows.Next() {
			var it calItem
			var done bool
			var doneAt string
			if rows.Scan(&it.Ref, &it.Label, &it.Due, &it.CaseID, &it.CaseNumber, &done, &doneAt) == nil {
				it.Kind = "TASK"
				if done { // shown ticked, with when
					it.Kind, it.Detail = "TASK_DONE", "done "+doneAt
				}
				add(it)
			}
		}
		rows.Close()
	}
	if out == nil {
		out = []calItem{}
	}
	return out
}

// tpaTaskCreate: POST /tpa/tasks -- a task for the whole organization.
func (s *server) tpaTaskCreate(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"not linked to any TPA organization"}`, http.StatusNotFound)
		return
	}
	var in struct {
		Subject string `json:"subject"`
		DueDate string `json:"due_date"`
		CaseID  string `json:"case_id"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&in) != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	in.Subject = strings.TrimSpace(in.Subject)
	if in.Subject == "" || len(in.Subject) > 300 {
		http.Error(w, `{"error":"say what the task is (up to 300 characters)"}`, http.StatusBadRequest)
		return
	}
	if _, err := time.Parse("2006-01-02", in.DueDate); err != nil {
		http.Error(w, `{"error":"due_date must be a date (YYYY-MM-DD)"}`, http.StatusBadRequest)
		return
	}
	var caseArg any
	if in.CaseID != "" {
		if !s.payerOwnsCase(r.Context(), tenant, payer{kind: "tpa", id: org.ID}, in.CaseID) {
			http.Error(w, `{"error":"that dispute is not one your organization filed"}`, http.StatusNotFound)
			return
		}
		caseArg = in.CaseID
	}
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.tasks (tenant, subject, case_id, assignee, due_date, status, created_by)
		VALUES ($1,$2,$3,$4,$5::date,'OPEN',$6) RETURNING id::text`,
		tenant, in.Subject, caseArg, "tpa:"+org.ID, in.DueDate, p.Subject).Scan(&id); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// tpaTaskDone: POST /tpa/tasks/{taskId}/done.
func (s *server) tpaTaskDone(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"not linked to any TPA organization"}`, http.StatusNotFound)
		return
	}
	tag, err := s.db.Exec(r.Context(), `
		UPDATE public.tasks SET status='DONE', completed_at=now()
		WHERE tenant=$1 AND id::text=$2 AND assignee=$3 AND status='OPEN'`, tenant, chi.URLParam(r, "taskId"), "tpa:"+org.ID)
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, `{"error":"no such open task for your organization"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "DONE"})
}
