package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Deadline extensions (scripts/deadline_extensions.sql): the PM moves one of a case's program clocks by a number of
// days, with a written reason. The projection (projectProgramClocks) adds the extensions to the clock's due date and
// hard limit, and the case's workflow is signalled DEADLINE_EXTENDED so its follow-ups and breach wait as well.

// nonExtendableClocks are statutory windows nobody can move: AHCA no longer grants extensions on the filing window
// (the same rule the eligibility override enforces in program.go).
var nonExtendableClocks = map[string]bool{"FILING_ELIGIBILITY": true}

const maxExtensionDays = 90

// clockExtensions: clock → total days extended, and every extension (oldest first) for the deadline table.
func (s *server) clockExtensions(ctx context.Context, tenant, caseID string) (map[string]int, map[string][]map[string]any) {
	total, list := map[string]int{}, map[string][]map[string]any{}
	rows, err := s.db.Query(ctx, `
		SELECT clock, days, reason, granted_by, created_at FROM public.case_deadline_extensions
		WHERE tenant=$1 AND case_id=$2 ORDER BY created_at`, tenant, caseID)
	if err != nil {
		return total, list // not migrated yet: no extensions, the deadlines still project
	}
	defer rows.Close()
	for rows.Next() {
		var clock, reason, by string
		var days int
		var at any
		if rows.Scan(&clock, &days, &reason, &by, &at) == nil {
			total[clock] += days
			list[clock] = append(list[clock], map[string]any{"days": days, "reason": reason, "granted_by": by, "at": at})
		}
	}
	return total, list
}

// extendDeadline: POST /cases/{caseId}/clocks/{clock}/extend {"days": 5, "reason": "..."}
func (s *server) extendDeadline(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: deadlines are extended by the PM"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID, clock := chi.URLParam(r, "caseId"), strings.ToUpper(chi.URLParam(r, "clock"))
	var in struct {
		Days   int    `json:"days"`
		Reason string `json:"reason"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if json.Unmarshal(raw, &in) != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Days < 1 || in.Days > maxExtensionDays {
		http.Error(w, fmt.Sprintf(`{"error":"an extension is 1 to %d days"}`, maxExtensionDays), http.StatusBadRequest)
		return
	}
	if len([]rune(in.Reason)) < 10 {
		http.Error(w, `{"error":"give the reason for the extension (at least 10 characters) — it is kept on the case record"}`, http.StatusBadRequest)
		return
	}
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		http.Error(w, `{"error":"no program rules for tenant"}`, http.StatusBadRequest)
		return
	}
	var rule *ClockRule
	for i := range cfg.Clocks {
		if cfg.Clocks[i].Name == clock {
			rule = &cfg.Clocks[i]
		}
	}
	if rule == nil {
		http.Error(w, fmt.Sprintf(`{"error":"unknown deadline %q"}`, clock), http.StatusNotFound)
		return
	}
	if nonExtendableClocks[clock] {
		http.Error(w, `{"error":"this deadline is statutory and cannot be extended (AHCA no longer grants extensions on the filing window)"}`, http.StatusUnprocessableEntity)
		return
	}
	var caseNumber string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)),
		caseID).Scan(&caseNumber); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// Only a running clock can be extended: one that hasn't started has no due date to move.
	dates := s.programDates(r, tenant, caseID)
	if rule.Basis != "received_at" && strings.TrimSpace(dates[rule.Basis]) == "" {
		http.Error(w, `{"error":"this deadline has not started yet — it can be extended once its clock is running"}`, http.StatusUnprocessableEntity)
		return
	}

	// Record first, then tell the workflow: a failed insert must never leave the workflow extended with no record.
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.case_deadline_extensions (tenant, case_id, clock, days, reason, granted_by)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		tenant, caseID, clock, in.Days, in.Reason, displayName(p)).Scan(&id); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	signalled := false
	if s.tc != nil {
		wfID := fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
		signalled = s.tc.SignalWorkflow(r.Context(), wfID, "", "DEADLINE_EXTENDED",
			map[string]any{"clock": clock, "days": in.Days, "by": displayName(p)}) == nil
	}
	if signalled {
		_, _ = s.db.Exec(r.Context(), `UPDATE public.case_deadline_extensions SET workflow_signalled=true WHERE id=$1`, id)
	}
	label := rule.Label
	if label == "" {
		label = clock
	}
	msg := fmt.Sprintf("%s deadline extended by %d day%s by %s — %s", label, in.Days, map[bool]string{true: "", false: "s"}[in.Days == 1], displayName(p), in.Reason)
	s.logActivity(r.Context(), tenant, caseID, "DEADLINE_EXTENDED", msg)
	s.logAudit(r.Context(), tenant, caseID, "DEADLINE_EXTENDED", map[string]any{
		"by": p.Subject, "clock": clock, "days": in.Days, "reason": in.Reason, "extension_id": id, "workflow_signalled": signalled})
	s.notify(r, tenant, "role:CASE_MANAGER", "DEADLINE_EXTENDED", fmt.Sprintf("Case %s: %s", caseNumber, msg), "#/cases/"+caseID)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "clock": clock, "days": in.Days, "workflow_signalled": signalled})
}
