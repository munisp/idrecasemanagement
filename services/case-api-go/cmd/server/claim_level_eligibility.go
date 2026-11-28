package main

// claim_level_eligibility.go — partial eligibility (change log 2026-11-28): the eligibility review also checks each
// claim on the dispute with that claim's own dates, so a dispute where some claims are out of time (or for services
// before the program started) and others are not is PARTIALLY ELIGIBLE: it goes on with the eligible claims.
//
// Decisions (agreed 2026-11-28):
//   - the minimum amount is tested on the ELIGIBLE claims only: dropping claims can take a dispute under it;
//   - fees are unchanged (still on every claim filed);
//   - a claim with no final determination date of its own is measured on the case's date, and says so.
//
// The review's result stays ELIGIBLE (reason "partially_eligible"), so the workflow, letters and clocks run as for
// any accepted dispute; the claims it finds ineligible are marked on the claims themselves (case_claims, by
// "Eligibility review") with the Partial Acceptance letter's wording. A claim a person marked by hand is left as it
// is, and counts as ineligible. If no claim is left eligible, the dispute is INELIGIBLE.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const claimReviewMarker = "Eligibility review"

// claimCheck is one claim's result, kept in the review's evidence ("claims") and printed on the checklist.
type claimCheck struct {
	Claim              string `json:"claim_number"`
	Eligible           bool   `json:"eligible"`
	Rule               string `json:"rule,omitempty"`    // over_filing_window | service_before_program_start | marked_by_staff
	Wording            string `json:"wording,omitempty"` // what the Partial Acceptance letter says after the claim number
	ServiceDate        string `json:"service_date,omitempty"`
	FinalDetermination string `json:"final_determination_at,omitempty"`
	DateFromCase       bool   `json:"date_from_case,omitempty"` // no date of its own: the case's date was used
	FilingDeadline     string `json:"filing_deadline,omitempty"`
	DisputedCents      *int64 `json:"disputed_cents,omitempty"`
	MarkedBy           string `json:"marked_by,omitempty"`
}

func claimRuleWording(rule string, months int) string {
	switch rule {
	case "over_filing_window":
		return fmt.Sprintf("were filed more than %d months after the final determination.", months)
	case "service_before_program_start":
		return "were for services provided before the program's start date."
	}
	return "are ineligible for review under the program."
}

type caseClaimRow struct {
	number, status, reason, by string
	service, fd                string
	cents                      *int64
}

// checkClaims is the pure part: each claim against the window and the program start, the case's date standing in
// for a claim without its own.
func checkClaims(cfg *ProgramConfig, in eligibilityInput, rows []caseClaimRow) []claimCheck {
	filed := time.Now()
	if f, err := time.Parse("2006-01-02", in.FiledOn); err == nil {
		filed = f
	}
	out := make([]claimCheck, 0, len(rows))
	for _, c := range rows {
		ck := claimCheck{Claim: c.number, Eligible: true, ServiceDate: c.service, DisputedCents: c.cents}
		if c.status == "INELIGIBLE" && c.by != claimReviewMarker { // a person's decision stands
			ck.Eligible, ck.Rule, ck.Wording, ck.MarkedBy = false, "marked_by_staff", c.reason, c.by
			out = append(out, ck)
			continue
		}
		ck.FinalDetermination = c.fd
		if ck.FinalDetermination == "" && in.FinalDeterminationAt != "" {
			ck.FinalDetermination, ck.DateFromCase = in.FinalDeterminationAt, true
		}
		if cfg.Eligibility.ServicesAfter != "" && c.service != "" {
			floor, ferr := time.Parse("2006-01-02", cfg.Eligibility.ServicesAfter)
			sd, serr := time.Parse("2006-01-02", c.service)
			if ferr == nil && serr == nil && !sd.After(floor) {
				ck.Eligible, ck.Rule = false, "service_before_program_start"
			}
		}
		if ck.Eligible && cfg.Eligibility.FilingWindowMonths > 0 && ck.FinalDetermination != "" {
			if fd, err := time.Parse("2006-01-02", ck.FinalDetermination); err == nil {
				deadline := fd.AddDate(0, cfg.Eligibility.FilingWindowMonths, 0)
				ck.FilingDeadline = deadline.Format("2006-01-02")
				if filed.After(deadline) {
					ck.Eligible, ck.Rule = false, "over_filing_window"
				}
			}
		}
		if !ck.Eligible {
			ck.Wording = claimRuleWording(ck.Rule, cfg.Eligibility.FilingWindowMonths)
		}
		out = append(out, ck)
	}
	return out
}

// decidePartial turns the claim results into the review's result. Pure: no database.
func decidePartial(result, reason string, evidence map[string]any, checks []claimCheck) (string, string) {
	var bad, good []claimCheck
	for _, c := range checks {
		if c.Eligible {
			good = append(good, c)
		} else {
			bad = append(bad, c)
		}
	}
	evidence["claims"] = checks
	if len(bad) == 0 {
		return result, reason
	}
	if len(good) == 0 { // nothing left to review: the dispute is ineligible, for the claims' own reason
		why := bad[0].Rule
		if why == "marked_by_staff" {
			why = "no_eligible_claims"
		}
		evidence["all_claims_ineligible"] = true
		return "INELIGIBLE", why
	}
	// The minimum amount, on the claims that stay (decision 2026-11-28). Only when every eligible claim has an amount;
	// otherwise the amount entered for the dispute stands, and the evidence says so.
	if minC, ok := evidence["threshold_min_cents"].(int64); ok {
		var sum int64
		known := true
		for _, c := range good {
			if c.DisputedCents == nil {
				known = false
				break
			}
			sum += *c.DisputedCents
		}
		if known {
			evidence["eligible_claims_amount_cents"] = sum
			if sum < minC {
				evidence["below_threshold_after_partial"] = true
				return "INELIGIBLE", "below_threshold"
			}
		} else {
			evidence["eligible_claims_amount_unknown"] = true
		}
	}
	evidence["partially_eligible"] = true
	evidence["eligible_claims"], evidence["ineligible_claims"] = len(good), len(bad)
	if result == "ELIGIBLE" && (reason == "" || reason == "partially_eligible") {
		reason = "partially_eligible"
	}
	return result, reason
}

// claimLevelEligibility runs after the case-level rules, on a dispute they did not find ineligible and that has at
// least two claims. It may turn the result into INELIGIBLE (no claim left, or under the minimum once claims drop).
func (s *server) claimLevelEligibility(ctx context.Context, tenant, caseID string, cfg *ProgramConfig, in eligibilityInput,
	result, reason string, evidence map[string]any) (string, string) {
	if result == "INELIGIBLE" {
		return result, reason
	}
	rows, err := s.db.Query(ctx, `SELECT claim_number, status, coalesce(ineligible_reason,''), coalesce(ineligible_by,''),
			coalesce(date_of_service::text,''), coalesce(date_of_final_determination::text,''), coalesce(disputed_cents, billed_cents)
		FROM public.case_claims WHERE tenant=$1 AND case_id=$2 ORDER BY claim_number`, tenant, caseID)
	if err != nil {
		return result, reason
	}
	var claims []caseClaimRow
	for rows.Next() {
		var c caseClaimRow
		if rows.Scan(&c.number, &c.status, &c.reason, &c.by, &c.service, &c.fd, &c.cents) == nil {
			claims = append(claims, c)
		}
	}
	rows.Close()
	if len(claims) < 2 {
		return result, reason
	}
	return decidePartial(result, reason, evidence, checkClaims(cfg, in, claims))
}

// markReviewedClaims writes the review's own findings onto the claims: those it found ineligible are marked (by
// "Eligibility review", with the letter's wording); those it had marked before and now finds eligible are accepted
// again. A person's marks are never touched. Called once the review is recorded.
func (s *server) markReviewedClaims(ctx context.Context, tenant, caseID, result string, evidence map[string]any) {
	checks, _ := evidence["claims"].([]claimCheck)
	if len(checks) == 0 || result == "INELIGIBLE" {
		return
	}
	var bad, good []string
	wording := map[string]string{}
	for _, c := range checks {
		if c.MarkedBy != "" {
			continue
		}
		if c.Eligible {
			good = append(good, c.Claim)
		} else {
			bad = append(bad, c.Claim)
			wording[c.Claim] = c.Wording
		}
	}
	for _, n := range bad {
		_, _ = s.db.Exec(ctx, `UPDATE public.case_claims SET status='INELIGIBLE', ineligible_reason=$4, ineligible_by=$5,
			ineligible_at=now() WHERE tenant=$1 AND case_id=$2 AND claim_number=$3`, tenant, caseID, n, wording[n], claimReviewMarker)
	}
	if len(good) > 0 {
		_, _ = s.db.Exec(ctx, `UPDATE public.case_claims SET status='SUBMITTED', ineligible_reason=NULL, ineligible_by=NULL,
			ineligible_at=NULL WHERE tenant=$1 AND case_id=$2 AND claim_number = ANY($3) AND ineligible_by=$4`,
			tenant, caseID, good, claimReviewMarker)
	}
	if len(bad) > 0 {
		s.logActivity(ctx, tenant, caseID, "CLAIM_ELIGIBILITY",
			fmt.Sprintf("Eligibility review: %d of %d claims ineligible (%s) — the dispute goes on with the others",
				len(bad), len(checks), strings.Join(bad, ", ")))
	}
}
