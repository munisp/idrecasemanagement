package main

// Conversational intake — conversation-first migration, step 5.
//
// Intake stops being a form a worker must know how to fill and becomes a
// conversation the worker can have in plain language ("Capitol Bridge called,
// provider dispute, about $4,200, contact is Dana at dana@…"). The model's
// ONLY job is extraction: one bounded call per turn that returns strict JSON
// — the currently-known intake fields plus its next follow-up question.
//
// Invariants:
//  1. THE MODEL NEVER FILES — this endpoint is a stateless form-filler. It
//     returns validated fields; the human reviews them in the intake form
//     and the EXISTING createIntake endpoint does the filing (with its own
//     validation, audit, and — for programmed tenants — real case creation).
//     Nothing persists pre-filing, so there is no half-machine-written
//     record to audit; the filing itself is the record.
//  2. STRICT JSON, SERVER-VALIDATED — every extracted field passes the same
//     shape/allowlist/range checks the form enforces; model text that fails
//     validation is dropped, not trusted.
//  3. ONE BOUNDED CALL per turn, temperature 0. The conversation state lives
//     in the client (fields echo back each turn), so this endpoint is
//     idempotent and replay-safe.
//  4. GROUNDED — the model extracts only what the worker actually said;
//     unknown fields stay empty and drive the follow-up question.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// intakeFields mirrors createIntake's payload. Keep them in lockstep: this
// struct exists so the portal can hand the extracted values straight to the
// existing filing endpoint without translation.
type intakeFields struct {
	Email               string `json:"email,omitempty"`
	ContactName         string `json:"contact_name,omitempty"`
	Org                 string `json:"org,omitempty"`
	FilingPartyType     string `json:"filing_party_type,omitempty"` // PROVIDER|HEALTH_PLAN
	DisputedAmountCents int64  `json:"disputed_amount_cents,omitempty"`
	QPACents            int64  `json:"qpa_cents,omitempty"`
	Notes               string `json:"notes,omitempty"`
}

type intakeConverseReply struct {
	Fields  intakeFields `json:"fields"`
	Missing []string     `json:"missing"`
	Ready   bool         `json:"ready"`
	Reply   string       `json:"reply"`
	Model   string       `json:"model,omitempty"`
}

const intakeChatSystem = `You are the intake assistant inside a federal No Surprises Act IDRE case platform. A case worker is describing a new dispute intake in plain language. Your ONLY job is to extract structured fields and ask the next question.

OUTPUT: strict JSON only, no prose, no markdown fences:
{"fields":{"email":"","contact_name":"","org":"","filing_party_type":"","disputed_amount_cents":0,"qpa_cents":0,"notes":""},"reply":""}

RULES:
- Extract ONLY what the worker actually stated across the conversation. Never guess emails, names, or amounts. Unknown strings stay "", unknown amounts stay 0.
- filing_party_type is PROVIDER or HEALTH_PLAN only (infer from "provider dispute"/"plan filed"; else leave "").
- Amounts are integer cents ($4,200 -> 420000). qpa_cents is the qualifying payment amount; disputed_amount_cents is the amount in dispute. If only one dollar figure is mentioned and it's the disputed amount, set disputed_amount_cents.
- notes: one sentence summarizing the situation in the worker's own terms, plus anything else they mentioned (dates, claim ids, urgency).
- reply: if any of email, org/contact_name, or an amount is still unknown, ask ONE short conversational follow-up for the most important gap. If everything needed is present, reply with a one-sentence summary for the worker to confirm, ending with "File it?".
- Keep prior extracted fields stable unless the worker corrects them.`

// intakeMissing computes what still blocks filing. email is the hard floor
// (createIntake 400s without it); org/contact and amounts are what make the
// intake USEFUL, so they gate "ready" — the human can always override by
// editing the form before filing.
func intakeMissing(f intakeFields) []string {
	var m []string
	if !strings.Contains(f.Email, "@") {
		m = append(m, "contact email")
	}
	if f.Org == "" && f.ContactName == "" {
		m = append(m, "organization or contact name")
	}
	if f.DisputedAmountCents <= 0 && f.QPACents <= 0 {
		m = append(m, "disputed amount or QPA")
	}
	return m
}

// validateIntakeFields is the trust boundary: model output becomes platform
// data only through these checks. Anything malformed is silently dropped to
// its zero value — the follow-up question will ask for it again.
func validateIntakeFields(f intakeFields) intakeFields {
	out := intakeFields{
		Email:       truncate(strings.TrimSpace(f.Email), 254),
		ContactName: truncate(strings.TrimSpace(f.ContactName), 200),
		Org:         truncate(strings.TrimSpace(f.Org), 200),
		Notes:       truncate(strings.TrimSpace(f.Notes), 2000),
	}
	if !strings.Contains(out.Email, "@") {
		out.Email = ""
	}
	fpt := strings.ToUpper(strings.TrimSpace(f.FilingPartyType))
	if fpt == "PROVIDER" || fpt == "HEALTH_PLAN" {
		out.FilingPartyType = fpt
	}
	if f.DisputedAmountCents > 0 && f.DisputedAmountCents <= 1_000_000_00*100 {
		out.DisputedAmountCents = f.DisputedAmountCents
	}
	if f.QPACents > 0 && f.QPACents <= 1_000_000_00*100 {
		out.QPACents = f.QPACents
	}
	return out
}

// parseIntakeModelJSON tolerates prose around the JSON object (same brace-cut
// discipline as the action-batch parser) and validates every field.
func parseIntakeModelJSON(raw string) (intakeFields, string, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return intakeFields{}, "", fmt.Errorf("no JSON object in model output")
	}
	var out struct {
		Fields intakeFields `json:"fields"`
		Reply  string       `json:"reply"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return intakeFields{}, "", fmt.Errorf("model JSON not parseable: %w", err)
	}
	return validateIntakeFields(out.Fields), truncate(strings.TrimSpace(out.Reply), 500), nil
}

// intakeConverse handles POST /intake/converse {message, fields, history}.
// One extraction call in, one validated field-set + follow-up question out.
func (s *server) intakeConverse(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires intake staff role"}`, http.StatusForbidden)
		return
	}
	if s.cfg.CopilotEndpoint == "" {
		http.Error(w, `{"error":"intake assistant not configured (COPILOT_ENDPOINT empty)"}`, http.StatusServiceUnavailable)
		return
	}
	var in struct {
		Message string       `json:"message"`
		Fields  intakeFields `json:"fields"`
		History []string     `json:"history"` // prior worker turns, plain text
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Message) == "" {
		http.Error(w, `{"error":"message required"}`, http.StatusBadRequest)
		return
	}
	in.Message = truncate(strings.TrimSpace(in.Message), 2000)
	if len(in.History) > 20 {
		in.History = in.History[len(in.History)-20:]
	}

	var convo strings.Builder
	for _, h := range in.History {
		convo.WriteString("Worker: " + truncate(h, 500) + "\n")
	}
	convo.WriteString("Worker: " + in.Message + "\n")
	fieldsJSON, _ := json.Marshal(validateIntakeFields(in.Fields))
	user := "CURRENT EXTRACTED FIELDS (JSON, keep stable unless corrected):\n" + string(fieldsJSON) +
		"\n\nCONVERSATION SO FAR:\n" + convo.String()

	raw, err := ollamaChat(r.Context(), s.cfg.CopilotEndpoint, s.cfg.CopilotModel,
		intakeChatSystem, user, 500)
	if err != nil {
		http.Error(w, `{"error":"model unavailable — you can still use the form directly"}`, http.StatusBadGateway)
		return
	}
	fields, reply, err := parseIntakeModelJSON(raw)
	if err != nil {
		http.Error(w, `{"error":"model output not usable — rephrase or use the form directly"}`, http.StatusBadGateway)
		return
	}
	missing := intakeMissing(fields)
	if len(missing) > 0 {
		// Server wins on readiness — never let the model declare done early.
	} else if reply == "" {
		reply = "I have everything needed. Review the fields and file when ready."
	}
	writeJSON(w, http.StatusOK, intakeConverseReply{
		Fields:  fields,
		Missing: missing,
		Ready:   len(missing) == 0,
		Reply:   reply,
		Model:   s.cfg.CopilotModel,
	})
}
