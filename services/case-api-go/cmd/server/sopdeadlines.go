// Author: Patrick Munis at newwave
// sopdeadlines.go — the contract's deadlines, measured by the system instead
// of by hand.
//
//   - Per-case deliverables (acceptance letter, cost estimate, rationale,
//     invoice, cover letter, closure letter) complete themselves when the
//     letter that produces them is sent, and are graded against their clock:
//     on time, late but within the contract's limit, or in breach.
//   - Reports due on a calendar (weekly on Mondays, monthly on the 10th) get
//     a DUE entry each period, so a missed one is visible.
//   - Contract-level deliverables are dated from the contract's execution,
//     effective and end dates, once an admin enters them.
//   - Fraud, waste and abuse: any staff member can raise a suspicion; the PM
//     assigns an independent reviewer and records the outcome, including a
//     referral to the agency.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type deliverableRule struct {
	Name        string   `json:"name"`
	ContractRef string   `json:"contract_ref"`
	DueRule     string   `json:"due_rule"`
	Format      string   `json:"format"`
	ProducedBy  []string `json:"produced_by"` // letter templates whose sending delivers this
}

func (s *server) deliverableRules(ctx context.Context, tenant string) []deliverableRule {
	var raw []byte
	if err := s.db.QueryRow(ctx,
		`SELECT coalesce(config->'deliverables','[]'::jsonb) FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw); err != nil {
		return nil
	}
	var rules []deliverableRule
	_ = json.Unmarshal(raw, &rules)
	return rules
}

// clockDue returns a clock's target date and its hard limit for a case. The
// hard limit equals the target when the clock has none. ok is false when the
// clock has not started (its basis date is not recorded).
func clockDue(rules []ClockRule, dates map[string]string, clock string) (target, hard time.Time, ok bool) {
	for _, rl := range rules {
		if rl.Name != clock {
			continue
		}
		raw := dates[rl.Basis]
		if len(raw) < 10 {
			return target, hard, false
		}
		basis, err := time.Parse("2006-01-02", raw[:10])
		if err != nil {
			return target, hard, false
		}
		add := func(n int) time.Time {
			if rl.DayType == "business" {
				return addBusinessDays(basis, n)
			}
			return basis.AddDate(0, 0, n)
		}
		target, hard = add(rl.Days), add(rl.Days)
		if rl.MaxDays > rl.Days {
			hard = add(rl.MaxDays)
		}
		return target, hard, true
	}
	return target, hard, false
}

// timeliness grades a delivery made on `on` against its dates.
func timeliness(on, target, hard time.Time) string {
	day := on.Truncate(24 * time.Hour)
	switch {
	case !day.After(target):
		return "ON_TIME"
	case !day.After(hard):
		return "LATE_WITHIN_LIMIT"
	}
	return "BREACH"
}

// recordCaseDeliverables marks the per-case deliverables a sent letter
// produces as delivered, once each, graded against the clock in their rule.
func (s *server) recordCaseDeliverables(r *http.Request, tenant, caseID, template string) {
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		return
	}
	dates := s.programDates(r, tenant, caseID)
	now := time.Now().UTC()
	for _, d := range s.deliverableRules(r.Context(), tenant) {
		if !contains(d.ProducedBy, template) {
			continue
		}
		var due, hardDue *string
		grade := "ON_TIME"
		if clock, isCase := strings.CutPrefix(d.DueRule, "case:"); isCase {
			if target, hard, ok := clockDue(cfg.Clocks, dates, clock); ok {
				t, h := target.Format("2006-01-02"), hard.Format("2006-01-02")
				due, hardDue = &t, &h
				grade = timeliness(now, target, hard)
			}
		}
		res, err := s.db.Exec(r.Context(), `
			INSERT INTO public.deliverables (tenant, name, contract_ref, due_rule, case_id, due_date, hard_due_date, status, delivered_at, timeliness)
			VALUES ($1,$2,$3,$4,$5,$6::date,$7::date,'DELIVERED',now(),$8)
			ON CONFLICT (tenant, name, case_id) DO UPDATE
			  SET status='DELIVERED', delivered_at=now(), timeliness=EXCLUDED.timeliness,
			      due_date=EXCLUDED.due_date, hard_due_date=EXCLUDED.hard_due_date
			  WHERE public.deliverables.delivered_at IS NULL`,
			tenant, d.Name, d.ContractRef, d.DueRule, caseID, due, hardDue, grade)
		if err != nil || res.RowsAffected() == 0 {
			continue // already delivered: the first delivery is the record
		}
		note := fmt.Sprintf("Contract deliverable %s %q delivered", d.ContractRef, d.Name)
		switch grade {
		case "LATE_WITHIN_LIMIT":
			note += " — LATE against its target date, within the contract's limit"
		case "BREACH":
			note += " — past the contract's limit"
			s.notify(r, tenant, "role:PM", "DELIVERABLE_BREACH",
				fmt.Sprintf("Case %s: deliverable %q went out past the contract's limit", caseID, d.Name), "#/cases/"+caseID)
		}
		s.logActivity(r.Context(), tenant, caseID, "DELIVERABLE", note)
	}
}

// outstandingDeliverables lists the per-case deliverables every open case
// owes and has not delivered: the acceptance letter and the determination
// package. (The cost estimate and the closure letter are owed only on some
// cases, so they appear when delivered, not as outstanding.)
func (s *server) outstandingDeliverables(r *http.Request, tenant string, cfg *ProgramConfig) []map[string]any {
	out := []map[string]any{}
	owed := []deliverableRule{}
	for _, d := range s.deliverableRules(r.Context(), tenant) {
		if strings.HasPrefix(d.DueRule, "case:") && d.Name != "Fee schedule / cost estimate" && !strings.HasPrefix(d.Name, "Closure letter") {
			owed = append(owed, d)
		}
	}
	if len(owed) == 0 {
		return out
	}
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT c.id::text, c.case_number, coalesce(c.program_dates,'{}'::jsonb), coalesce(c.internal_status,''),
		       coalesce((SELECT array_agg(name) FROM public.deliverables d
		                 WHERE d.tenant=$1 AND d.case_id=c.id::text AND d.delivered_at IS NOT NULL), '{}')
		FROM tenant_%s.cases c
		WHERE coalesce(c.agency_status,'') <> 'Closed' AND c.status <> 'AWAITING_PAYMENT'
		  AND c.program_dates ? 'packet_complete_at'
		ORDER BY c.opened_at LIMIT 500`, sanitizeTenant(tenant)), tenant)
	if err != nil {
		return out
	}
	defer rows.Close()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for rows.Next() {
		var id, number, internal string
		var datesAny map[string]any
		var delivered []string
		if rows.Scan(&id, &number, &datesAny, &internal, &delivered) != nil {
			continue
		}
		dates := map[string]string{}
		for k, v := range datesAny {
			dates[k] = fmt.Sprint(v)
		}
		for _, d := range owed {
			if contains(delivered, d.Name) {
				continue
			}
			target, hard, ok := clockDue(cfg.Clocks, dates, strings.TrimPrefix(d.DueRule, "case:"))
			if !ok {
				continue
			}
			state := "ON_TRACK"
			switch {
			case today.After(hard):
				state = "BREACH"
			case today.After(target):
				state = "LATE_WITHIN_LIMIT"
			case int(target.Sub(today).Hours()/24) <= 3:
				state = "DUE_SOON"
			}
			out = append(out, map[string]any{
				"case_id": id, "case_number": number, "internal_status": internal, "name": d.Name, "contract_ref": d.ContractRef,
				"due_date": target.Format("2006-01-02"), "hard_due_date": hard.Format("2006-01-02"),
				"days_left": int(target.Sub(today).Hours() / 24), "state": state,
			})
		}
	}
	return out
}

// contractDue computes a contract-level deliverable's date from the
// contract's own dates: "contract:execution+30d", "contract:effective-30d",
// "contract:end", "contract:end+60d", "contract:end+30bd". "" when the rule
// has no date (standing) or the contract date it needs is not entered.
func contractDue(rule string, dates map[string]string) string {
	spec, ok := strings.CutPrefix(rule, "contract:")
	if !ok {
		return ""
	}
	i := strings.IndexAny(spec, "+-")
	base, off := spec, ""
	if i >= 0 {
		base, off = spec[:i], spec[i:]
	}
	raw := dates[base+"_date"]
	if len(raw) < 10 {
		return ""
	}
	d, err := time.Parse("2006-01-02", raw[:10])
	if err != nil {
		return ""
	}
	if off != "" {
		var n int
		business := strings.HasSuffix(off, "bd")
		if _, err := fmt.Sscanf(strings.TrimRight(off[1:], "bd"), "%d", &n); err != nil {
			return ""
		}
		switch {
		case off[0] == '-':
			d = d.AddDate(0, 0, -n)
		case business:
			d = addBusinessDays(d, n)
		default:
			d = d.AddDate(0, 0, n)
		}
	}
	return d.Format("2006-01-02")
}

// setContractDates: PUT /program/contract-dates {execution_date, effective_date, end_date}
func (s *server) setContractDates(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN") {
		http.Error(w, `{"error":"forbidden: contract dates are set by an admin"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in map[string]string
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	clean := map[string]string{}
	for _, k := range []string{"execution_date", "effective_date", "end_date"} {
		v := strings.TrimSpace(in[k])
		if v == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", v); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": k + " must be a date in YYYY-MM-DD form"})
			return
		}
		clean[k] = v
	}
	cj, _ := json.Marshal(clean)
	if _, err := s.db.Exec(r.Context(),
		`UPDATE public.program_rules SET config = jsonb_set(config, '{contract}', $2::jsonb, true), updated_at=now() WHERE tenant=$1`,
		tenant, string(cj)); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "CONTRACT_DATES_SET", map[string]any{"by": p.Subject, "dates": clean})
	writeJSON(w, http.StatusOK, map[string]any{"contract": clean})
}

// reportPeriod returns the period key and due date of the report a rule
// currently owes: "weekly:MONDAY" -> this week's Monday; "monthly:10" -> the
// 10th of this month. ok is false for other rules.
func reportPeriod(rule string, now time.Time) (period string, due time.Time, ok bool) {
	day := now.UTC().Truncate(24 * time.Hour)
	switch {
	case strings.HasPrefix(rule, "weekly:"):
		back := (int(day.Weekday()) + 6) % 7 // days since Monday
		due = day.AddDate(0, 0, -back)
		y, w := due.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w), due, true
	case strings.HasPrefix(rule, "monthly:"):
		var dom int
		if _, err := fmt.Sscanf(rule[8:], "%d", &dom); err != nil || dom < 1 || dom > 28 {
			return "", due, false
		}
		due = time.Date(day.Year(), day.Month(), dom, 0, 0, 0, 0, time.UTC)
		return day.Format("2006-01"), due, true
	}
	return "", due, false
}

// sweepReportDeliverables opens this period's DUE entry for every calendar
// report of every programmed tenant, once, and tells the PM. Hourly; idempotent.
func (s *server) sweepReportDeliverables() int {
	ctx := context.Background()
	rows, err := s.db.Query(ctx, `SELECT tenant FROM public.program_rules`)
	if err != nil {
		return 0
	}
	var tenants []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tenants = append(tenants, t)
		}
	}
	rows.Close()
	opened := 0
	for _, tenant := range tenants {
		for _, d := range s.deliverableRules(ctx, tenant) {
			period, due, ok := reportPeriod(d.DueRule, time.Now())
			if !ok {
				continue
			}
			res, err := s.db.Exec(ctx, `
				INSERT INTO public.deliverables (tenant, name, contract_ref, due_rule, due_date, hard_due_date, status, period)
				VALUES ($1,$2,$3,$4,$5::date,$5::date,'DUE',$6)
				ON CONFLICT (tenant, name, period) WHERE period IS NOT NULL DO NOTHING`,
				tenant, d.Name, d.ContractRef, d.DueRule, due.Format("2006-01-02"), period)
			if err != nil || res.RowsAffected() == 0 {
				continue
			}
			opened++
			_, _ = s.db.Exec(ctx, `
				INSERT INTO public.notifications (tenant, user_sub, type, body, link)
				VALUES ($1,'role:PM','DELIVERABLE_DUE',$2,'#/deliverables')`,
				tenant, fmt.Sprintf("%s for %s is due %s — send it and mark it delivered", d.Name, period, due.Format("Jan 2")))
		}
	}
	return opened
}

// ---- Fraud, waste and abuse ---------------------------------------------------

// fwaAction: POST /cases/{caseId}/fwa {action: report|assign|conclude, ...}
//
//	report    any case staff: {reason}
//	assign    PM: {reviewer}                 the independent reviewer with medical expertise
//	conclude  PM: {agree: bool, summary}     agree = refer the case to the agency
func (s *server) fwaAction(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Action   string `json:"action"`
		Reason   string `json:"reason"`
		Reviewer string `json:"reviewer"`
		Agree    *bool  `json:"agree"`
		Summary  string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	var cur map[string]any
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(details->'fwa','{}'::jsonb) FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&cur); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	isPM := hasAnyRole(p, "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN")
	status, _ := cur["status"].(string)
	switch in.Action {
	case "report":
		if !hasAnyRole(p, caseStaffRoles...) {
			http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
			return
		}
		if strings.TrimSpace(in.Reason) == "" {
			http.Error(w, `{"error":"a reason is required"}`, http.StatusBadRequest)
			return
		}
		if status != "" && status != "NOT_SUBSTANTIATED" {
			http.Error(w, `{"error":"a fraud, waste or abuse review is already open on this case"}`, http.StatusConflict)
			return
		}
		cur = map[string]any{"status": "REPORTED", "reason": strings.TrimSpace(in.Reason), "reported_by": displayName(p), "reported_at": now}
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.escalations (tenant, case_id, clock, level, escalated_to, detail)
			VALUES ($1,$2,'FWA',2,'PM',$3)`, tenant, caseID, "Suspected fraud, waste or abuse: "+truncate(in.Reason, 1500))
		s.notify(r, tenant, "role:PM", "FWA_REPORTED",
			fmt.Sprintf("Case %s: %s suspects fraud, waste or abuse — assign an independent reviewer", caseID, displayName(p)), "#/cases/"+caseID)
	case "assign":
		if !isPM {
			http.Error(w, `{"error":"forbidden: the PM assigns the independent reviewer"}`, http.StatusForbidden)
			return
		}
		if status != "REPORTED" && status != "UNDER_REVIEW" {
			http.Error(w, `{"error":"nothing has been reported on this case"}`, http.StatusConflict)
			return
		}
		if strings.TrimSpace(in.Reviewer) == "" {
			http.Error(w, `{"error":"name the independent reviewer"}`, http.StatusBadRequest)
			return
		}
		cur["status"], cur["reviewer"], cur["assigned_by"], cur["assigned_at"] = "UNDER_REVIEW", strings.TrimSpace(in.Reviewer), displayName(p), now
	case "conclude":
		if !isPM {
			http.Error(w, `{"error":"forbidden: the PM records the outcome"}`, http.StatusForbidden)
			return
		}
		if status != "UNDER_REVIEW" {
			http.Error(w, `{"error":"assign an independent reviewer first"}`, http.StatusConflict)
			return
		}
		if in.Agree == nil || strings.TrimSpace(in.Summary) == "" {
			http.Error(w, `{"error":"agree (true/false) and a summary of the findings are required"}`, http.StatusBadRequest)
			return
		}
		cur["status"] = "NOT_SUBSTANTIATED"
		if *in.Agree {
			cur["status"] = "REFERRED_TO_AGENCY"
		}
		cur["summary"], cur["concluded_by"], cur["concluded_at"] = strings.TrimSpace(in.Summary), displayName(p), now
	default:
		http.Error(w, `{"error":"action must be report, assign or conclude"}`, http.StatusBadRequest)
		return
	}
	if err := s.setDetailsPath(r, tenant, caseID, "fwa", "", cur); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "FWA",
		fmt.Sprintf("Fraud, waste and abuse: %s by %s — now %s", in.Action, displayName(p), cur["status"]))
	s.logAudit(r.Context(), tenant, caseID, "FWA_"+strings.ToUpper(in.Action), map[string]any{"by": p.Subject, "fwa": cur})
	writeJSON(w, http.StatusOK, map[string]any{"fwa": cur})
}

// closureReasonFor maps how a case closed to the program's
// "Withdrawal/Dismissed Reason" list. "" when there is nothing to set.
func closureReasonFor(template, eligReason string) (reason, outcome string) {
	switch template {
	case "ineligible":
		switch eligReason {
		case "over_filing_window", "over_12_months":
			return "Dismissed-Timeliness eligibility failed", "Dismissed"
		case "plan_not_fl_regulated":
			return "Member plan is not regulated by Florida", "Dismissed"
		case "self_funded_plan":
			return "Self-Funded Plan", "Dismissed"
		}
		return "Other", "Dismissed"
	case "dismissal":
		return "Provider No Response", "Dismissed"
	case "withdrawal":
		return "Other", "Withdrawn"
	}
	return "", ""
}

// setCorrespondenceRecipients: PUT /program/recipients {agency: [...], pm: [...]}
// — the agency contacts every letter copies (and the Final Order goes to),
// and the PM address(es) copied on the Final Order. Admin only.
func (s *server) setCorrespondenceRecipients(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN") {
		http.Error(w, `{"error":"forbidden: recipients are set by an admin"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Agency []string `json:"agency"`
		PM     []string `json:"pm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	clean := func(xs []string) ([]string, string) {
		out := []string{}
		for _, x := range xs {
			x = strings.TrimSpace(x)
			if x == "" {
				continue
			}
			if !validMailbox(x) {
				return nil, x
			}
			out = append(out, x)
		}
		return out, ""
	}
	agency, bad := clean(in.Agency)
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a valid email address: " + bad})
		return
	}
	pm, bad := clean(in.PM)
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a valid email address: " + bad})
		return
	}
	aj, _ := json.Marshal(agency)
	pj, _ := json.Marshal(pm)
	if _, err := s.db.Exec(r.Context(), `
		UPDATE public.program_rules SET config = jsonb_set(jsonb_set(config,
		  '{correspondence,agency_recipients}', $2::jsonb, true), '{correspondence,pm_recipients}', $3::jsonb, true), updated_at=now()
		WHERE tenant=$1`, tenant, string(aj), string(pj)); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "CORRESPONDENCE_RECIPIENTS_SET", map[string]any{"by": p.Subject, "agency": agency, "pm": pm})
	writeJSON(w, http.StatusOK, map[string]any{"agency": agency, "pm": pm})
}

// getCorrespondenceRecipients: GET /program/recipients
func (s *server) getCorrespondenceRecipients(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var a, p []byte
	_ = s.db.QueryRow(r.Context(), `
		SELECT coalesce(config->'correspondence'->'agency_recipients','[]'::jsonb), coalesce(config->'correspondence'->'pm_recipients','[]'::jsonb)
		FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&a, &p)
	agency, pm := []string{}, []string{}
	_ = json.Unmarshal(a, &agency)
	_ = json.Unmarshal(p, &pm)
	writeJSON(w, http.StatusOK, map[string]any{"agency": agency, "pm": pm})
}
