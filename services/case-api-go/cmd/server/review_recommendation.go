// Author: Patrick Munis at newwave
package main

// The reviewer's recommendation (2026-11-26, the change log's roles: the Coder drafts decision letters for coding
// determinations, the Nurse Reviewer for clinical ones). Completing a coding or clinical review records the outcome
// the reviewer recommends; it starts the decision report before the Attorney sees it:
//   - the outcome (Overturned / Partially overturned / Upheld);
//   - the letter: a coding-only review gives the claim dispute decision letter, a medical-necessity review (with or
//     without coding, change log #15) the medical-necessity (IRO) letter.
// The Attorney confirms or changes it. Once an outcome has been saved on the report, a recommendation never
// overwrites it; it is kept on the case (details.review_recommendation) and shown on the report.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

var recommendableOutcomes = map[string]bool{"OVERTURNED": true, "PARTIALLY_OVERTURNED": true, "UPHELD": true}

// recommendedReportType: the decision letter a review leads to.
func recommendedReportType(signal, scenario string) string {
	if signal == "CLINICAL_REVIEW_COMPLETE" || scenario == "med_nec" || scenario == "med_nec_coding" {
		return "IRO"
	}
	return "CLAIM_DISPUTE"
}

// recordRecommendation keeps the reviewer's recommendation and starts the decision report with it.
func (s *server) recordRecommendation(r *http.Request, tenant, caseID, signal string, data map[string]any) {
	out, _ := data["recommended_outcome"].(string)
	if !recommendableOutcomes[out] {
		return
	}
	scenario, _ := data["review_scenario"].(string)
	if scenario == "" {
		var sc string
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce(details->'review_recommendation'->>'scenario','') FROM tenant_%s.cases WHERE id::text=$1`,
			sanitizeTenant(tenant)), caseID).Scan(&sc)
		scenario = sc
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	rt := recommendedReportType(signal, scenario)
	who := map[string]string{"CODING_REVIEW_COMPLETE": "Coder", "CLINICAL_REVIEW_COMPLETE": "Nurse Reviewer"}[signal]
	rec := map[string]any{"outcome": out, "report_type": rt, "by": displayName(p), "role": who, "scenario": scenario,
		"at": time.Now().UTC().Format(time.RFC3339)}
	js, _ := json.Marshal(rec)
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`UPDATE tenant_%s.cases SET details = coalesce(details,'{}'::jsonb) || jsonb_build_object('review_recommendation', $2::jsonb)
		WHERE id::text=$1`, sanitizeTenant(tenant)), caseID, string(js))
	started := s.startReportFromRecommendation(r.Context(), tenant, caseID, out, rt, displayName(p))
	words := map[string]string{"OVERTURNED": "overturned", "PARTIALLY_OVERTURNED": "partially overturned", "UPHELD": "upheld"}[out]
	letter := map[string]string{"IRO": "the medical-necessity letter", "CLAIM_DISPUTE": "the claim dispute letter"}[rt]
	msg := fmt.Sprintf("The %s (%s) recommends: %s — %s", who, displayName(p), words, letter)
	if started {
		msg += "; the decision report is started with it for the Attorney to confirm"
	}
	s.logActivity(r.Context(), tenant, caseID, "REVIEW_RECOMMENDATION", msg)
}

// startReportFromRecommendation sets the report's outcome and letter type when no outcome is saved yet.
func (s *server) startReportFromRecommendation(ctx context.Context, tenant, caseID, outcome, reportType, by string) bool {
	tag, err := s.db.Exec(ctx, `
		INSERT INTO public.decision_reports AS d (tenant, case_id, report_type, outcome, sections, claims, disallowed, reviewers, section_authors, updated_by)
		VALUES ($1, $2, $3, $4, '{}', '[]', '[]', '[]', '{}', $5)
		ON CONFLICT (tenant, case_id) DO UPDATE SET report_type = EXCLUDED.report_type, outcome = EXCLUDED.outcome,
		  updated_by = EXCLUDED.updated_by, updated_at = now()
		WHERE d.outcome IS NULL`, tenant, caseID, reportType, outcome, by+" (recommendation)")
	return err == nil && tag.RowsAffected() > 0
}
