package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
)

// The decision report (scripts/decision_reports.sql): what the reviewers write for the Review Rationale and
// Letter, the cover letter and the invoices. Each part is written by the people who own it -- the physician's
// review by the clinical reviewers, the coding review and the claims tables by the coder, the applicable law by
// the attorney -- and the Reviewer and the PM may edit all of it. lettergen builds the letters from it.
//
// The letterhead and invoice details (public.letter_settings) are per tenant: the program config carries the
// standard wording ("letters"), the table carries what an office changes, and the remit-to bank details, which
// only Finance and the tenant's administrators may see in full or change.

const (
	drSectionMax    = 20000
	drClaimsMax     = 500
	drDisallowedMax = 200
)

var (
	drAdminRoles    = []string{"TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}
	drReviewerRoles = []string{"CASE_MANAGER", "PM", "ATTORNEY"}
	drClinicalRoles = []string{"NURSE_PHYSICIAN", "DOCTOR", "NURSE", "CASE_MANAGER", "PM"}
	drCodingRoles   = []string{"CODER", "CASE_MANAGER", "PM"}
	drLegalRoles    = []string{"ATTORNEY", "CASE_MANAGER", "PM"}
	drReadRoles     = append(append([]string{}, caseStaffRoles...), "TENANT_ADMIN", "FINANCE")
)

// drSections: every section a report may carry, and who writes it.
var drSections = map[string][]string{
	// The claim dispute report (FL 408.7057).
	"claim_summary":      drReviewerRoles,
	"dispute_issue":      drReviewerRoles,
	"provider_position":  drReviewerRoles,
	"plan_position":      drReviewerRoles,
	"applicable_law":     drLegalRoles,
	"analysis":           drReviewerRoles,
	"physician_review":   drClinicalRoles,
	"coding_review":      drCodingRoles,
	"pricing_method":     drCodingRoles,
	"closing_note":       drReviewerRoles,
	"standard_of_review": drLegalRoles,
	// The cover letter.
	"case_file_summary": drReviewerRoles,
	// The external review (IRO) decision.
	"iro_program_title":    drReviewerRoles,
	"agency_case_no":       drReviewerRoles,
	"binding_statute":      drLegalRoles,
	"iro_number":           drReviewerRoles,
	"plan_reference_no":    drReviewerRoles,
	"date_received":        drReviewerRoles,
	"records_received":     drReviewerRoles,
	"review_received_from": drReviewerRoles,
	"decision_date":        drReviewerRoles,
	"dates_of_service":     drReviewerRoles,
	"denial_reason":        drReviewerRoles,
	"reviewer_credentials": drClinicalRoles,
	"determination_reason": drClinicalRoles,
	"evidence":             drClinicalRoles,
	"professional_review":  drClinicalRoles,
}

// drReviewerKinds: who may be named in the cover letter as having reviewed the claims.
var drReviewerKinds = []string{"attorney", "physician", "coding consultant", "nurse", "administrative"}

var drOutcomes = []string{"OVERTURNED", "PARTIALLY_OVERTURNED", "UPHELD"}

type drClaim struct {
	ClaimNumber        string `json:"claim_number"`
	CPT                string `json:"cpt"`
	DOS                string `json:"dos"`
	Zip                string `json:"zip"`
	BilledCents        *int64 `json:"billed_cents"`
	UCRCents           *int64 `json:"ucr_cents"`
	LesserCents        *int64 `json:"lesser_cents"`
	PaidCents          *int64 `json:"paid_cents"`
	ReimbursementCents *int64 `json:"reimbursement_cents"`
}

type drDisallowed struct {
	Reason      string `json:"reason"`
	Description string `json:"description"`
	RevenueCode string `json:"revenue_code"`
	Quantity    string `json:"quantity"`
	AmountCents *int64 `json:"amount_cents"`
}

func drCan(p principal, roles []string) bool {
	return hasAnyRole(p, roles...) || hasAnyRole(p, drAdminRoles...)
}

func drAmountOK(v *int64) bool {
	return v == nil || (*v >= 0 && *v <= maxDisputedAmountCents)
}

func drShort(s string, n int) bool { return utf8.RuneCountInString(s) <= n }

// validDrClaims checks the claims table: short text cells, amounts in range.
func validDrClaims(cs []drClaim) string {
	if len(cs) > drClaimsMax {
		return fmt.Sprintf("at most %d claims", drClaimsMax)
	}
	for i, c := range cs {
		if !drShort(c.ClaimNumber, 60) || !drShort(c.CPT, 60) || !drShort(c.DOS, 60) || !drShort(c.Zip, 20) {
			return fmt.Sprintf("claim %d: a cell is too long", i+1)
		}
		for _, v := range []*int64{c.BilledCents, c.UCRCents, c.LesserCents, c.PaidCents, c.ReimbursementCents} {
			if !drAmountOK(v) {
				return fmt.Sprintf("claim %d: an amount is out of range", i+1)
			}
		}
	}
	return ""
}

func validDrDisallowed(ds []drDisallowed) string {
	if len(ds) > drDisallowedMax {
		return fmt.Sprintf("at most %d disallowed charges", drDisallowedMax)
	}
	for i, d := range ds {
		if !drShort(d.Reason, 600) || !drShort(d.Description, 300) || !drShort(d.RevenueCode, 20) || !drShort(d.Quantity, 20) {
			return fmt.Sprintf("disallowed charge %d: a cell is too long", i+1)
		}
		if !drAmountOK(d.AmountCents) {
			return fmt.Sprintf("disallowed charge %d: the amount is out of range", i+1)
		}
	}
	return ""
}

// drOutcomeFromCase maps the recorded case outcome to the report's wording.
func drOutcomeFromCase(caseOutcome string) string {
	switch caseOutcome {
	case "Provider Full Award", "Provider Default Award":
		return "OVERTURNED"
	case "Provider Partial Award":
		return "PARTIALLY_OVERTURNED"
	case "Provider No Award":
		return "UPHELD"
	}
	return ""
}

// getDecisionReport: GET /cases/{caseId}/decision-report -- the report, what the case already says (for the
// form to start from), and which parts the caller may edit.
func (s *server) getDecisionReport(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, drReadRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var caseNumber, caseOutcome string
	var disputed, awarded *int64
	var accepted *string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT case_number, coalesce(details->>'case_outcome',''), disputed_amount_cents,
		       nullif(details->>'final_amount_awarded_cents','')::bigint,
		       coalesce(program_dates->>'date_case_accepted', program_dates->>'date_acknowledgment_letter_sent_to_provider')
		FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&caseNumber, &caseOutcome, &disputed, &awarded, &accepted); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	rows, err := s.queryRows(r, `
		SELECT report_type, outcome, awarded_cents, already_paid_cents, percent_awarded::float8 AS percent_awarded,
		       sections, claims, disallowed, reviewers, section_authors, updated_by, updated_at
		FROM public.decision_reports WHERE tenant=$1 AND case_id=$2`, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	var report any
	if len(rows) == 1 {
		report = rows[0]
	}
	// The claim lines already on the case start the claims table.
	claims, _ := s.queryRows(r, `
		SELECT claim_number, coalesce(cpt,'') AS cpt, coalesce(date_of_service::text,'') AS dos,
		       billed_cents, paid_cents
		FROM public.case_claims WHERE tenant=$1 AND case_id=$2 ORDER BY claim_number LIMIT 500`, tenant, caseID)
	var paid int64
	for _, c := range claims {
		if v, ok := c["paid_cents"].(int64); ok {
			paid += v
		}
	}
	canEdit := map[string]bool{
		"decision": drCan(p, append(append([]string{}, drReviewerRoles...), drLegalRoles...)),
		"claims":   drCan(p, drCodingRoles),
	}
	for k, roles := range drSections {
		canEdit[k] = drCan(p, roles)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"report": report,
		"case": map[string]any{
			"case_number": caseNumber, "case_outcome": caseOutcome, "disputed_cents": disputed,
			"awarded_cents": awarded, "already_paid_cents": paid, "accepted_on": accepted,
			"outcome": drOutcomeFromCase(caseOutcome), "claims": claims,
		},
		"can_edit": canEdit, "reviewer_kinds": drReviewerKinds,
	})
}

// putDecisionReport: PUT /cases/{caseId}/decision-report -- saves the parts given; parts left out keep their
// value. Each part is checked against who may write it.
func (s *server) putDecisionReport(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil || len(in) == 0 {
		http.Error(w, `{"error":"a JSON object with the parts to save is required"}`, http.StatusBadRequest)
		return
	}
	bad := func(msg string) { writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg}) }
	forbid := func(part string) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: you may not edit " + strings.ReplaceAll(part, "_", " ")})
	}
	decisionRoles := append(append([]string{}, drReviewerRoles...), drLegalRoles...)
	var exists bool
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT true FROM tenant_%s.cases WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&exists); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}

	// Parts given: set = the column's new value (nil = clear it).
	type part struct {
		set bool
		val any
	}
	cols := map[string]*part{}
	var changed []string
	for k, v := range in {
		switch k {
		case "report_type":
			if !drCan(p, drReviewerRoles) {
				forbid(k)
				return
			}
			var t string
			if json.Unmarshal(v, &t) != nil || (t != "CLAIM_DISPUTE" && t != "IRO") {
				bad("report_type must be CLAIM_DISPUTE or IRO")
				return
			}
			cols[k] = &part{true, t}
		case "outcome":
			if !drCan(p, decisionRoles) {
				forbid(k)
				return
			}
			var o *string
			if json.Unmarshal(v, &o) != nil || (o != nil && *o != "" && !contains(drOutcomes, *o)) {
				bad("outcome must be OVERTURNED, PARTIALLY_OVERTURNED or UPHELD")
				return
			}
			if o != nil && *o == "" {
				o = nil
			}
			cols[k] = &part{true, o}
		case "awarded_cents", "already_paid_cents":
			if !drCan(p, decisionRoles) {
				forbid(k)
				return
			}
			var n *int64
			if json.Unmarshal(v, &n) != nil || !drAmountOK(n) {
				bad(k + " must be a whole number of cents, not negative")
				return
			}
			cols[k] = &part{true, n}
		case "percent_awarded":
			if !drCan(p, decisionRoles) {
				forbid(k)
				return
			}
			var f *float64
			if json.Unmarshal(v, &f) != nil || (f != nil && (*f < 0 || *f > 100)) {
				bad("percent_awarded must be between 0 and 100")
				return
			}
			cols[k] = &part{true, f}
		case "reviewers":
			if !drCan(p, drReviewerRoles) {
				forbid(k)
				return
			}
			var rv []string
			if json.Unmarshal(v, &rv) != nil {
				bad("reviewers must be a list")
				return
			}
			for _, x := range rv {
				if !contains(drReviewerKinds, x) {
					bad("reviewers may be: " + strings.Join(drReviewerKinds, ", "))
					return
				}
			}
			if rv == nil {
				rv = []string{}
			}
			b, _ := json.Marshal(rv)
			cols[k] = &part{true, string(b)}
		case "claims":
			if !drCan(p, drCodingRoles) {
				forbid(k)
				return
			}
			var cs []drClaim
			if json.Unmarshal(v, &cs) != nil {
				bad("claims must be a list of claim rows")
				return
			}
			if msg := validDrClaims(cs); msg != "" {
				bad(msg)
				return
			}
			if cs == nil {
				cs = []drClaim{}
			}
			b, _ := json.Marshal(cs)
			cols[k] = &part{true, string(b)}
		case "disallowed":
			if !drCan(p, drCodingRoles) {
				forbid(k)
				return
			}
			var ds []drDisallowed
			if json.Unmarshal(v, &ds) != nil {
				bad("disallowed must be a list of disallowed-charge rows")
				return
			}
			if msg := validDrDisallowed(ds); msg != "" {
				bad(msg)
				return
			}
			if ds == nil {
				ds = []drDisallowed{}
			}
			b, _ := json.Marshal(ds)
			cols[k] = &part{true, string(b)}
		case "sections":
			var secs map[string]string
			if json.Unmarshal(v, &secs) != nil {
				bad("sections must be an object of text")
				return
			}
			for sk, sv := range secs {
				roles, ok := drSections[sk]
				if !ok {
					bad("unknown section " + sk)
					return
				}
				if !drCan(p, roles) {
					forbid(sk)
					return
				}
				if !drShort(sv, drSectionMax) {
					bad(fmt.Sprintf("%s is longer than %d characters", sk, drSectionMax))
					return
				}
				secs[sk] = strings.TrimSpace(sv)
				changed = append(changed, sk)
			}
			b, _ := json.Marshal(secs)
			cols[k] = &part{true, string(b)}
			continue
		default:
			bad("unknown part " + k)
			return
		}
		changed = append(changed, k)
	}

	// Who wrote each part last, shown beside it on the form.
	authors := map[string]string{}
	for _, k := range changed {
		authors[k] = displayName(p)
	}
	authorsJ, _ := json.Marshal(authors)
	get := func(k string) (bool, any) {
		if c, ok := cols[k]; ok {
			return c.set, c.val
		}
		return false, nil
	}
	setType, vType := get("report_type")
	setOut, vOut := get("outcome")
	setAw, vAw := get("awarded_cents")
	setPd, vPd := get("already_paid_cents")
	setPct, vPct := get("percent_awarded")
	setSec, vSec := get("sections")
	setCl, vCl := get("claims")
	setDis, vDis := get("disallowed")
	setRev, vRev := get("reviewers")
	if vSec == nil {
		vSec = "{}"
	}
	// Sections merge key by key (two people editing different sections never undo each other); the tables and
	// the decision are replaced whole.
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.decision_reports AS d (tenant, case_id, report_type, outcome, awarded_cents, already_paid_cents,
		       percent_awarded, sections, claims, disallowed, reviewers, section_authors, updated_by)
		VALUES ($1,$2, coalesce($4::text,'CLAIM_DISPUTE'), $6::text, $8::bigint, $10::bigint, $12::numeric, $14::jsonb,
		        coalesce($16::jsonb,'[]'), coalesce($18::jsonb,'[]'), coalesce($20::jsonb,'[]'), $21::jsonb, $22)
		ON CONFLICT (tenant, case_id) DO UPDATE SET
		  report_type        = CASE WHEN $3 THEN $4::text ELSE d.report_type END,
		  outcome            = CASE WHEN $5 THEN $6::text ELSE d.outcome END,
		  awarded_cents      = CASE WHEN $7 THEN $8::bigint ELSE d.awarded_cents END,
		  already_paid_cents = CASE WHEN $9 THEN $10::bigint ELSE d.already_paid_cents END,
		  percent_awarded    = CASE WHEN $11 THEN $12::numeric ELSE d.percent_awarded END,
		  sections           = CASE WHEN $13 THEN d.sections || $14::jsonb ELSE d.sections END,
		  claims             = CASE WHEN $15 THEN $16::jsonb ELSE d.claims END,
		  disallowed         = CASE WHEN $17 THEN $18::jsonb ELSE d.disallowed END,
		  reviewers          = CASE WHEN $19 THEN $20::jsonb ELSE d.reviewers END,
		  section_authors    = d.section_authors || $21::jsonb,
		  updated_by = $22, updated_at = now()`,
		tenant, caseID, setType, vType, setOut, vOut, setAw, vAw, setPd, vPd, setPct, vPct,
		setSec, vSec, setCl, vCl, setDis, vDis, setRev, vRev, string(authorsJ), displayName(p))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "DECISION_REPORT_UPDATED",
		"Decision report updated by "+displayName(p)+": "+strings.ReplaceAll(strings.Join(changed, ", "), "_", " "))
	s.logAudit(r.Context(), tenant, caseID, "DECISION_REPORT_UPDATED", map[string]any{"by": p.Subject, "parts": changed})
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "parts": changed})
}

// ---- Letterhead and invoice details ----------------------------------------------------------------------------

var (
	// Bank details: Finance and the tenant's administrators. The PM issues invoices, so it does not also set where
	// they are paid.
	lsRemitRoles = []string{"FINANCE", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}
	lsEditRoles  = []string{"PM", "FINANCE", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}
	lsDigitsRE   = regexp.MustCompile(`^\d+$`)
)

// lsFields: the settings an office may change, by group. Text only, each short.
var lsFields = map[string][]string{
	"letterhead": {"org_name", "org_short", "program_name", "program_title", "contractor_line", "state", "contact_email", "contact_phone"},
	"agency":     {"email_salutation", "salutation", "line1", "line2", "line3", "line4"},
	"invoice":    {"payment_note", "bank_changed_notice", "account_use_note"},
	"remit":      {"payee", "bank", "routing", "account", "remittance_email"},
}

// abaOK: the ABA routing number checksum.
func abaOK(n string) bool {
	if len(n) != 9 || !lsDigitsRE.MatchString(n) {
		return false
	}
	d := func(i int) int { return int(n[i] - '0') }
	return (3*(d(0)+d(3)+d(6))+7*(d(1)+d(4)+d(7))+(d(2)+d(5)+d(8)))%10 == 0
}

func maskTail(v string) string {
	if len(v) <= 4 {
		return strings.Repeat("•", len(v))
	}
	return strings.Repeat("•", len(v)-4) + v[len(v)-4:]
}

// getLetterSettings: GET /letter-settings -- the office's letterhead and invoice details over the program's
// standard wording. Bank numbers are masked unless the caller may change them.
func (s *server) getLetterSettings(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, drReadRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	settings := map[string]map[string]string{}
	var raw []byte
	var by string
	var at any
	_ = s.db.QueryRow(r.Context(), `SELECT settings, updated_by, updated_at FROM public.letter_settings WHERE tenant=$1`, tenant).
		Scan(&raw, &by, &at)
	_ = json.Unmarshal(raw, &settings)
	if !hasAnyRole(p, lsRemitRoles...) {
		if rm := settings["remit"]; rm != nil {
			for _, k := range []string{"routing", "account"} {
				if rm[k] != "" {
					rm[k] = maskTail(rm[k])
				}
			}
		}
	}
	var defaults json.RawMessage
	if d, ok := progRaw(r, s, tenant, "letters"); ok {
		defaults = d
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "defaults": defaults, "fields": lsFields,
		"updated_by": by, "updated_at": at,
		"can_edit": hasAnyRole(p, lsEditRoles...), "can_edit_remit": hasAnyRole(p, lsRemitRoles...)})
}

// putLetterSettings: PUT /letter-settings {"letterhead": {...}, "agency": {...}, "invoice": {...}, "remit": {...}}
// Groups given replace that group; groups left out are kept. An empty value removes the field (the program's
// standard wording applies again).
func (s *server) putLetterSettings(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, lsEditRoles...) {
		http.Error(w, `{"error":"forbidden: letterhead details are changed by the PM, Finance or an administrator"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in map[string]map[string]string
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if json.Unmarshal(raw, &in) != nil || len(in) == 0 {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	bad := func(msg string) { writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg}) }
	clean := map[string]map[string]string{}
	for g, vals := range in {
		allowed, ok := lsFields[g]
		if !ok {
			bad("unknown group " + g)
			return
		}
		if g == "remit" && !hasAnyRole(p, lsRemitRoles...) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: bank details are changed by Finance or an administrator"})
			return
		}
		out := map[string]string{}
		for k, v := range vals {
			if !contains(allowed, k) {
				bad("unknown field " + g + "." + k)
				return
			}
			v = strings.TrimSpace(v)
			if !drShort(v, 300) {
				bad(g + "." + k + " is too long")
				return
			}
			if v == "" {
				continue
			}
			if g == "remit" && (k == "routing" || k == "account") && strings.Contains(v, "•") {
				out[k] = v // the masked number shown on the page, sent back unchanged: resolved to the stored one below
				continue
			}
			switch {
			case g == "remit" && k == "routing" && !abaOK(v):
				bad("the routing number is not a valid 9-digit ABA routing number")
				return
			case g == "remit" && k == "account" && (!lsDigitsRE.MatchString(v) || len(v) < 4 || len(v) > 17):
				bad("the account number is 4 to 17 digits")
				return
			case k == "remittance_email" || k == "contact_email":
				if !strings.Contains(v, "@") || strings.ContainsAny(v, " ,;") {
					bad(g + "." + k + " must be one email address")
					return
				}
			}
			out[k] = v
		}
		clean[g] = out
	}
	// A masked value sent back unchanged keeps the stored number.
	if rm := clean["remit"]; rm != nil {
		var cur map[string]map[string]string
		var curRaw []byte
		_ = s.db.QueryRow(r.Context(), `SELECT settings FROM public.letter_settings WHERE tenant=$1`, tenant).Scan(&curRaw)
		_ = json.Unmarshal(curRaw, &cur)
		for _, k := range []string{"routing", "account"} {
			if strings.Contains(rm[k], "•") {
				if cur["remit"] != nil && cur["remit"][k] != "" && maskTail(cur["remit"][k]) == rm[k] {
					rm[k] = cur["remit"][k]
				} else {
					bad("enter the full " + k + " number")
					return
				}
			}
		}
	}
	b, _ := json.Marshal(clean)
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.letter_settings (tenant, settings, updated_by) VALUES ($1,$2::jsonb,$3)
		ON CONFLICT (tenant) DO UPDATE SET settings = public.letter_settings.settings || $2::jsonb,
		  updated_by=$3, updated_at=now()`, tenant, string(b), displayName(p)); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	groups := make([]string, 0, len(clean))
	for g := range clean {
		groups = append(groups, g)
	}
	// The audit names what changed, never the bank numbers themselves.
	s.logAudit(r.Context(), tenant, "", "LETTER_SETTINGS_UPDATED", map[string]any{"by": p.Subject, "groups": groups})
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "groups": groups})
}
