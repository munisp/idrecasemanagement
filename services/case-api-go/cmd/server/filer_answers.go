package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// filerEligibilityAnswers checks the filer's answers to the two eligibility
// questions documents often leave out -- the provider type and whether the
// provider is in the plan's network -- and returns them as case details.
// Either may be left blank ("not sure"): the documents decide then, or the
// Reviewer does.
func (s *server) filerEligibilityAnswers(r *http.Request, tenant, providerType, inNetwork string) (map[string]any, string) {
	out := map[string]any{}
	if pt := strings.TrimSpace(providerType); pt != "" {
		cfg := s.loadProgram(r, tenant)
		if cfg == nil || !contains(eligibilityProviderTypes(cfg), pt) {
			return nil, "provider type " + pt + " is not one this program knows"
		}
		out["provider_type"] = pt
	}
	switch strings.ToLower(strings.TrimSpace(inNetwork)) {
	case "":
	case "yes", "true":
		out["contracted"] = true
	case "no", "false":
		out["contracted"] = false
	default:
		return nil, "in_network must be yes, no, or left blank"
	}
	return out, ""
}

// applyFilerEligibilityAnswers puts the filer's answers on the case. They
// are what a person stated, so they beat what was read off the documents;
// where a document said otherwise, the disagreement is recorded for the
// eligibility review (as eligibility_conflicts, which sends the result for a
// second look) instead of being dropped.
func (s *server) applyFilerEligibilityAnswers(ctx context.Context, tenant, caseID string, answers map[string]any) {
	if len(answers) == 0 {
		return
	}
	tbl := sanitizeTenant(tenant)
	var raw []byte
	if s.db.QueryRow(ctx, fmt.Sprintf(`SELECT coalesce(details,'{}'::jsonb) FROM tenant_%s.cases WHERE id=$1`, tbl), caseID).Scan(&raw) != nil {
		return
	}
	var cur map[string]any
	_ = json.Unmarshal(raw, &cur)
	conflicts, _ := cur["eligibility_conflicts"].([]any)
	set := map[string]any{}
	for k, v := range answers {
		if was, ok := cur[k]; ok && fmt.Sprint(was) != fmt.Sprint(v) {
			conflicts = append(conflicts, map[string]any{
				"field": k, "case_value": v, "document_value": was, "document": "the filed documents"})
		}
		set[k] = v
	}
	if _, ok := answers["provider_type"]; ok {
		set["provider_type_basis"] = "" // stated by the filer, not inferred
	}
	if c, ok := answers["contracted"].(bool); ok {
		set["out_of_network"] = map[bool]string{true: "No", false: "Yes"}[c]
	}
	if len(conflicts) > 0 {
		set["eligibility_conflicts"] = conflicts
	}
	b, _ := json.Marshal(set)
	_, _ = s.db.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET details = coalesce(details,'{}'::jsonb) || $2::jsonb WHERE id=$1`, tbl), caseID, b)
}
