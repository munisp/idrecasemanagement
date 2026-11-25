// filing_packet.go — the Florida claim dispute filing packet (Capitol Bridge), entered as a form instead of a PDF.
//
// The portal's filing form has the packet's own sections: the filing party, the provider, the provider type, the
// health plan and its type, the contract / internal-dispute / type-of-service answers, the claim count and
// aggregated amount, the claims table, the fee-estimate answer, both parties' positions, the supporting
// documentation, the eligibility attestation, the appointment of representative and the certification. It saves
// into the same case fields doc-intel fills when it reads an uploaded packet (packet_docs.py: cases.details keys
// and public.case_claims rows), so a typed packet and an uploaded one end up as the same case.
//
// Filers (the public filing page, TPAs, health plans) save once, when they file: nothing on the case is
// overwritten. Staff correct it afterwards through PUT /cases/{caseId}/filing-packet, which does overwrite.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// The packet's choices, spelled as the packet prints them (and as packet_docs.py stores them).
var (
	packetProviderTypes = []string{"Hospital", "Other Inpatient Facility", "DME/Medical Supplier", "Chiropractor", "Podiatrist",
		"Physical/Occupational/Speech Therapy", "Rural Hospital", "Physician/Medical Group", "Laboratory/Imaging", "Dentist",
		"Home Health Agency", "Transportation", "Pharmacy", "Other"}
	packetPlanTypes = []string{"Commercial HMO", "Medicaid HMO", "Prepaid Health Plan", "Exclusive Provider Organization (EPO)",
		"Major Medical Expense Health Insurance Policy (PPO)", "Medicaid", "Other"}
	packetServiceTypes = []string{"Hospital Inpatient", "Hospital Outpatient", "Professional", "Other"}
	packetExhausted    = []string{"Yes", "No", "Unknown"}
	packetDocuments    = []string{"claim_forms", "denials_eobs", "florida_license", "final_determination_letter", "contract",
		"waiver_of_payment", "notice_of_overpayment", "overpayment_response", "medical_records", "coding_rules", "other"}
	packetAttestations = []string{"interest_only", "late_payment_only", "medicare_grievance", "medicaid_fair_hearing",
		"pending_court_action", "pre_2000_binding_process", "internal_process_not_exhausted", "over_12_months", "rural_hospital"}
	// Free-text fields and their longest accepted length.
	packetText = map[string]int{
		"provider_org": 200, "provider_legal_entity": 200, "provider_trade_name": 200, "provider_tin": 20, "provider_npi": 10,
		"provider_license": 60, "provider_street": 200, "provider_city": 100, "provider_state": 20, "provider_postal": 10,
		"provider_contact": 200, "provider_alt_address": 300, "provider_phone": 40, "provider_fax": 40, "provider_email": 200,
		"provider_type_other": 100, "health_plan": 200, "health_plan_trade_name": 200, "plan_type_other": 100,
		// Not on the PDF packet: where the health plan's dispute notices go (it is the other side when a provider files).
		"health_plan_contact": 200, "health_plan_email": 200, "health_plan_phone": 40, "health_plan_naic": 20,
		"type_of_service_other": 100, "documents_other": 300,
		"attested_by_name": 200, "attested_by_title": 200, "aor_provider": 200, "aor_health_plan": 200,
		"aor_representative": 200, "aor_signed_by_name": 200, "aor_signed_by_title": 200,
		"certified_by_name": 200, "certified_by_title": 200,
		"provider_position_basis": 20000, "provider_position_references": 20000, "provider_position_rebuttal": 20000,
		"plan_position_basis": 20000, "plan_position_references": 20000, "plan_position_rebuttal": 20000,
	}
	packetDates = []string{"attested_on", "aor_signed_on", "certified_on"}
	packetBools = []string{"contracted", "fee_estimate_requested", "attestation_certified", "certification_agreed"}
	isoDate     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	tenDigits   = regexp.MustCompile(`^\d{10}$`)
)

// filingPacket is the form as the portal posts it: details by their case-field names, the aggregated amount in
// cents, and the claim rows.
type filingPacket struct {
	Details             map[string]any `json:"details"`
	DisputedAmountCents *int64         `json:"disputed_amount_cents"`
	Claims              []packetClaim  `json:"claims"`
	RemoveClaims        []string       `json:"remove_claims"` // staff corrections only
	// Files the filer attaches on the form (change log 2026-11-25 #6, #8): the packet PDF they filled in and read into
	// the form, and the appointment of representative letter on their letterhead. PDFs, base64, kept on the case.
	Attachments []filingAttachment `json:"attachments,omitempty"`
}

type filingAttachment struct {
	Kind     string `json:"kind"` // filing_packet | aor_letter
	Filename string `json:"filename"`
	DataB64  string `json:"data_b64"`
	raw      []byte
}

const maxFilingAttachmentBytes = 10 << 20

// filingAttachmentNames: what each kind is filed as on the case (folder INTAKE).
var filingAttachmentNames = map[string]string{
	"filing_packet": "Filing packet (as uploaded by the filer).pdf",
	"aor_letter":    "Appointment of representative letter.pdf",
}

// attachmentProblems decodes and checks the attachments: known kinds, one of each, PDFs within the size limit.
func (pk *filingPacket) attachmentProblems() []string {
	if pk == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for i := range pk.Attachments {
		a := &pk.Attachments[i]
		name := filingAttachmentNames[a.Kind]
		if name == "" {
			out = append(out, fmt.Sprintf("attachment %d: unknown kind %q", i+1, a.Kind))
			continue
		}
		if seen[a.Kind] {
			out = append(out, "attach one "+strings.TrimSuffix(name, ".pdf")+" only")
			continue
		}
		seen[a.Kind] = true
		raw, err := base64.StdEncoding.DecodeString(a.DataB64)
		switch {
		case err != nil || len(raw) == 0:
			out = append(out, strings.TrimSuffix(name, ".pdf")+": the file could not be read")
		case len(raw) > maxFilingAttachmentBytes:
			out = append(out, strings.TrimSuffix(name, ".pdf")+": the file is over 10 MB")
		case !bytes.HasPrefix(raw, []byte("%PDF")):
			out = append(out, strings.TrimSuffix(name, ".pdf")+": must be a PDF")
		default:
			a.raw = raw
		}
	}
	return out
}

// packetRequired are the change log's required fields (2026-11-25 #2): a filer cannot send the form without them.
var packetRequiredDocs = []string{"claim_forms", "denials_eobs", "florida_license", "final_determination_letter"}

func (pk *filingPacket) requiredProblems() []string {
	if pk == nil {
		return nil
	}
	d := pk.Details
	str := func(k string) string { v, _ := d[k].(string); return strings.TrimSpace(v) }
	var out []string
	need := func(ok bool, msg string) {
		if !ok {
			out = append(out, msg)
		}
	}
	need(str("packet_provider_type") != "" && (str("packet_provider_type") != "Other" || str("provider_type_other") != ""), "Provider type is required")
	need(str("health_plan") != "", "Health plan name is required")
	need(str("plan_type") != "" && (str("plan_type") != "Other" || str("plan_type_other") != ""), "Health plan type is required")
	need(str("type_of_service") != "" && (str("type_of_service") != "Other" || str("type_of_service_other") != ""), "Type of service is required")
	n, _ := toFloat(d["claims_submitted"])
	need(n >= 1, "Number of claims is required")
	need(pk.DisputedAmountCents != nil && *pk.DisputedAmountCents > 0, "Aggregated disputed amount is required")
	claims := 0
	for _, c := range pk.Claims {
		if strings.TrimSpace(c.ClaimNumber) != "" {
			claims++
		}
	}
	need(claims > 0, "Claim(s) information: enter at least one claim")
	docs := map[string]bool{}
	if list, ok := d["packet_documents_submitted"].([]any); ok {
		for _, x := range list {
			if v, ok := x.(string); ok {
				docs[v] = true
			}
		}
	}
	for _, k := range packetRequiredDocs {
		if !docs[k] {
			out = append(out, "Supporting documents: tick every required document (*) you are submitting")
			break
		}
	}
	agreed, _ := d["certification_agreed"].(bool)
	need(agreed && str("certified_by_name") != "" && str("certified_by_title") != "" && str("certified_on") != "",
		"Certification: tick it and enter your name, title and the date")
	return out
}

// filerPacketRejected: a filer's packet (public, TPA, health plan) must check out, carry the required fields and
// attach only readable PDFs. A filing with no packet at all (an API client) is not refused here.
func filerPacketRejected(w http.ResponseWriter, pk *filingPacket) bool {
	if pk == nil {
		return false
	}
	_, _, _, problems := pk.normalize()
	problems = append(problems, pk.requiredProblems()...)
	problems = append(problems, pk.attachmentProblems()...)
	if len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the filing packet is not complete", "problems": problems})
		return true
	}
	return false
}

type packetClaim struct {
	ClaimNumber              string `json:"claim_number"`
	PatientFirstName         string `json:"patient_first_name"`
	PatientLastName          string `json:"patient_last_name"`
	TypeOfService            string `json:"type_of_service"`
	DateOfService            string `json:"date_of_service"`
	BilledCents              *int64 `json:"billed_cents"`
	PaidCents                *int64 `json:"paid_cents"`
	DisputedCents            *int64 `json:"disputed_cents"`
	DateClaimSubmitted       string `json:"date_claim_submitted"`
	DateOfDenial             string `json:"date_of_denial"`
	DateOfFinalDetermination string `json:"date_of_final_determination"`
}

func oneOf(v string, list []string) (string, bool) {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(v), x) {
			return x, true
		}
	}
	return "", false
}

func validISODate(v string) bool {
	if !isoDate.MatchString(v) {
		return false
	}
	_, err := time.Parse("2006-01-02", v)
	return err == nil
}

// packetProgramProviderType is the program's threshold provider type from the packet's two answers -- the same rule
// as packet_docs.program_provider_type: a rural hospital outright; a hospital by the type of service; a physician
// group or dentist is physician_dentist; any other type is "other"; a hospital without a service type stays unknown.
func packetProgramProviderType(providerType, typeOfService string) string {
	s := strings.ToLower(typeOfService)
	switch providerType {
	case "":
		return ""
	case "Rural Hospital":
		return "rural_hospital"
	case "Hospital":
		if strings.Contains(s, "inpatient") {
			return "hospital_inpatient"
		}
		if strings.Contains(s, "outpatient") {
			return "hospital_outpatient"
		}
		return ""
	case "Physician/Medical Group", "Dentist":
		return "physician_dentist"
	}
	return "other"
}

// normalize checks the form and returns the details to save, the aggregated amount and the claim rows. Every
// problem is returned at once, as "field: what is wrong", so the form can show them together. Pure, so it is tested.
func (pk *filingPacket) normalize() (map[string]any, *int64, []packetClaim, []string) {
	var problems []string
	out := map[string]any{}
	d := pk.Details
	if d == nil {
		d = map[string]any{}
	}
	str := func(k string) string { v, _ := d[k].(string); return strings.TrimSpace(v) }
	for k, max := range packetText {
		v := str(k)
		if v == "" {
			continue
		}
		if len([]rune(v)) > max {
			problems = append(problems, fmt.Sprintf("%s: at most %d characters", k, max))
			continue
		}
		out[k] = v
	}
	if v, ok := out["provider_state"].(string); ok {
		st := strings.ToUpper(v)
		if st == "FLORIDA" {
			st = "FL"
		}
		if len(st) != 2 || st[0] < 'A' || st[0] > 'Z' || st[1] < 'A' || st[1] > 'Z' {
			problems = append(problems, "provider_state: a 2-letter state code")
		} else {
			out["provider_state"] = st
		}
	}
	if v, ok := out["provider_postal"].(string); ok && !validPostal(v) {
		problems = append(problems, "provider_postal: a ZIP code, 12345 or 12345-6789")
	}
	if v, ok := out["provider_npi"].(string); ok && !tenDigits.MatchString(v) {
		problems = append(problems, "provider_npi: 10 digits")
	}
	// The same rule as every other TIN taken (verifications.go); kept as the filer wrote it.
	if v, ok := out["provider_tin"].(string); ok {
		if _, msg := tinCheck(v); msg != "" {
			problems = append(problems, "provider_tin: "+msg)
		}
	}
	if v, ok := out["provider_email"].(string); ok && !validMailbox(v) {
		problems = append(problems, "provider_email: an email address")
	}
	if v, ok := out["health_plan_email"].(string); ok && !validMailbox(v) {
		problems = append(problems, "health_plan_email: an email address")
	}
	if v := strings.ToUpper(str("filing_party_type")); v != "" {
		if v != "PROVIDER" && v != "HEALTH_PLAN" {
			problems = append(problems, "filing_party_type: PROVIDER or HEALTH_PLAN")
		} else {
			out["filing_party_type"] = v
		}
	}
	choice := func(key string, list []string) string {
		v := str(key)
		if v == "" {
			return ""
		}
		c, ok := oneOf(v, list)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: one of %s", key, strings.Join(list, ", ")))
			return ""
		}
		return c
	}
	ptype := choice("packet_provider_type", packetProviderTypes)
	if ptype == "Other" && out["provider_type_other"] != nil {
		out["packet_provider_type"] = "Other: " + out["provider_type_other"].(string)
	} else if ptype != "" {
		out["packet_provider_type"] = ptype
	}
	if plan := choice("plan_type", packetPlanTypes); plan == "Other" && out["plan_type_other"] != nil {
		out["plan_type"] = out["plan_type_other"]
	} else if plan != "" {
		out["plan_type"] = plan
	}
	service := choice("type_of_service", packetServiceTypes)
	if service == "Other" && out["type_of_service_other"] != nil {
		out["type_of_service"] = out["type_of_service_other"]
	} else if service != "" {
		out["type_of_service"] = service
	}
	if v := choice("internal_dispute_exhausted", packetExhausted); v != "" {
		out["internal_dispute_exhausted"] = v
	}
	for _, k := range packetBools {
		if v, ok := d[k]; ok && v != nil {
			b, isBool := v.(bool)
			if !isBool {
				problems = append(problems, k+": true or false")
				continue
			}
			out[k] = b
		}
	}
	if v, ok := d["claims_submitted"]; ok && v != nil {
		n, isNum := v.(float64)
		if !isNum || n < 0 || n > 100000 || n != float64(int64(n)) {
			problems = append(problems, "claims_submitted: a whole number")
		} else {
			out["claims_submitted"] = int64(n)
		}
	}
	for _, k := range packetDates {
		if v := str(k); v != "" {
			if !validISODate(v) {
				problems = append(problems, k+": a date, YYYY-MM-DD")
			} else {
				out[k] = v
			}
		}
	}
	if raw, ok := d["packet_documents_submitted"]; ok && raw != nil {
		list, _ := raw.([]any)
		docs := []string{}
		for _, x := range list {
			sv, _ := x.(string)
			if c, ok := oneOf(sv, packetDocuments); ok {
				docs = append(docs, c)
			} else {
				problems = append(problems, "packet_documents_submitted: unknown document "+sv)
			}
		}
		out["packet_documents_submitted"] = docs
	}
	if raw, ok := d["packet_attestation"]; ok && raw != nil {
		m, _ := raw.(map[string]any)
		att := map[string]bool{}
		for k, v := range m {
			if _, ok := oneOf(k, packetAttestations); !ok {
				problems = append(problems, "packet_attestation: unknown criterion "+k)
				continue
			}
			b, _ := v.(bool)
			att[k] = b
		}
		out["packet_attestation"] = att
	}
	// What the packet's answers mean for the program: the threshold provider type, the line of business, the
	// report's out-of-network flag.
	if prog := packetProgramProviderType(ptype, service); prog != "" {
		out["provider_type"] = prog
	}
	if plan, _ := out["plan_type"].(string); plan != "" {
		if strings.Contains(strings.ToLower(plan), "medicaid") {
			out["line_of_business"] = "Medicaid"
		} else if _, std := oneOf(plan, packetPlanTypes); std {
			out["line_of_business"] = "Commercial"
		}
	}
	if c, ok := out["contracted"].(bool); ok {
		out["out_of_network"] = map[bool]string{true: "No", false: "Yes"}[c]
	}
	if pk.DisputedAmountCents != nil && (*pk.DisputedAmountCents < 0 || *pk.DisputedAmountCents > 1e13) {
		problems = append(problems, "disputed_amount_cents: out of range")
	}
	var claims []packetClaim
	seen := map[string]bool{}
	if len(pk.Claims) > 500 {
		problems = append(problems, "claims: at most 500 rows here; import larger sets as a spreadsheet")
	}
	for i, c := range pk.Claims {
		row := fmt.Sprintf("claims row %d", i+1)
		c.ClaimNumber = strings.TrimSpace(c.ClaimNumber)
		if c.ClaimNumber == "" {
			if c.BilledCents != nil || c.PatientLastName != "" || c.DateOfService != "" {
				problems = append(problems, row+": claim # is required")
			}
			continue
		}
		if len(c.ClaimNumber) > 60 {
			problems = append(problems, row+": claim # is too long")
			continue
		}
		if seen[c.ClaimNumber] {
			problems = append(problems, row+": claim "+c.ClaimNumber+" is listed twice")
			continue
		}
		seen[c.ClaimNumber] = true
		for _, dt := range []struct{ name, v string }{{"date of service", c.DateOfService}, {"date claim submitted", c.DateClaimSubmitted},
			{"date of denial", c.DateOfDenial}, {"date of final determination", c.DateOfFinalDetermination}} {
			if dt.v != "" && !validISODate(dt.v) {
				problems = append(problems, row+": "+dt.name+" must be YYYY-MM-DD")
			}
		}
		for _, amt := range []*int64{c.BilledCents, c.PaidCents, c.DisputedCents} {
			if amt != nil && (*amt < 0 || *amt > 1e13) {
				problems = append(problems, row+": amounts must be between $0 and $100 billion")
				break
			}
		}
		if c.DisputedCents == nil && c.BilledCents != nil && c.PaidCents != nil && *c.BilledCents >= *c.PaidCents {
			v := *c.BilledCents - *c.PaidCents
			c.DisputedCents = &v
		}
		claims = append(claims, c)
	}
	if len(problems) > 0 {
		return nil, nil, nil, problems
	}
	return out, pk.DisputedAmountCents, claims, nil
}

// applyFilingPacket saves a checked packet onto a case: the details, the aggregated amount and the claim rows. With
// overwrite false (a filer's first save) only what the case does not have yet is filled; with overwrite true (staff
// correcting it) the form's values replace the case's. Returns the details keys that were saved.
func (s *server) applyFilingPacket(ctx context.Context, tenant, caseID string, details map[string]any, disputed *int64,
	claims []packetClaim, remove []string, overwrite bool) ([]string, error) {
	tbl := sanitizeTenant(tenant)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	keys := make([]string, 0, len(details))
	for k := range details {
		keys = append(keys, k)
	}
	if len(details) > 0 {
		js, _ := json.Marshal(details)
		// jsonb || keeps the right-hand value: the form's to overwrite, the case's own to only fill.
		expr := "coalesce(details,'{}'::jsonb) || $2::jsonb"
		if !overwrite {
			expr = "$2::jsonb || coalesce(details,'{}'::jsonb)"
		}
		tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET details = %s, updated_at=now() WHERE id=$1`, tbl, expr), caseID, string(js))
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, fmt.Errorf("case not found")
		}
	}
	if disputed != nil {
		cond := ""
		if !overwrite {
			cond = " AND coalesce(disputed_amount_cents,0)=0"
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET disputed_amount_cents=$2 WHERE id=$1%s`, tbl, cond), caseID, *disputed); err != nil {
			return nil, err
		}
	}
	set := func(col string) string {
		if overwrite {
			return fmt.Sprintf("%s = EXCLUDED.%s", col, col)
		}
		return fmt.Sprintf("%s = coalesce(case_claims.%s, EXCLUDED.%s)", col, col, col)
	}
	cols := []string{"patient_first_name", "patient_last_name", "type_of_service", "date_of_service", "billed_cents", "paid_cents",
		"disputed_cents", "date_claim_submitted", "date_of_denial", "date_of_final_determination"}
	sets := make([]string, len(cols))
	for i, c := range cols {
		sets[i] = set(c)
	}
	for _, c := range claims {
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.case_claims (tenant, case_id, claim_number, patient_first_name, patient_last_name, type_of_service,
				date_of_service, billed_cents, paid_cents, disputed_cents, date_claim_submitted, date_of_denial,
				date_of_final_determination, evidence_location)
			VALUES ($1,$2,$3,nullif($4,''),nullif($5,''),nullif($6,''),nullif($7,'')::date,$8,$9,$10,
				nullif($11,'')::date,nullif($12,'')::date,nullif($13,'')::date,'filing packet form')
			ON CONFLICT (tenant, case_id, claim_number) DO UPDATE SET `+strings.Join(sets, ", "),
			tenant, caseID, c.ClaimNumber, strings.TrimSpace(c.PatientFirstName), strings.TrimSpace(c.PatientLastName),
			strings.TrimSpace(c.TypeOfService), c.DateOfService, c.BilledCents, c.PaidCents, c.DisputedCents,
			c.DateClaimSubmitted, c.DateOfDenial, c.DateOfFinalDetermination); err != nil {
			return nil, err
		}
	}
	if overwrite && len(remove) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM public.case_claims WHERE tenant=$1 AND case_id=$2 AND claim_number = ANY($3)`,
			tenant, caseID, remove); err != nil {
			return nil, err
		}
	}
	return keys, tx.Commit(ctx)
}

// otherParty is the side the dispute is against, as the packet names it, for whoever did not name it outside the
// packet: the health plan (its name and notice email) when a provider files, the provider (name, email, NPI) when a
// health plan files. Fields already given win.
func (pk *filingPacket) otherParty(rsp respondent) respondent {
	if pk == nil {
		return rsp
	}
	str := func(k string) string { v, _ := pk.Details[k].(string); return strings.TrimSpace(v) }
	first := func(vals ...string) string {
		for _, v := range vals {
			if v != "" {
				return v
			}
		}
		return ""
	}
	if rsp.Side == "PROVIDER" {
		rsp.Name = first(rsp.Name, str("provider_org"), str("provider_legal_entity"))
		rsp.Email = first(rsp.Email, str("provider_email"))
		rsp.NPI = first(rsp.NPI, str("provider_npi"))
		return rsp
	}
	rsp.Name = first(rsp.Name, str("health_plan"), str("health_plan_trade_name"))
	rsp.Email = first(rsp.Email, str("health_plan_email"))
	rsp.NAIC = first(rsp.NAIC, str("health_plan_naic"))
	return rsp
}

// otherSide is the responding side for a filing party.
func otherSide(filingParty string) string {
	if filingParty == "HEALTH_PLAN" {
		return "PROVIDER"
	}
	return "HEALTH_PLAN"
}

// packetRejected answers 400 with every problem when a filer's packet form does not check out -- before the case is
// opened, so the filer fixes it instead of filing half of it. Returns true when it answered.
func packetRejected(w http.ResponseWriter, pk *filingPacket) bool {
	if pk == nil {
		return false
	}
	if _, _, _, problems := pk.normalize(); len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the filing packet has problems", "problems": problems})
		return true
	}
	return false
}

// fileWithPacket is what the three filing paths (public, TPA, health plan) call right after the case exists: save
// the form the filer completed, fill-only. A packet that does not check out never blocks the filing -- the problems
// go on the case timeline for the Reviewer, who corrects them from the case page.
func (s *server) fileWithPacket(r *http.Request, tenant, caseID string, pk *filingPacket, by string) {
	if pk == nil {
		return
	}
	details, disputed, claims, problems := pk.normalize()
	if len(problems) > 0 {
		s.logActivity(r.Context(), tenant, caseID, "FILING_PACKET_INCOMPLETE",
			"The filing form was submitted with problems, so it was not saved: "+strings.Join(problems, "; "))
		return
	}
	keys, err := s.applyFilingPacket(r.Context(), tenant, caseID, details, disputed, claims, nil, false)
	if err != nil {
		s.logActivity(r.Context(), tenant, caseID, "FILING_PACKET_INCOMPLETE", "The filing form could not be saved; enter it from the case page.")
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "FILING_PACKET_SAVED",
		fmt.Sprintf("Filing packet entered by %s: %d field(s), %d claim(s)", by, len(keys), len(claims)))
	s.logAudit(r.Context(), tenant, caseID, "FILING_PACKET_SAVED", map[string]any{"by": by, "fields": keys, "claims": len(claims)})
	s.keepFilingCopies(r, tenant, caseID, pk, by)
}

// keepFilingCopies (change log #6, #8, #12): the attachments are filed on the case under Intake; the filer's own
// packet PDF when they uploaded one, otherwise a PDF of what they typed is generated (lettergen filing_record).
// details.packet_source says which, for the intake list.
func (s *server) keepFilingCopies(r *http.Request, tenant, caseID string, pk *filingPacket, by string) {
	ctx := r.Context()
	_ = pk.attachmentProblems() // decodes; filers were checked already
	source := "form"
	for _, a := range pk.Attachments {
		if a.raw == nil {
			continue
		}
		name := filingAttachmentNames[a.Kind]
		orig := a.Filename
		if !strings.HasSuffix(strings.ToLower(orig), ".pdf") {
			orig = name
		}
		if _, code, msg := s.storeCaseFile(ctx, tenant, caseID, "filer:"+by, orig, name, "application/pdf", a.raw, "INTAKE"); code != 0 {
			s.logActivity(ctx, tenant, caseID, "FILING_ATTACHMENT_NOT_STORED", fmt.Sprintf("%s could not be kept: %s", name, msg))
			continue
		}
		if a.Kind == "filing_packet" {
			source = "pdf"
		}
	}
	_, _ = s.db.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET details = coalesce(details,'{}'::jsonb) || jsonb_build_object('packet_source', $2::text) WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID, source)
	if source == "form" {
		var caseNumber string
		_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber)
		if err := s.enqueueLetter(r, tenant, caseID, caseNumber, "filing_record", by); err != nil {
			slog.Error("filing record", "case", caseID, "err", err)
		}
	}
}

// putFilingPacket: PUT /v1/tenants/{tenant}/cases/{caseId}/filing-packet — staff save the case's filing packet
// form, overwriting what the case had (their correction of the filer's form or of the document reading).
func (s *server) putFilingPacket(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: the filing packet is kept by the Reviewer or the PM"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var pk filingPacket
	if err := json.NewDecoder(r.Body).Decode(&pk); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	details, disputed, claims, problems := pk.normalize()
	if len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the filing packet has problems", "problems": problems})
		return
	}
	before, _, _, _ := s.readFilingState(r.Context(), tenant, caseID)
	keys, err := s.applyFilingPacket(r.Context(), tenant, caseID, details, disputed, claims, pk.RemoveClaims, true)
	if err != nil {
		if err.Error() == "case not found" {
			http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "FILING_PACKET_UPDATED",
		fmt.Sprintf("Filing packet updated by %s: %d field(s), %d claim(s)%s", displayName(p), len(keys), len(claims),
			map[bool]string{true: fmt.Sprintf(", %d claim(s) removed", len(pk.RemoveClaims)), false: ""}[len(pk.RemoveClaims) > 0]))
	after, _, _, _ := s.readFilingState(r.Context(), tenant, caseID)
	changes := diffFiling(before, after)
	// A corrected amount can cross the program's $1M escalation trigger (audit F-06): the same check as a filer's edit.
	if disputed != nil {
		s.amountRaised(r, tenant, caseID, amountOf(before.Amount), *disputed)
	}
	if len(changes) > 0 {
		s.recordChange(r.Context(), tenant, caseID, "CORRECTED", p.Subject, displayName(p), "OFFICE", changes, "", "")
	}
	s.logAudit(r.Context(), tenant, caseID, "FILING_PACKET_UPDATED", map[string]any{
		"by": p.Subject, "fields": keys, "claims": len(claims), "removed": pk.RemoveClaims, "changes": changes})
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "fields": keys, "claims": len(claims)})
}
