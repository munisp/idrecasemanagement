// sopfields.go — the case fields the SOP assigns to one role each.
//
//   - Review notes: five fields, each written by the role that owns it
//     (Reviewer/PM: Internal Case Review; Coder: Coder Review;
//     Nurse/Physician: Clinical Review; Attorney: Legal Review; and the
//     external Comments/Notes that go to the agency).
//   - Dispute Summary: the Coder's -- plan type verified from the plan's
//     letterhead, the summary, and the full review cost estimate.
//   - AOR confirmation: the Attorney's ruling on an AOR the Reviewer flagged.
//   - Eligibility checklist: a printable record of the latest review.
//
// Everything is kept in the case's details JSON, so it travels with the case
// and needs no schema of its own.
package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// reviewNoteFields: field -> label and the roles that may write it.
var reviewNoteFields = map[string]struct {
	Label string
	Roles []string
}{
	"internal_case_review": {"Internal Case Review notes", []string{"CASE_MANAGER", "PM"}},
	"coder_review":         {"Coder Review notes", []string{"CODER"}},
	"clinical_review":      {"Clinical Review notes", []string{"NURSE_PHYSICIAN", "DOCTOR", "NURSE"}},
	"legal_review":         {"Legal Review notes", []string{"ATTORNEY"}},
	"external":             {"Comments/Notes (External for Florida)", []string{"CASE_MANAGER", "PM"}},
}

// mayWriteReviewNote: the owning roles, plus the admins.
func mayWriteReviewNote(p principal, field string) bool {
	f, ok := reviewNoteFields[field]
	if !ok {
		return false
	}
	return hasAnyRole(p, append(append([]string{}, f.Roles...), "FEDERAL_ADMIN", "PLATFORM_ADMIN")...)
}

// setDetailsPath writes one object under details.<key>.<sub> (or details.<key>
// when sub is ""), creating the parent if the case has none yet.
func (s *server) setDetailsPath(r *http.Request, tenant, caseID, key, sub string, value any) error {
	vj, _ := json.Marshal(value)
	q := `UPDATE tenant_%s.cases SET details = jsonb_set(details, ARRAY[$2::text], $3::jsonb, true), updated_at=now() WHERE id::text=$1`
	args := []any{caseID, key, string(vj)}
	if sub != "" {
		q = `UPDATE tenant_%s.cases SET details = jsonb_set(details, ARRAY[$2::text],
		       coalesce(details->$2::text, '{}'::jsonb) || jsonb_build_object($4::text, $3::jsonb), true), updated_at=now()
		     WHERE id::text=$1`
		args = append(args, sub)
	}
	res, err := s.db.Exec(r.Context(), fmt.Sprintf(q, sanitizeTenant(tenant)), args...)
	if err == nil && res.RowsAffected() == 0 {
		return fmt.Errorf("not found")
	}
	return err
}

// setReviewNote: PUT /cases/{caseId}/review-notes {field, text}
func (s *server) setReviewNote(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Field string `json:"field"`
		Text  string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	f, ok := reviewNoteFields[in.Field]
	if !ok {
		http.Error(w, `{"error":"unknown notes field"}`, http.StatusBadRequest)
		return
	}
	if !mayWriteReviewNote(p, in.Field) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": fmt.Sprintf("forbidden: %s are written by %s", f.Label, strings.Join(f.Roles, " / "))})
		return
	}
	in.Text = strings.TrimSpace(in.Text)
	if len(in.Text) > 20000 {
		http.Error(w, `{"error":"notes are too long (20,000 characters at most)"}`, http.StatusBadRequest)
		return
	}
	note := map[string]string{"text": in.Text, "by": displayName(p), "at": time.Now().UTC().Format(time.RFC3339)}
	if err := s.setDetailsPath(r, tenant, caseID, "review_notes", in.Field, note); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "REVIEW_NOTES", f.Label+" updated by "+displayName(p))
	s.logAudit(r.Context(), tenant, caseID, "REVIEW_NOTES_UPDATED", map[string]any{"by": p.Subject, "field": in.Field})
	writeJSON(w, http.StatusOK, map[string]any{"field": in.Field, "note": note})
}

// setDisputeSummary: PUT /cases/{caseId}/dispute-summary — the Coder's tab.
func (s *server) setDisputeSummary(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CODER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: the Dispute Summary is completed by the Coder"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if !s.stepGuard(w, r, tenant, caseID, "summary") {
		return
	}
	var in struct {
		PlanType             string `json:"plan_type"`
		VerifiedFromLetter   bool   `json:"plan_type_verified_from_letterhead"`
		Summary              string `json:"summary"`
		EstimateCents        *int64 `json:"full_review_estimate_cents"`
		EstimateBasis        string `json:"estimate_basis"` // the Estimated Cost Matrix row the figure came from
		CodingReviewRequired *bool  `json:"coding_review_required"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if in.EstimateCents != nil && (*in.EstimateCents < 0 || *in.EstimateCents > maxDisputedAmountCents) {
		http.Error(w, `{"error":"full_review_estimate_cents is out of range"}`, http.StatusBadRequest)
		return
	}
	if len(in.Summary) > 20000 {
		http.Error(w, `{"error":"summary is too long (20,000 characters at most)"}`, http.StatusBadRequest)
		return
	}
	sum := map[string]any{
		"plan_type": strings.TrimSpace(in.PlanType), "plan_type_verified_from_letterhead": in.VerifiedFromLetter,
		"summary": strings.TrimSpace(in.Summary), "estimate_basis": strings.TrimSpace(in.EstimateBasis),
		"by": displayName(p), "at": time.Now().UTC().Format(time.RFC3339),
	}
	if in.EstimateCents != nil {
		sum["full_review_estimate_cents"] = *in.EstimateCents
	}
	if in.CodingReviewRequired != nil {
		sum["coding_review_required"] = *in.CodingReviewRequired
	}
	if err := s.setDetailsPath(r, tenant, caseID, "dispute_summary", "", sum); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	msg := "Dispute Summary completed by " + displayName(p)
	if in.EstimateCents != nil {
		// The SOP has the estimate recorded where the Reviewer and PM read it.
		msg += fmt.Sprintf(" — full review cost estimate $%d.%02d", *in.EstimateCents/100, *in.EstimateCents%100)
		if b := strings.TrimSpace(in.EstimateBasis); b != "" {
			msg += " (" + b + ")"
		}
	}
	s.logActivity(r.Context(), tenant, caseID, "DISPUTE_SUMMARY", msg)
	s.logAudit(r.Context(), tenant, caseID, "DISPUTE_SUMMARY_UPDATED", map[string]any{"by": p.Subject, "summary": sum})
	writeJSON(w, http.StatusOK, map[string]any{"dispute_summary": sum})
}

// confirmAOR: POST /cases/{caseId}/aor-confirmation {valid, reason} — the
// Attorney's ruling on an AOR the Reviewer recorded as invalid.
func (s *server) confirmAOR(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: AOR validity is confirmed by the Attorney"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if !s.stepGuard(w, r, tenant, caseID, "aor_confirm") {
		return
	}
	var in struct {
		Valid  *bool  `json:"valid"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Valid == nil || strings.TrimSpace(in.Reason) == "" {
		http.Error(w, `{"error":"valid (true/false) and a reason are required"}`, http.StatusBadRequest)
		return
	}
	conf := map[string]any{"valid": *in.Valid, "reason": strings.TrimSpace(in.Reason),
		"by": displayName(p), "at": time.Now().UTC().Format(time.RFC3339)}
	if err := s.setDetailsPath(r, tenant, caseID, "aor_confirmation", "", conf); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	verdict, next := "INVALID", "ask the filer for a revised AOR; the case stays on hold until it arrives"
	if *in.Valid {
		verdict, next = "VALID", "record the AOR as valid and run the eligibility review again"
		_ = s.setDetailsPath(r, tenant, caseID, "aor_valid", "", true)
	}
	s.logActivity(r.Context(), tenant, caseID, "AOR_CONFIRMATION",
		fmt.Sprintf("Attorney %s confirmed the AOR is %s — %s", displayName(p), verdict, conf["reason"]))
	s.logAudit(r.Context(), tenant, caseID, "AOR_CONFIRMED", map[string]any{"by": p.Subject, "valid": *in.Valid, "reason": conf["reason"]})
	s.notify(r, tenant, "role:CASE_MANAGER", "AOR_CONFIRMED",
		fmt.Sprintf("Case %s: the Attorney confirmed the AOR is %s — %s.", caseID, verdict, next), "#/cases/"+caseID)
	writeJSON(w, http.StatusOK, map[string]any{"aor_confirmation": conf})
}

// eligibilityChecklist: GET /cases/{caseId}/eligibility/checklist — the
// Eligibility Checklist for the latest review, as a page to print or save.
func (s *server) eligibilityChecklist(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, caseStaffRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var caseNumber string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	var result, reason, decidedBy string
	var evJ []byte
	var at time.Time
	if err := s.db.QueryRow(r.Context(), `
		SELECT result, coalesce(reason,''), evidence, coalesce(decided_by,''), created_at
		FROM public.eligibility_reviews WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC LIMIT 1`,
		tenant, caseID).Scan(&result, &reason, &evJ, &decidedBy, &at); err != nil {
		http.Error(w, `{"error":"no eligibility review has been recorded on this case"}`, http.StatusNotFound)
		return
	}
	ev := map[string]any{}
	_ = json.Unmarshal(evJ, &ev)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="Eligibility Checklist %s.html"`, caseNumber))
	fmt.Fprint(w, eligibilityChecklistHTML(caseNumber, result, reason, decidedBy, at, ev))
}

// eligibilityChecklistHTML renders the checklist. Pure, so it can be tested.
func eligibilityChecklistHTML(caseNumber, result, reason, decidedBy string, at time.Time, ev map[string]any) string {
	e := html.EscapeString
	keys := make([]string, 0, len(ev))
	for k := range ev {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var rows strings.Builder
	for _, k := range keys {
		v := ev[k]
		if v == nil {
			v = "—"
		}
		fmt.Fprintf(&rows, "<tr><td>%s</td><td>%s</td></tr>", e(strings.ReplaceAll(k, "_", " ")), e(fmt.Sprint(v)))
	}
	if reason != "" {
		reason = " — " + strings.ReplaceAll(reason, "_", " ")
	}
	return fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>Eligibility Checklist %s</title>
<style>body{font:14px/1.5 Georgia,serif;max-width:720px;margin:32px auto;padding:0 16px;color:#111}
h1{font-size:20px}table{border-collapse:collapse;width:100%%;margin-top:12px}
td{border:1px solid #999;padding:6px 10px;vertical-align:top}td:first-child{width:42%%;text-transform:capitalize}
.r{font-weight:bold}</style></head><body>
<h1>Eligibility Checklist — %s</h1>
<p>Result: <span class="r">%s</span>%s<br>Reviewed by %s on %s</p>
<table>%s</table>
<p style="margin-top:24px;font-size:12px">Generated from the eligibility review recorded on the case. The review record itself cannot be edited; a correction is a new review.</p>
</body></html>`, e(caseNumber), e(caseNumber), e(result), e(reason),
		e(decidedBy), at.UTC().Format("January 2, 2006"), rows.String())
}
