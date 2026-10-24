// Author: Patrick Munis at newwave
// templates_admin.go — email templates managed from the dashboard, and the
// rules for which one is used when.
//
// A program ships with its standard templates in program_rules.config. That
// row is re-applied by every deploy, so nothing a person edits can live
// there. What people change lives in three tables of its own, laid over the
// program's defaults:
//
//	corr_templates          an edited copy of a standard template, or a
//	                        template somebody added
//	corr_template_usage     "at this point in the process use that template",
//	                        optionally only for some cases
//	corr_template_versions  what a template was before each change
//
// The point in the process — the event — is the standard template's own key
// ("acceptance", "plan_notification", ...). Everything that makes a letter a
// STEP hangs off that key: the QA role that must approve it, the workflow
// signal sent when it goes out, the date it records, the deliverable it
// produces. So choosing another template for an event swaps what the letter
// SAYS and who it goes to, never what it IS: the acceptance letter is still
// the acceptance letter, still approved by the PM, whatever text it carries.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// templateEditRoles may add and change templates and the usage rules.
// Everyone else on staff reads them.
var templateEditRoles = []string{"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN"}

var templateKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{2,40}$`)

// templateEventLabels says, in the words on the screen, when each standard
// template is used. A standard template missing here is labelled by its key.
var templateEventLabels = map[string]string{
	"submission_instructions":    "Fee paid, case opens (sent automatically)",
	"estimate_cost":              "Cost estimate to the filing party",
	"estimate_followup":          "Follow-up when the estimate is not answered",
	"acceptance":                 "Dispute found eligible",
	"ineligible":                 "Dispute found ineligible",
	"dismissal":                  "Dispute dismissed",
	"withdrawal":                 "Filing party withdraws",
	"plan_notification":          "Health plan is notified of the dispute",
	"plan_opt_out":               "Health plan opts out",
	"plan_opt_out_refused":       "Opt-out refused",
	"plan_docs_notice":           "Plan's documents shared with the provider",
	"rfi":                        "Request for information",
	"documentation_not_received": "Documents not received by the deadline",
	"final_order":                "Final Order package to the Agency",
	"final_order_rationale_copy": "Rationale copy to both parties",
	"payment_reminder":           "Invoice coming due (drafted automatically)",
	"past_due_reminder":          "Invoice past due (drafted automatically)",
	"refund":                     "Case closed and fee refunded",
}

// templatePlaceholders are the tokens a template may carry, and what each
// becomes when the message is drafted.
var templatePlaceholders = []map[string]string{
	{"token": "{case_number}", "meaning": "The case number, e.g. FL26-001156"},
	{"token": "{filing_party}", "meaning": "The organization that filed the dispute"},
	{"token": "{provider_name}", "meaning": "The provider named on the dispute"},
	{"token": "{payer_name}", "meaning": "The health plan named on the dispute"},
	{"token": "{disputed_amount}", "meaning": "The amount in dispute"},
	{"token": "{today}", "meaning": "Today's date"},
	{"token": "{share_link}", "meaning": "A secure upload link, created when the message is sent"},
	{"token": "{download_link}", "meaning": "A secure download link, created when the message is sent"},
}

var templateRecipientRoles = []string{"requester", "filing_party", "provider", "health_plan", "provider_or_plan",
	"billed_party", "all_parties", "agency", "pm"}

// baseCorrTemplates is the program's standard templates, as deployed.
func (s *server) baseCorrTemplates(r *http.Request, tenant string) []CorrTemplate {
	var raw []byte
	if err := s.db.QueryRow(r.Context(),
		`SELECT config->'correspondence'->'templates' FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw); err != nil {
		return nil
	}
	var out []CorrTemplate
	_ = json.Unmarshal(raw, &out)
	return out
}

// overlayTemplates lays the dashboard's rows over the standard templates: an
// edited standard template keeps its key, QA roles and threading and takes
// the edited subject, body and recipients; an added template is appended and
// takes the program's QA gate (defaultQARole), which it can never be saved
// without. Pure, so it is tested.
func overlayTemplates(base []CorrTemplate, rows []CorrTemplate) []CorrTemplate {
	gate := defaultQARole(base)
	out := make([]CorrTemplate, 0, len(base)+len(rows))
	edited := map[string]CorrTemplate{}
	for _, row := range rows {
		edited[row.Key] = row
	}
	seen := map[string]bool{}
	for _, b := range base {
		seen[b.Key] = true
		if e, ok := edited[b.Key]; ok {
			b.Name, b.Subject, b.Body, b.To, b.CC = e.Name, e.Subject, e.Body, e.To, e.CC
			b.UpdatedBy, b.UpdatedAt, b.Edited = e.UpdatedBy, e.UpdatedAt, true
		}
		out = append(out, b) // a standard template is always active: steps depend on it
	}
	for _, row := range rows {
		if seen[row.Key] {
			continue
		}
		row.Custom, row.QARole, row.QAPreRole, row.Thread = true, gate, "", false
		out = append(out, row)
	}
	return out
}

func (s *server) templateRows(r *http.Request, tenant string) []CorrTemplate {
	rows, err := s.db.Query(r.Context(), `
		SELECT key, coalesce(name,''), subject, body, to_roles, cc_roles, active, coalesce(updated_by,''),
		       to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM public.corr_templates WHERE tenant=$1 ORDER BY key`, tenant)
	if err != nil {
		return nil // tables not migrated yet: the program's standard templates still work
	}
	defer rows.Close()
	var out []CorrTemplate
	for rows.Next() {
		var t CorrTemplate
		var to, cc []byte
		var active bool
		if rows.Scan(&t.Key, &t.Name, &t.Subject, &t.Body, &to, &cc, &active, &t.UpdatedBy, &t.UpdatedAt) != nil {
			continue
		}
		_ = json.Unmarshal(to, &t.To)
		_ = json.Unmarshal(cc, &t.CC)
		t.Inactive = !active
		out = append(out, t)
	}
	return out
}

// templateUsage is one "use this template at that point" rule.
type templateUsage struct {
	ID              int64  `json:"id"`
	Event           string `json:"event"`
	Template        string `json:"template"`
	FilingPartyType string `json:"filing_party_type"` // "" = any; PROVIDER | HEALTH_PLAN
	LineOfBusiness  string `json:"line_of_business"`  // "" = any
	UpdatedBy       string `json:"updated_by,omitempty"`
}

func (s *server) templateUsageRows(r *http.Request, tenant string) []templateUsage {
	rows, err := s.db.Query(r.Context(), `
		SELECT id, event, template, filing_party_type, line_of_business, coalesce(updated_by,'')
		FROM public.corr_template_usage WHERE tenant=$1 ORDER BY event, id`, tenant)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []templateUsage
	for rows.Next() {
		var u templateUsage
		if rows.Scan(&u.ID, &u.Event, &u.Template, &u.FilingPartyType, &u.LineOfBusiness, &u.UpdatedBy) == nil {
			out = append(out, u)
		}
	}
	return out
}

// pickUsage chooses the rule for an event and a case: the rule matching the
// most of the case's own facts wins, and a rule naming a fact the case does
// not have never applies. nil when the event has no rule that fits -- the
// standard template is used. Pure, so it is tested.
func pickUsage(rules []templateUsage, event, filingPartyType, lineOfBusiness string) *templateUsage {
	var best *templateUsage
	bestScore := -1
	for i := range rules {
		u := &rules[i]
		if u.Event != event {
			continue
		}
		score := 0
		if u.FilingPartyType != "" {
			if !strings.EqualFold(u.FilingPartyType, filingPartyType) {
				continue
			}
			score++
		}
		if u.LineOfBusiness != "" {
			if !strings.EqualFold(u.LineOfBusiness, lineOfBusiness) {
				continue
			}
			score++
		}
		if score > bestScore {
			best, bestScore = u, score
		}
	}
	return best
}

// effectiveTemplate is the template to draft from for an event on a case:
// the standard template for that event, carrying the content of whichever
// template the usage rules choose. A key that is not an event (a template
// somebody added, picked by hand) is returned as it is. nil when the key is
// unknown or the template has been retired.
func (s *server) effectiveTemplate(r *http.Request, tenant, caseID, key string) *CorrTemplate {
	all := s.corrTemplates(r, tenant)
	byKey := map[string]CorrTemplate{}
	for _, t := range all {
		byKey[t.Key] = t
	}
	base, ok := byKey[key]
	if !ok || base.Inactive {
		return nil
	}
	if base.Custom {
		return &base
	}
	var party, lob string
	if caseID != "" {
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
			`SELECT coalesce(details->>'filing_party_type',''), coalesce(details->>'line_of_business','')
			 FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&party, &lob)
	}
	if u := pickUsage(s.templateUsageRows(r, tenant), key, party, lob); u != nil {
		if chosen, ok := byKey[u.Template]; ok && !chosen.Inactive {
			base.Subject, base.Body, base.To, base.CC = chosen.Subject, chosen.Body, chosen.To, chosen.CC
			base.ContentFrom = chosen.Key
		}
	}
	return &base
}

// caseTemplates: GET /cases/{caseId}/correspondence/templates — the
// templates a letter on THIS case can be drafted from, each already carrying
// the content the usage rules choose for it. The compose screens read this,
// so the text a person sees is the text that will be drafted.
func (s *server) caseTemplates(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, caseStaffRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	out := []CorrTemplate{}
	for _, t := range s.corrTemplates(r, tenant) {
		if t.Inactive {
			continue
		}
		if eff := s.effectiveTemplate(r, tenant, caseID, t.Key); eff != nil {
			out = append(out, *eff)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// listTemplates: GET /program/templates — everything the Templates screen
// shows: the templates, the points in the process with their rules, and the
// placeholders and recipient roles a template may use.
func (s *server) listTemplates(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, caseStaffRoles...) && !hasAnyRole(p, templateEditRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	base := s.baseCorrTemplates(r, tenant)
	all := overlayTemplates(base, s.templateRows(r, tenant))
	usage := s.templateUsageRows(r, tenant)
	usedBy := map[string][]string{}
	for _, u := range usage {
		usedBy[u.Template] = append(usedBy[u.Template], u.Event)
	}
	type row struct {
		CorrTemplate
		UsedBy []string `json:"used_by"`
		When   string   `json:"when,omitempty"`
	}
	templates := make([]row, 0, len(all))
	for _, t := range all {
		rw := row{CorrTemplate: t, UsedBy: usedBy[t.Key]}
		if !t.Custom {
			rw.When = templateEventLabel(t.Key)
		}
		if rw.UsedBy == nil {
			rw.UsedBy = []string{}
		}
		templates = append(templates, rw)
	}
	type event struct {
		Event string          `json:"event"`
		Label string          `json:"label"`
		Rules []templateUsage `json:"rules"`
	}
	events := make([]event, 0, len(base))
	for _, b := range base {
		ev := event{Event: b.Key, Label: templateEventLabel(b.Key), Rules: []templateUsage{}}
		for _, u := range usage {
			if u.Event == b.Key {
				ev.Rules = append(ev.Rules, u)
			}
		}
		events = append(events, ev)
	}
	lobs := []string{}
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		lobs = cfg.FieldSchema["line_of_business"]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"templates": templates, "events": events, "placeholders": templatePlaceholders,
		"recipient_roles": templateRecipientRoles, "lines_of_business": lobs,
		"qa_role": defaultQARole(base), "can_edit": hasAnyRole(p, templateEditRoles...),
	})
}

func templateEventLabel(key string) string {
	if l := templateEventLabels[key]; l != "" {
		return l
	}
	return strings.ToUpper(key[:1]) + strings.ReplaceAll(key[1:], "_", " ")
}

type templateIn struct {
	Name    string   `json:"name"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
	To      []string `json:"to"`
	CC      []string `json:"cc"`
	Active  *bool    `json:"active"`
}

// validate trims the fields and reports the first problem, or "".
func (in *templateIn) validate() string {
	in.Name, in.Subject = strings.TrimSpace(in.Name), strings.TrimSpace(in.Subject)
	in.Body = strings.TrimSpace(strings.ReplaceAll(in.Body, "\r\n", "\n"))
	switch {
	case in.Subject == "":
		return "a template needs a subject"
	case len(in.Subject) > 300:
		return "the subject is too long (300 characters at most)"
	case in.Body == "":
		return "a template needs a body"
	case len(in.Body) > 20000:
		return "the body is too long (20,000 characters at most)"
	case len(in.Name) > 120:
		return "the name is too long (120 characters at most)"
	}
	for _, list := range [][]string{in.To, in.CC} {
		for _, role := range list {
			if !contains(templateRecipientRoles, role) {
				return fmt.Sprintf("%q is not a recipient this program knows — use one of: %s", role, strings.Join(templateRecipientRoles, ", "))
			}
		}
	}
	if len(in.To) == 0 {
		return "choose who the template goes to"
	}
	// A placeholder the system does not know stays in the letter as typed:
	// say so now, not after it has been sent.
	known := map[string]bool{}
	for _, ph := range templatePlaceholders {
		known[ph["token"]] = true
	}
	for _, tok := range regexp.MustCompile(`\{[a-z_]+\}`).FindAllString(in.Subject+" "+in.Body, -1) {
		if !known[tok] {
			return fmt.Sprintf("%s is not a placeholder the system fills in", tok)
		}
	}
	return ""
}

// putTemplate: PUT /program/templates/{key} — change a standard template's
// wording and recipients, or add or change a template of your own. The state
// before the change is kept.
func (s *server) putTemplate(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, templateEditRoles...) {
		http.Error(w, `{"error":"forbidden: templates are changed by the PM or an administrator"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	key := chi.URLParam(r, "key")
	if !templateKeyRE.MatchString(key) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a template's short name is 3–41 lowercase letters, digits or underscores, starting with a letter"})
		return
	}
	var in templateIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if msg := in.validate(); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	base := s.baseCorrTemplates(r, tenant)
	standard := false
	for _, b := range base {
		standard = standard || b.Key == key
	}
	active := in.Active == nil || *in.Active
	if !active {
		if standard {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "a standard template cannot be retired — steps in the process depend on it. Choose a different template for that step under “When to use” instead."})
			return
		}
		if events := s.templateInUse(r, tenant, key); len(events) > 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "this template is in use for: " + strings.Join(events, ", ") + ". Choose another template there first."})
			return
		}
	}
	before := s.templateSnapshot(r, tenant, key)
	toJ, _ := json.Marshal(nonNil(in.To))
	ccJ, _ := json.Marshal(nonNil(in.CC))
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.corr_templates (tenant, key, name, subject, body, to_roles, cc_roles, active, custom, updated_by, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())
		ON CONFLICT (tenant, key) DO UPDATE SET name=EXCLUDED.name, subject=EXCLUDED.subject, body=EXCLUDED.body,
			to_roles=EXCLUDED.to_roles, cc_roles=EXCLUDED.cc_roles, active=EXCLUDED.active,
			updated_by=EXCLUDED.updated_by, updated_at=now()`,
		tenant, key, in.Name, in.Subject, in.Body, toJ, ccJ, active, !standard, displayName(p)); err != nil {
		http.Error(w, `{"error":"db (template tables migrated?)"}`, http.StatusInternalServerError)
		return
	}
	action := "EDITED"
	if before == nil {
		action = "CREATED"
		if standard {
			action = "EDITED" // first edit of a standard template: "before" is the program's own text
			for _, b := range base {
				if b.Key == key {
					before = map[string]any{"subject": b.Subject, "body": b.Body, "to": b.To, "cc": b.CC, "source": "program standard"}
				}
			}
		}
	}
	s.recordTemplateVersion(r, tenant, key, action, before, displayName(p))
	s.logAudit(r.Context(), tenant, "", "TEMPLATE_"+action, map[string]any{"by": p.Subject, "template": key, "standard": standard})
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "saved": true, "standard": standard})
}

// deleteTemplate: DELETE /program/templates/{key} — remove a template you
// added, or put a standard template back to the program's own wording.
func (s *server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, templateEditRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	key := chi.URLParam(r, "key")
	standard := false
	for _, b := range s.baseCorrTemplates(r, tenant) {
		standard = standard || b.Key == key
	}
	if !standard {
		if events := s.templateInUse(r, tenant, key); len(events) > 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "this template is in use for: " + strings.Join(events, ", ") + ". Choose another template there first."})
			return
		}
	}
	before := s.templateSnapshot(r, tenant, key)
	if before == nil {
		http.Error(w, `{"error":"nothing to remove"}`, http.StatusNotFound)
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM public.corr_templates WHERE tenant=$1 AND key=$2`, tenant, key); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	action := "DELETED"
	if standard {
		action = "RESTORED_TO_STANDARD"
	}
	s.recordTemplateVersion(r, tenant, key, action, before, displayName(p))
	s.logAudit(r.Context(), tenant, "", "TEMPLATE_"+action, map[string]any{"by": p.Subject, "template": key})
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "action": action})
}

func (s *server) templateInUse(r *http.Request, tenant, key string) []string {
	var events []string
	for _, u := range s.templateUsageRows(r, tenant) {
		if u.Template == key && !contains(events, templateEventLabel(u.Event)) {
			events = append(events, templateEventLabel(u.Event))
		}
	}
	return events
}

func (s *server) templateSnapshot(r *http.Request, tenant, key string) map[string]any {
	var name, subject, body, by string
	var to, cc []byte
	var active bool
	err := s.db.QueryRow(r.Context(), `
		SELECT coalesce(name,''), subject, body, to_roles, cc_roles, active, coalesce(updated_by,'')
		FROM public.corr_templates WHERE tenant=$1 AND key=$2`, tenant, key).Scan(&name, &subject, &body, &to, &cc, &active, &by)
	if err != nil {
		return nil
	}
	var toL, ccL []string
	_ = json.Unmarshal(to, &toL)
	_ = json.Unmarshal(cc, &ccL)
	return map[string]any{"name": name, "subject": subject, "body": body, "to": toL, "cc": ccL, "active": active, "updated_by": by}
}

func (s *server) recordTemplateVersion(r *http.Request, tenant, key, action string, before map[string]any, by string) {
	snap, _ := json.Marshal(before)
	if before == nil {
		snap = []byte("null")
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.corr_template_versions (tenant, key, action, snapshot, changed_by) VALUES ($1,$2,$3,$4,$5)`,
		tenant, key, action, snap, by)
}

// templateHistory: GET /program/templates/{key}/history — each change, and
// what the template was before it.
func (s *server) templateHistory(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, caseStaffRoles...) && !hasAnyRole(p, templateEditRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, `
		SELECT id, action, snapshot AS before, changed_by, changed_at
		FROM public.corr_template_versions WHERE tenant=$1 AND key=$2 ORDER BY changed_at DESC LIMIT 50`,
		tenant, chi.URLParam(r, "key"))
	if err != nil {
		rows = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": rows})
}

// putTemplateUsage: PUT /program/templates/usage — "at this point, use that
// template", for every case or only for some. Choosing the event's own
// standard template with no condition removes the rule: that is the default.
func (s *server) putTemplateUsage(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, templateEditRoles...) {
		http.Error(w, `{"error":"forbidden: template rules are changed by the PM or an administrator"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in templateUsage
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	in.FilingPartyType = strings.ToUpper(strings.TrimSpace(in.FilingPartyType))
	in.LineOfBusiness = strings.TrimSpace(in.LineOfBusiness)
	if in.FilingPartyType != "" && in.FilingPartyType != "PROVIDER" && in.FilingPartyType != "HEALTH_PLAN" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the filer is a provider or a health plan"})
		return
	}
	base := s.baseCorrTemplates(r, tenant)
	isEvent := false
	for _, b := range base {
		isEvent = isEvent || b.Key == in.Event
	}
	if !isEvent {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that is not a point in this program's process"})
		return
	}
	var chosen *CorrTemplate
	for _, t := range overlayTemplates(base, s.templateRows(r, tenant)) {
		if t.Key == in.Template {
			cp := t
			chosen = &cp
		}
	}
	if chosen == nil || chosen.Inactive {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose a template that exists and has not been retired"})
		return
	}
	unconditional := in.FilingPartyType == "" && in.LineOfBusiness == ""
	if in.Template == in.Event && unconditional {
		_, _ = s.db.Exec(r.Context(), `
			DELETE FROM public.corr_template_usage WHERE tenant=$1 AND event=$2 AND filing_party_type='' AND line_of_business=''`,
			tenant, in.Event)
		s.logAudit(r.Context(), tenant, "", "TEMPLATE_USAGE_RESET", map[string]any{"by": p.Subject, "event": in.Event})
		writeJSON(w, http.StatusOK, map[string]any{"event": in.Event, "template": in.Event, "default": true})
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.corr_template_usage (tenant, event, template, filing_party_type, line_of_business, updated_by, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,now())
		ON CONFLICT (tenant, event, filing_party_type, line_of_business)
		DO UPDATE SET template=EXCLUDED.template, updated_by=EXCLUDED.updated_by, updated_at=now()`,
		tenant, in.Event, in.Template, in.FilingPartyType, in.LineOfBusiness, displayName(p)); err != nil {
		http.Error(w, `{"error":"db (template tables migrated?)"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TEMPLATE_USAGE_SET", map[string]any{
		"by": p.Subject, "event": in.Event, "template": in.Template,
		"filing_party_type": in.FilingPartyType, "line_of_business": in.LineOfBusiness})
	writeJSON(w, http.StatusOK, map[string]any{"event": in.Event, "template": in.Template})
}

// deleteTemplateUsage: DELETE /program/templates/usage/{id}.
func (s *server) deleteTemplateUsage(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, templateEditRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var event, template string
	err := s.db.QueryRow(r.Context(), `
		DELETE FROM public.corr_template_usage WHERE tenant=$1 AND id::text=$2 RETURNING event, template`,
		tenant, chi.URLParam(r, "id")).Scan(&event, &template)
	if errors.Is(err, pgx.ErrNoRows) || err != nil {
		http.Error(w, `{"error":"rule not found"}`, http.StatusNotFound)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TEMPLATE_USAGE_REMOVED", map[string]any{"by": p.Subject, "event": event, "template": template})
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}

// previewTemplate: POST /program/templates/preview {subject, body, case_id?}
// — the text with its placeholders filled from a real case (the most recent
// one when none is named). Links are shown as words: nothing is created.
func (s *server) previewTemplate(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, caseStaffRoles...) && !hasAnyRole(p, templateEditRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
		CaseID  string `json:"case_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	caseID := in.CaseID
	if caseID == "" {
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
			`SELECT id::text FROM tenant_%s.cases ORDER BY opened_at DESC LIMIT 1`, sanitizeTenant(tenant))).Scan(&caseID)
	}
	links := strings.NewReplacer("{share_link}", "[secure upload link]", "{download_link}", "[secure download link]")
	var caseNumber string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber)
	writeJSON(w, http.StatusOK, map[string]any{
		"subject":     links.Replace(s.renderTemplate(r, tenant, caseID, truncate(in.Subject, 300))),
		"body":        links.Replace(s.renderTemplate(r, tenant, caseID, truncate(in.Body, 20000))),
		"case_number": caseNumber,
	})
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// templateToday is today's date as letters write it.
func templateToday() string { return time.Now().Format("January 2, 2006") }
