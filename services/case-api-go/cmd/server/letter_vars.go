package main

import (
	"encoding/json"
	"net/http"
	"strings"
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
	org := pick("letterhead", "org_name", def.OrgName)
	short := pick("letterhead", "org_short", def.OrgShort)
	if short == "" {
		short = strings.TrimSuffix(org, " LLC")
	}
	return map[string]string{
		"{org_name}":          org,
		"{org_short}":         short,
		"{contact_email}":     pick("letterhead", "contact_email", def.ContactEmail),
		"{contact_phone}":     pick("letterhead", "contact_phone", def.ContactPhone),
		"{program_name}":      pick("letterhead", "program_name", def.ProgramName),
		"{agency_salutation}": pick("agency", "email_salutation", def.AgencyEmail),
	}
}

// fillInNames replaces the bracketed fill-ins the standard templates carry ("[Plan name]", "[Provider Contact
// Name]"...) with the case's own names, when the case knows them. One it does not know is left for the drafter.
func fillInNames(text, provider, plan, providerContact string) string {
	pairs := []string{}
	if plan != "" {
		pairs = append(pairs, "[Plan name]", plan, "[plan name]", plan, "[PLAN NAME]", strings.ToUpper(plan))
	}
	if provider != "" {
		pairs = append(pairs, "[Provider Name]", provider, "[provider name]", provider, "[PROVIDER NAME]", strings.ToUpper(provider))
	}
	if providerContact != "" {
		pairs = append(pairs, "[Provider Contact Name]", providerContact)
	}
	if len(pairs) == 0 {
		return text
	}
	return strings.NewReplacer(pairs...).Replace(text)
}
