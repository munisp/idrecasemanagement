package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// letterVars: the office's own names, as every email template prints them -- {org_name}, {org_short},
// {contact_email}, {contact_phone}, {agency_salutation}, {program_name}. They come from the program's standard
// wording (config "letters") overlaid with what the office set under Letterhead & invoice (public.letter_settings),
// so a new contact address or a new Agency contact is an edit there, not a change to every template.
func (s *server) letterVars(r *http.Request, tenant string) map[string]string {
	var def struct {
		OrgName      string `json:"org_name"`
		OrgShort     string `json:"org_short"`
		ContactEmail string `json:"contact_email"`
		ContactPhone string `json:"contact_phone"`
		ProgramName  string `json:"program_name"`
		AgencyEmail  string `json:"agency_email_salutation"`
	}
	if raw, ok := progRaw(r, s, tenant, "letters"); ok {
		_ = json.Unmarshal(raw, &def)
	}
	var set map[string]map[string]string
	var raw []byte
	_ = s.db.QueryRow(r.Context(), `SELECT settings FROM public.letter_settings WHERE tenant=$1`, tenant).Scan(&raw)
	_ = json.Unmarshal(raw, &set)
	pick := func(group, key, fallback string) string {
		if v := strings.TrimSpace(set[group][key]); v != "" {
			return v
		}
		return fallback
	}
	vars := s.customVars(r, tenant)
	org := pick("letterhead", "org_name", def.OrgName)
	short := pick("letterhead", "org_short", def.OrgShort)
	if short == "" {
		short = strings.TrimSuffix(org, " LLC")
	}
	out := map[string]string{}
	for k, v := range vars {
		out["{"+k+"}"] = v
	}
	sal := pick("agency", "email_salutation", def.AgencyEmail)
	if sal == "" && vars["agency_contacts"] != "" {
		sal = "Dear " + vars["agency_contacts"] + ","
	}
	for k, v := range map[string]string{
		"{org_name}":          org,
		"{org_short}":         short,
		"{contact_email}":     pick("letterhead", "contact_email", def.ContactEmail),
		"{contact_phone}":     pick("letterhead", "contact_phone", def.ContactPhone),
		"{program_name}":      pick("letterhead", "program_name", def.ProgramName),
		"{agency_salutation}": sal,
	} {
		out[k] = v
	}
	return out
}

// Template variables: named values the PM and admins set once for the whole tenant -- the Agency's contacts, a
// director's name, an address -- and use as {name} in any template. The program ships some ("letters" ->
// "variables" in its config); the office's own live in letter_settings under "variables" and win.

var (
	tvEditRoles = []string{"PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}
	tvNameRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,40}$`)
	tvMax       = 100
)

// tvBuiltin: the office values every template can use, where each is kept (letter_settings group and field),
// and what it means on the screen.
var tvBuiltin = []struct{ name, group, field, meaning string }{
	{"org_name", "letterhead", "org_name", "Your organization's name"},
	{"org_short", "letterhead", "org_short", "Its short name, used in signatures"},
	{"contact_email", "letterhead", "contact_email", "The contact email"},
	{"contact_phone", "letterhead", "contact_phone", "The contact phone"},
	{"program_name", "letterhead", "program_name", "The program's name"},
	{"agency_salutation", "agency", "email_salutation", "How emails to the Agency open (blank: Dear {agency_contacts},)"},
}

// tvReserved: tokens filled from the case itself, which a variable may not take over.
func tvReserved(name string) bool {
	for _, ph := range templatePlaceholders {
		if ph["token"] == "{"+name+"}" {
			return true
		}
	}
	for _, b := range tvBuiltin {
		if b.name == name {
			return true
		}
	}
	return false
}

func (s *server) letterSettings(r *http.Request, tenant string) map[string]map[string]string {
	var set map[string]map[string]string
	var raw []byte
	_ = s.db.QueryRow(r.Context(), `SELECT settings FROM public.letter_settings WHERE tenant=$1`, tenant).Scan(&raw)
	_ = json.Unmarshal(raw, &set)
	if set == nil {
		set = map[string]map[string]string{}
	}
	return set
}

// programVars: the program's standard variables, from its config.
func (s *server) programVars(r *http.Request, tenant string) map[string]string {
	var def struct {
		Variables map[string]string `json:"variables"`
	}
	if raw, ok := progRaw(r, s, tenant, "letters"); ok {
		_ = json.Unmarshal(raw, &def)
	}
	if def.Variables == nil {
		def.Variables = map[string]string{}
	}
	return def.Variables
}

// customVars: the program's variables overlaid with the office's.
func (s *server) customVars(r *http.Request, tenant string) map[string]string {
	out := s.programVars(r, tenant)
	for k, v := range s.letterSettings(r, tenant)["variables"] {
		out[k] = v
	}
	return out
}

// templateTokens: every placeholder a template of this tenant may carry.
func (s *server) templateTokens(r *http.Request, tenant string) []map[string]string {
	out := append([]map[string]string{}, templatePlaceholders...)
	names := []string{}
	vars := s.customVars(r, tenant)
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		out = append(out, map[string]string{"token": "{" + k + "}", "meaning": "Your variable: " + vars[k]})
	}
	return out
}

// getTemplateVariables: GET /template-variables -- the office values and the variables, with their current values.
func (s *server) getTemplateVariables(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, drReadRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	set := s.letterSettings(r, tenant)
	vals := s.letterVars(r, tenant)
	builtin := []map[string]any{}
	for _, b := range tvBuiltin {
		builtin = append(builtin, map[string]any{"name": b.name, "meaning": b.meaning, "value": vals["{"+b.name+"}"],
			"set": set[b.group][b.field] != ""})
	}
	prog := s.programVars(r, tenant)
	custom := []map[string]any{}
	names := []string{}
	all := s.customVars(r, tenant)
	for k := range all {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		_, standard := prog[k]
		custom = append(custom, map[string]any{"name": k, "value": all[k], "standard": standard, "standard_value": prog[k]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"builtin": builtin, "variables": custom, "can_edit": hasAnyRole(p, tvEditRoles...)})
}

// putTemplateVariables: PUT /template-variables {"builtin": {"org_name": "..."}, "variables": {"agency_contacts": "..."}}
// The variables given replace the office's set; an office value left empty goes back to the program's standard
// wording. A variable of the program's own cannot be deleted, only changed.
func (s *server) putTemplateVariables(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, tvEditRoles...) {
		http.Error(w, `{"error":"forbidden: template variables are set by the PM or an administrator"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Builtin   map[string]string  `json:"builtin"`
		Variables *map[string]string `json:"variables"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if json.Unmarshal(raw, &in) != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	bad := func(msg string) { writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg}) }
	set := s.letterSettings(r, tenant)
	changed := []string{}
	for name, v := range in.Builtin {
		found := false
		for _, b := range tvBuiltin {
			if b.name != name {
				continue
			}
			found = true
			v = strings.TrimSpace(v)
			if utf8.RuneCountInString(v) > 300 {
				bad(name + " is too long")
				return
			}
			if name == "contact_email" && v != "" && (!strings.Contains(v, "@") || strings.ContainsAny(v, " ,;")) {
				bad("contact_email must be one email address")
				return
			}
			if set[b.group] == nil {
				set[b.group] = map[string]string{}
			}
			if v == "" {
				delete(set[b.group], b.field)
			} else {
				set[b.group][b.field] = v
			}
			changed = append(changed, name)
		}
		if !found {
			bad("unknown office value " + name)
			return
		}
	}
	if in.Variables != nil {
		vars := map[string]string{}
		if len(*in.Variables) > tvMax {
			bad(fmt.Sprintf("at most %d variables", tvMax))
			return
		}
		for name, v := range *in.Variables {
			name = strings.TrimSpace(name)
			v = strings.TrimSpace(v)
			if !tvNameRE.MatchString(name) {
				bad("a variable's name is 2–41 lowercase letters, digits or underscores, starting with a letter: " + name)
				return
			}
			if tvReserved(name) {
				bad("{" + name + "} is filled in by the system and cannot be a variable")
				return
			}
			if v == "" {
				bad("give " + name + " a value")
				return
			}
			if utf8.RuneCountInString(v) > 2000 {
				bad(name + " is longer than 2000 characters")
				return
			}
			vars[name] = v
		}
		for k := range s.programVars(r, tenant) {
			if _, ok := vars[k]; !ok {
				bad("{" + k + "} is one of the program's own variables: change its value instead of removing it")
				return
			}
		}
		set["variables"] = vars
		changed = append(changed, "variables")
	}
	if len(changed) == 0 {
		bad("nothing to save")
		return
	}
	b, _ := json.Marshal(set)
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.letter_settings (tenant, settings, updated_by) VALUES ($1,$2::jsonb,$3)
		ON CONFLICT (tenant) DO UPDATE SET settings=$2::jsonb, updated_by=$3, updated_at=now()`,
		tenant, string(b), displayName(p)); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TEMPLATE_VARIABLES_UPDATED", map[string]any{"by": p.Subject, "changed": changed})
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "changed": changed})
}

// letterNames: what the case knows for the bracketed fill-ins of the standard templates.
type letterNames struct {
	Provider, Plan, ProviderContact, PlanContact, DueDate string
	Filer                                                 string // the filing party as the letter greets it: its contact, else its name
	Estimate                                              string // the Full Review cost estimate, "$1,500.00" ("" not yet estimated)
}

// fillInNames replaces the bracketed fill-ins the standard templates carry ("[Plan name]", "[Provider Contact
// Name]"...) with the case's own names. A contact the case doesn't know becomes a courteous title -- never a bracket
// left in a letter that goes out. A party name or due date it doesn't know is left for the drafter to see in QA.
func fillInNames(text string, n letterNames) string {
	pairs := []string{
		"[Provider Contact Name]", orWord(n.ProviderContact, "Provider Representative"),
		"[Health Plan Contact Name]", orWord(n.PlanContact, "Health Plan Representative"),
	}
	if n.Plan != "" {
		pairs = append(pairs, "[Plan name]", n.Plan, "[plan name]", n.Plan, "[PLAN NAME]", strings.ToUpper(n.Plan))
	}
	if n.Provider != "" {
		pairs = append(pairs, "[Provider Name]", n.Provider, "[provider name]", n.Provider, "[PROVIDER NAME]", strings.ToUpper(n.Provider))
	}
	if n.DueDate != "" {
		pairs = append(pairs, "[due date]", n.DueDate)
	}
	if n.Filer != "" {
		pairs = append(pairs, "[Filing Party full name]", n.Filer, "[Filing party full name]", n.Filer, "[filing party full name]", n.Filer)
	}
	if n.Estimate != "" {
		// The templates write "$[insert amount]"; the amount carries its own dollar sign.
		pairs = append(pairs, "$[insert amount]", n.Estimate, "[insert amount]", n.Estimate)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}
