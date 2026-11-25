// Author: Patrick Munis at newwave
package main

// The Responding Party Claim Rebuttal Form (the FL Plan Packet, 6.18.24) as the responding party fills it in on the
// portal (2026-11-25): who they are, whether they take part and whether they challenge eligibility (non-SMMC plans),
// the exclusions they attest to, their position, the documents they send, the certification and the claim
// worksheet. Submitted with the response (respondingSubmit), kept on the case as details.responding_rebuttal, and
// kept as a PDF under Correspondence (lettergen "rebuttal_record", no QA).

import (
	"fmt"
	"strings"
)

// rebuttalExclusions: the attestation's exclusions, as the form lists them.
var rebuttalExclusions = map[string]string{
	"interest_payment":       "This claim is related to an interest payment.",
	"prompt_payment":         "This claim is related solely to an issue of prompt payment.",
	"medicare_grievance":     "This claim is part of an internal grievance in a Medicare Managed Care Organization or a reconsideration appeal through the Medicare reconsideration process.",
	"medicaid_fair_hearing":  "This claim is a part of a Medicaid fair hearing pursued under 42 CFR §431.220 et seq.",
	"pending_court_action":   "This claim is the basis for an action pending in state or federal court.",
	"pre_2000_binding":       "This claim is subject to a binding claim dispute resolution process provided by contract entered into prior to October 1, 2000.",
	"internal_not_exhausted": "This claim is subject to an internal dispute resolution process provided by contract entered into or renewed on or after October 1, 2000, which process has not been exhausted.",
	"over_12_months":         "More than 12 months have elapsed since the final determination was made on this claim by the Managed Care Organization.",
	"plan_not_fl_regulated":  "This Health Plan is not regulated by the State of Florida.",
}

var rebuttalDocs = map[string]bool{"claim_forms": true, "denials_eobs": true, "final_determination": true, "contract": true,
	"medical_records": true, "coding_rules": true, "other": true}

// rebuttalProblems: what the form is missing (empty = it can be submitted). side is the responding side.
func rebuttalProblems(side string, rb map[string]any) []string {
	var out []string
	need := func(ok bool, msg string) {
		if !ok {
			out = append(out, msg)
		}
	}
	obj := func(k string) map[string]any { m, _ := rb[k].(map[string]any); return m }
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return strings.TrimSpace(v) }
	party, att, cert, pos := obj("party"), obj("attest_signature"), obj("certification"), obj("position")
	need(str(party, "legal_name") != "", "Responding party: legal name is required")
	need(str(party, "contact_person") != "", "Responding party: contact person is required")
	need(strings.Contains(str(party, "email"), "@"), "Responding party: e-mail is required")
	if side == "HEALTH_PLAN" {
		smmc, _ := rb["smmc"].(bool)
		if !smmc {
			p, c := str(rb, "participate"), str(rb, "challenge_eligibility")
			need(p == "yes" || p == "no", "Say whether the plan wishes to participate in the dispute process")
			need(c == "yes" || c == "no", "Say whether the plan challenges the eligibility of this claim")
		}
	}
	if str(rb, "challenge_eligibility") == "yes" {
		ex, _ := rb["exclusions"].([]any)
		n := 0
		for _, x := range ex {
			if k, _ := x.(string); rebuttalExclusions[k] != "" {
				n++
			}
		}
		need(n > 0, "Eligibility attestation: tick the exclusion(s) this claim meets")
		ok, _ := rb["exclusions_certified"].(bool)
		need(ok, "Eligibility attestation: certify you are authorized to complete it")
	}
	// "Signature required, even if not challenging eligibility or participating."
	need(str(att, "printed_name") != "" && str(att, "title") != "" && str(att, "signature") != "" && str(att, "date") != "",
		"Signature: printed name, title, electronic signature and date are required")
	if str(rb, "participate") != "no" {
		need(str(pos, "basis") != "", "Your position: describe the basis (question 1)")
		agreed, _ := cert["agreed"].(bool)
		need(agreed && str(cert, "printed_name") != "" && str(cert, "title") != "" && str(cert, "signature") != "" && str(cert, "date") != "",
			"Certification: tick it and enter your printed name, title, electronic signature and the date")
	}
	if docs, ok := rb["documents"].([]any); ok {
		for _, x := range docs {
			if k, _ := x.(string); !rebuttalDocs[k] {
				out = append(out, fmt.Sprintf("Supporting documentation: unknown item %q", k))
			}
		}
	}
	if ws, ok := rb["worksheet"].([]any); ok && len(ws) > 500 {
		out = append(out, "Claim worksheet: at most 500 rows")
	}
	return out
}
