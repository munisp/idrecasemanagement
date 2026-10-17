package main

// Case-worker productivity: eligibility auto-adjudication + queue triage.
//
// The workload analysis is unambiguous about where case-manager minutes go:
// eligibility review is the single biggest sink (the federal program found
// the same — it is their primary processing bottleneck), and a FIFO queue
// forces expert judgment onto cases that needed one-click confirmation.
//
//   Lever 1 — auto-eligibility: evalEligibility is the SAME decision logic
//     checkEligibility has always used, refactored pure. autoEligibility
//     derives the inputs from the case record + analyzed document
//     extractions and only decides when every input the program rules
//     actually need is present — anything missing routes to a human with
//     an explicit "what's missing" list. Miss, don't guess: an auto
//     "ELIGIBLE" fabricated from absent evidence would be worse than no
//     automation.
//   Lever 2/4 — triage lanes + SLA: listCases computes a triage_lane
//     (AUTO_REVIEW | STANDARD | COMPLEX) and sla_days_remaining per case,
//     sortable (sort=sla = oldest first) and filterable (lane=). Lanes are
//     computed in SQL from signals the platform already has: batches,
//     duplicates, escalation history, dollar materiality, and whether the
//     numbers needed for a one-click confirm are even present.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	chi "github.com/go-chi/chi/v5"
)

// eligibilityInput is everything evalEligibility needs. Pointer fields are
// tri-state: nil = unknown (auto-adjudication must NOT decide on unknowns
// the rules would have used).
type eligibilityInput struct {
	ProviderType         string   `json:"provider_type"`
	Contracted           *bool    `json:"contracted"`
	DisputedAmountCents  int64    `json:"disputed_amount_cents"`
	FinalDeterminationAt string   `json:"final_determination_at"` // YYYY-MM-DD
	Flags                []string `json:"flags"`
	AORValid             *bool    `json:"aor_valid"`
	AORReason            string   `json:"aor_reason"`   // why the Reviewer found the AOR valid or not
	ServiceDate          string   `json:"service_date"` // YYYY-MM-DD, earliest date of service in dispute
	DisputeType          string   `json:"dispute_type"` // denied_in_full | denied_in_part | underpaid | overpaid
}

// noOverrideReasons are computed INELIGIBLE results a reviewer may not set
// aside: the filing window is statutory and not extendable.
var noOverrideReasons = map[string]bool{"over_filing_window": true, "over_12_months": true}

// invalidEligibilityInput returns a message when a supplied date is not a
// YYYY-MM-DD calendar date, "" otherwise.
func invalidEligibilityInput(in eligibilityInput) string {
	for name, v := range map[string]string{"final_determination_at": in.FinalDeterminationAt, "service_date": in.ServiceDate} {
		if v == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return name + " must be a date in YYYY-MM-DD form"
		}
	}
	return ""
}

// evalEligibility is the pure decision core shared by checkEligibility
// (human-invoked) and autoEligibility (system-invoked). Same rules, same
// thresholds, same window math — one implementation, two callers, so the
// auto path can never drift from what a human reviewer would compute.
func evalEligibility(cfg *ProgramConfig, in eligibilityInput) (result, reason string, evidence map[string]any) {
	result, reason = "ELIGIBLE", ""
	evidence = map[string]any{"flags": in.Flags, "provider_type": in.ProviderType}
	if in.AORValid != nil {
		evidence["aor_valid"] = *in.AORValid
		if strings.TrimSpace(in.AORReason) != "" {
			evidence["aor_reason"] = strings.TrimSpace(in.AORReason)
		}
	}

	// explicit ineligibility flags win
	for _, f := range in.Flags {
		if contains(cfg.Eligibility.Reasons, f) {
			return "INELIGIBLE", f, evidence
		}
	}
	// claim type: the program takes denied, underpaid and overpaid claims.
	if len(cfg.Eligibility.DisputeTypes) > 0 && in.DisputeType != "" {
		evidence["dispute_type"] = in.DisputeType
		if !contains(cfg.Eligibility.DisputeTypes, in.DisputeType) {
			return "INELIGIBLE", "not_an_eligible_dispute_type", evidence
		}
	}
	// date of service: only services rendered after the program start count.
	if cfg.Eligibility.ServicesAfter != "" && in.ServiceDate != "" {
		floor, ferr := time.Parse("2006-01-02", cfg.Eligibility.ServicesAfter)
		sd, serr := time.Parse("2006-01-02", in.ServiceDate)
		if ferr == nil && serr == nil {
			evidence["services_after"] = cfg.Eligibility.ServicesAfter
			evidence["service_date"] = in.ServiceDate
			if !sd.After(floor) {
				return "INELIGIBLE", "service_before_program_start", evidence
			}
		}
	}
	// filing window (12-month style)
	if cfg.Eligibility.FilingWindowMonths > 0 && in.FinalDeterminationAt != "" {
		if fd, err := time.Parse("2006-01-02", in.FinalDeterminationAt); err == nil {
			deadline := fd.AddDate(0, cfg.Eligibility.FilingWindowMonths, 0)
			evidence["filing_deadline"] = deadline.Format("2006-01-02")
			if time.Now().After(deadline) {
				return "INELIGIBLE", "over_filing_window", evidence
			}
		}
	}
	// threshold matrix. A provider_type that matches none of the configured
	// entries (typo, free-text mismatch, or simply blank) is a data error,
	// not "no threshold applies" -- FL's own matrix already has an explicit
	// "other" catch-all with min_cents:0 for anything that doesn't fit the
	// named categories, so a real submission always matches SOMETHING.
	// Silently falling through left evalEligibility defaulting to its
	// initial ELIGIBLE regardless of amount -- confirmed live, a malformed
	// provider_type passed eligibility at $1.
	knownType := false
	for _, th := range cfg.Eligibility.Thresholds {
		if th.ProviderType != in.ProviderType {
			continue
		}
		knownType = true
		if th.Contracted != nil && in.Contracted != nil && *th.Contracted != *in.Contracted {
			continue
		}
		evidence["threshold_min_cents"] = th.MinCents
		if in.DisputedAmountCents < th.MinCents {
			return "INELIGIBLE", "below_threshold", evidence
		}
		break
	}
	if len(cfg.Eligibility.Thresholds) > 0 && !knownType {
		return "INELIGIBLE", "unrecognized_provider_type", evidence
	}
	// AOR hold
	if in.AORValid != nil && !*in.AORValid {
		return "HOLD_AOR", "aor_invalid_pending_attorney", evidence
	}
	return result, reason, evidence
}

// missingEligibilityInputs lists which rule-required inputs are absent.
// The auto path decides ONLY when this is empty (or the missing inputs
// belong to rules the program doesn't use).
func missingEligibilityInputs(cfg *ProgramConfig, in eligibilityInput) []string {
	var missing []string
	if len(cfg.Eligibility.Thresholds) > 0 {
		knownType := in.ProviderType != ""
		if knownType {
			knownType = false
			for _, th := range cfg.Eligibility.Thresholds {
				if th.ProviderType == in.ProviderType {
					knownType = true
					break
				}
			}
		}
		// Blank AND unrecognized (typo/corrupt data) both route to a human
		// -- evalEligibility now decides INELIGIBLE on an unrecognized type,
		// which is the right call for a human who explicitly submitted it,
		// but auto-adjudication shouldn't fire a closure letter off what
		// could just be a data-entry error it has no way to tell apart.
		if !knownType {
			missing = append(missing, "provider_type")
		}
	}
	needsContracted := false
	for _, th := range cfg.Eligibility.Thresholds {
		if th.Contracted != nil {
			needsContracted = true
			break
		}
	}
	if needsContracted && in.Contracted == nil {
		missing = append(missing, "contracted")
	}
	if cfg.Eligibility.FilingWindowMonths > 0 && in.FinalDeterminationAt == "" {
		missing = append(missing, "final_determination_at")
	}
	return missing
}

// deriveEligibilityInput assembles the input from the case row and the
// case's analyzed documents (doc-intel persists ctx including normalized
// fields into public.doc_analysis.result). Details jsonb wins over
// document guesses — a value a human put on the case beats an extraction.
func (s *server) deriveEligibilityInput(r *http.Request, tenant, caseID string) (eligibilityInput, error) {
	tbl := sanitizeTenant(tenant)
	var in eligibilityInput
	var details []byte
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(disputed_amount_cents,0), coalesce(details,'{}'::jsonb)
		 FROM tenant_%s.cases WHERE id=$1`, tbl), caseID).Scan(&in.DisputedAmountCents, &details)
	if err != nil {
		return in, err
	}
	var d map[string]any
	_ = json.Unmarshal(details, &d)
	if v, _ := d["provider_type"].(string); v != "" {
		in.ProviderType = v
	}
	if v, ok := d["contracted"].(bool); ok {
		in.Contracted = &v
	}
	if v, _ := d["final_determination_at"].(string); v != "" {
		in.FinalDeterminationAt = v
	}
	if v, ok := d["aor_valid"].(bool); ok {
		in.AORValid = &v
	}
	if v, _ := d["service_date"].(string); v != "" {
		in.ServiceDate = v
	}
	if v, _ := d["dispute_type"].(string); v != "" {
		in.DisputeType = v
	}
	if arr, ok := d["eligibility_flags"].([]any); ok {
		for _, f := range arr {
			if s2, ok := f.(string); ok {
				in.Flags = append(in.Flags, s2)
			}
		}
	}
	// Document-derived fallbacks: with MULTIPLE EOBs/determinations on one
	// dispute (the common case — a claim bundle), the filing window runs from
	// the LAST EOB/determination date ("proof of timeliness: last_eob_date"
	// in program rules), not from whichever document happened to be analyzed
	// most recently. Take the max date across every analyzed EOB and
	// determination letter; ISO dates order lexicographically.
	if in.FinalDeterminationAt == "" {
		_ = s.db.QueryRow(r.Context(), `
			SELECT coalesce(MAX(v), '') FROM (
				SELECT NULLIF(result->'normalized'->>'determination_date','') AS v
				FROM public.doc_analysis
				WHERE tenant=$1 AND case_id=$2 AND status LIKE 'ANALYZED%'
				  AND doc_type IN ('eob','determination_letter')
				UNION ALL
				SELECT NULLIF(result->'normalized'->>'service_date','')
				FROM public.doc_analysis
				WHERE tenant=$1 AND case_id=$2 AND status LIKE 'ANALYZED%'
				  AND doc_type IN ('eob','determination_letter')
			) dates WHERE v IS NOT NULL`, tenant, caseID).Scan(&in.FinalDeterminationAt)
	}
	// Earliest analyzed date of service, for the program-start floor. The
	// earliest is the conservative one: a single pre-program service line
	// makes that part of the dispute out of scope.
	if in.ServiceDate == "" {
		_ = s.db.QueryRow(r.Context(), `
			SELECT coalesce(MIN(NULLIF(result->'normalized'->>'service_date','')), '')
			FROM public.doc_analysis
			WHERE tenant=$1 AND case_id=$2 AND status LIKE 'ANALYZED%'
			  AND doc_type IN ('idr_claim','eob')`, tenant, caseID).Scan(&in.ServiceDate)
	}
	return in, nil
}

// persistEligibilityOutcome records the review and applies the same side
// effects as the manual endpoint (status moves, AOR notify, workflow signal,
// activity + audit). One helper, both callers.
func (s *server) persistEligibilityOutcome(r *http.Request, tenant, caseID, decidedBy,
	result, reason string, evidence map[string]any, opts eligibilityPersistOpts) string {
	ev, _ := json.Marshal(evidence)
	var reviewID string
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.eligibility_reviews (tenant, case_id, result, reason, evidence, decided_by, rule_version, borderline, override, override_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		tenant, caseID, result, reason, ev, decidedBy,
		opts.ruleVersion, opts.borderline, opts.override, opts.overrideReason).Scan(&reviewID)
	if opts.borderline {
		s.notify(r, tenant, "role:PM", "ELIGIBILITY_BORDERLINE",
			fmt.Sprintf("Case %s: borderline eligibility decision (%s) — PM second review required: %s",
				caseID, result, strings.Join(opts.borderlineReasons, "; ")), "#/cases/"+caseID)
	}
	if opts.override {
		s.notify(r, tenant, "*", "ELIGIBILITY_OVERRIDE",
			fmt.Sprintf("Case %s: INELIGIBLE overridden to ELIGIBLE by %s — reason: %s",
				caseID, decidedBy, opts.overrideReason), "#/cases/"+caseID)
	}

	switch result {
	case "ELIGIBLE":
		s.tellRespondentOpened(r.Context(), tenant, caseID) // the other side is in the dispute now
	case "INELIGIBLE":
		s.tellRespondentClosed(r.Context(), tenant, caseID) // it was told of it: tell it it ended
	}
	switch result {
	case "HOLD_AOR":
		_, _ = s.db.Exec(r.Context(),
			fmt.Sprintf(`UPDATE tenant_%s.cases SET internal_status='Hold', agency_status='Other', updated_at=now() WHERE id=$1`, sanitizeTenant(tenant)), caseID)
		s.notify(r, tenant, "role:ATTORNEY", "AOR_REVIEW", fmt.Sprintf("Case %s: AOR flagged invalid — please confirm (case page, Eligibility review)", caseID), "#/cases/"+caseID)
	case "INELIGIBLE":
		// Nothing to set here any more: "Provider Closure Letter Issued" is a
		// fact about a letter, so the status is taken when the Ineligible
		// Dispute letter has passed QA and gone out (afterLetterSent). The
		// case is frozen meanwhile by the review itself (caselock.go).
		s.notify(r, tenant, "role:CASE_MANAGER", "ELIGIBILITY_INELIGIBLE",
			fmt.Sprintf("Case %s: found ineligible (%s) — send the Ineligible Dispute letter through QA; the case closes when it has been sent", caseID, orDash(reason)), "#/cases/"+caseID)
	}
	var caseNumber string
	if err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber); err == nil {
		wfID := fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
		_ = s.tc.SignalWorkflow(r.Context(), wfID, "", "ELIGIBILITY_RESULT", map[string]any{"result": result, "reason": reason})
	}
	// The SOP has the Reviewer tell the PM and the Coder the result.
	reasonSuffix := ""
	if reason != "" {
		reasonSuffix = " — " + strings.ReplaceAll(reason, "_", " ")
	}
	for _, role := range []string{"PM", "CODER"} {
		s.notify(r, tenant, "role:"+role, "ELIGIBILITY_RESULT",
			fmt.Sprintf("Case %s: eligibility %s%s", orDash(caseNumber), result, reasonSuffix), "#/cases/"+caseID)
	}
	if caseNumber != "" {
		s.fileEligibilityChecklist(r, tenant, caseID, caseNumber, result, reason, decidedBy, evidence)
	}
	s.logActivity(r.Context(), tenant, caseID, "ELIGIBILITY_REVIEW",
		fmt.Sprintf("Eligibility %s%s — evidence recorded (review %s, by %s)", result, orDash(" — "+reason), reviewID, decidedBy))
	s.logAudit(r.Context(), tenant, caseID, "ELIGIBILITY_DECIDED", map[string]any{
		"by": decidedBy, "result": result, "reason": reason, "review_id": reviewID,
		"rule_version": opts.ruleVersion, "borderline": opts.borderline,
		"override": opts.override, "override_reason": opts.overrideReason,
	})
	return reviewID
}

// autoEligibility (Lever 1): derive -> decide-if-complete -> persist.
// Returns NEEDS_HUMAN with the missing input list when the rules can't be
// fully evaluated from platform data — the case then surfaces in the human
// eligibility queue with the blanks already enumerated, which is itself a
// minutes-per-touch saving.
func (s *server) autoEligibility(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: the eligibility review is done by the Reviewer"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	if !s.stepGuard(w, r, tenant, id, "eligibility") {
		return
	}
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		http.Error(w, `{"error":"no program rules for tenant"}`, http.StatusBadRequest)
		return
	}
	in, err := s.deriveEligibilityInput(r, tenant, id)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// A date read off a document that is not a real calendar date ("2026-02-30")
	// would make evalEligibility skip the rule that needs it -- the filing
	// window or the service-date floor -- and decide without it. That is a
	// question for a person, not a pass.
	if msg := invalidEligibilityInput(in); msg != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"result": "NEEDS_HUMAN", "missing": []string{strings.SplitN(msg, " ", 2)[0]},
			"detail": "a date read from the documents is not a valid calendar date (" + msg + "): enter it by hand",
		})
		return
	}
	if missing := missingEligibilityInputs(cfg, in); len(missing) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"result": "NEEDS_HUMAN", "missing": missing,
			"detail": "rule-required inputs are not derivable from the case record or analyzed documents",
		})
		return
	}
	result, reason, evidence := evalEligibility(cfg, in)
	evidence["auto_inputs"] = in
	bl := eligibilityBorderline(cfg, in, evidence)
	if conflicts := s.eligibilityConflicts(r, tenant, id); len(conflicts) > 0 {
		evidence["document_conflicts"] = conflicts
		bl = append(bl, conflicts...)
	}
	if inferred := s.inferredInputs(r, tenant, id); len(inferred) > 0 {
		evidence["inferred_inputs"] = inferred
		bl = append(bl, inferred...)
	}
	if len(bl) > 0 {
		evidence["borderline"] = true
		evidence["borderline_reasons"] = bl
	}
	reviewID := s.persistEligibilityOutcome(r, tenant, id,
		"auto: case+document data", result, reason, evidence, eligibilityPersistOpts{
			ruleVersion: eligibilityRuleVersion(cfg),
			borderline:  len(bl) > 0, borderlineReasons: bl,
		})
	writeJSON(w, http.StatusOK, map[string]any{
		"review_id": reviewID, "result": result, "reason": reason,
		"evidence": evidence, "auto": true,
	})
}

// --- triage lanes + SLA (listCases enrichment) ----------------------------

// triageLaneSQL computes AUTO_REVIEW | STANDARD | COMPLEX from signals the
// platform already tracks. COMPLEX first (batched, duplicated, escalated,
// or high-materiality), then AUTO_REVIEW (everything needed for a one-click
// confirm is present and nothing is on fire), else STANDARD.
const triageLaneSQL = `
	CASE
	  WHEN c.batch_id IS NOT NULL OR c.duplicate_of IS NOT NULL
	       OR coalesce(c.disputed_amount_cents,0) >= 10000000
	       OR esc.case_id IS NOT NULL
	  THEN 'COMPLEX'
	  WHEN coalesce(c.qpa_cents,0) > 0 AND coalesce(c.disputed_amount_cents,0) > 0
	  THEN 'AUTO_REVIEW'
	  ELSE 'STANDARD'
	END`

// slaDaysSQL: 30 statutory business days ≈ 42 calendar days (the NG
// platform computes exact business days against its holidays table; the
// original uses the conservative calendar approximation).
const slaDaysSQL = `42 - GREATEST(0, (CURRENT_DATE - c.opened_at::date))`

// eligibilityProviderTypes lists the provider types the program's threshold
// matrix knows, in configured order.
func eligibilityProviderTypes(cfg *ProgramConfig) []string {
	var out []string
	for _, th := range cfg.Eligibility.Thresholds {
		if !contains(out, th.ProviderType) {
			out = append(out, th.ProviderType)
		}
	}
	return out
}

// eligibilityConflicts returns, as sentences, the places where an analyzed
// document disagrees with a value the case already carries. doc-intel keeps
// the first value it reads and records every later disagreement in
// details.eligibility_conflicts; nothing is overwritten.
func (s *server) eligibilityConflicts(r *http.Request, tenant, caseID string) []string {
	var raw []byte
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(details->'eligibility_conflicts','[]'::jsonb) FROM tenant_%s.cases WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&raw); err != nil {
		return nil
	}
	return conflictSentences(raw)
}

// inferredInputs returns a sentence for each eligibility input the document
// reader worked out rather than read (today: the provider type, on a
// document with no place-of-service code). The decision still stands; the
// sentence makes it a borderline one, so the PM takes a second look.
func (s *server) inferredInputs(r *http.Request, tenant, caseID string) []string {
	var basis string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(details->>'provider_type_basis','') FROM tenant_%s.cases WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&basis)
	if strings.HasPrefix(basis, "inferred") {
		return []string{"provider type was " + basis + " — confirm it before relying on the result"}
	}
	return nil
}

func conflictSentences(raw []byte) []string {
	var items []struct {
		Field    string `json:"field"`
		Case     any    `json:"case_value"`
		Document any    `json:"document_value"`
		Source   string `json:"document"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []string
	for _, it := range items {
		src := it.Source
		if src == "" {
			src = "a document"
		}
		out = append(out, fmt.Sprintf("documents disagree on %s: the case has %v, %s says %v",
			strings.ReplaceAll(it.Field, "_", " "), it.Case, src, it.Document))
	}
	return out
}
