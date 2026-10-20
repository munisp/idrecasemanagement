package main

// autoassign.go — new cases go to the tenant's Reviewers in turn (round robin).
//
// Each tenant has its own rotation (public.case_auto_assign, scripts/auto_assign.sql): the Reviewer who got the last
// case, the people the PM has taken out of the rotation (on leave, part-time), and whether it is on at all. A new
// case goes to the next Reviewer after the last one, in a fixed order (their Keycloak id), skipping anyone suspended
// or taken out. On by default: a tenant with no row rotates.
//
// It runs where every case is created (startAhcaCaseFull: the Florida intake in all its forms; initiateCase: the
// federal one) and only fills a case nobody holds yet. A case waiting on its initial fee is locked until it is paid,
// so it gets nobody (and uses no turn) until the payment opens it (activatePaidIntakeFor). The PM and the Tenant Admin change or undo any assignment as
// before (assignCase); the rotation simply carries on from where it is.
//
// Two cases created at once (a bulk filing) cannot both go to the same person out of turn: choosing and recording
// the next Reviewer happens under a row lock on the tenant's rotation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// nextInRotation: the Reviewer after `last` in the fixed order of `ids` (wrapping round), or the first when `last`
// is gone or empty. "" when there is nobody. Pure, so it is tested.
func nextInRotation(ids []string, last string) string {
	if len(ids) == 0 {
		return ""
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for _, id := range sorted {
		if id > last {
			return id
		}
	}
	return sorted[0]
}

// inRotation: the Reviewers taking new cases — eligible staff minus the ones taken out. Order is kept.
func inRotation(staff []staffMember, excluded []string) []staffMember {
	out := make([]staffMember, 0, len(staff))
	for _, m := range staff {
		if !contains(excluded, m.ID) {
			out = append(out, m)
		}
	}
	return out
}

// reviewersCached: the tenant's active Reviewers, kept a minute (a bulk filing creates many cases in a row, and each
// lookup is several Keycloak calls).
var reviewerCache = struct {
	sync.Mutex
	at   map[string]time.Time
	list map[string][]staffMember
}{at: map[string]time.Time{}, list: map[string][]staffMember{}}

func (s *server) reviewersCached(ctx context.Context, tenant string) ([]staffMember, error) {
	reviewerCache.Lock()
	if t, ok := reviewerCache.at[tenant]; ok && time.Since(t) < time.Minute {
		l := reviewerCache.list[tenant]
		reviewerCache.Unlock()
		return l, nil
	}
	reviewerCache.Unlock()
	l, err := s.staffWithRole(ctx, tenant, "CASE_MANAGER")
	if err != nil {
		return nil, err
	}
	reviewerCache.Lock()
	reviewerCache.at[tenant], reviewerCache.list[tenant] = time.Now(), l
	reviewerCache.Unlock()
	return l, nil
}

func forgetReviewers(tenant string) {
	reviewerCache.Lock()
	delete(reviewerCache.at, tenant)
	reviewerCache.Unlock()
}

type autoAssignSettings struct {
	Enabled   bool       `json:"enabled"`
	Last      string     `json:"last_assignee"`
	Excluded  []string   `json:"excluded"`
	UpdatedBy string     `json:"updated_by"`
	UpdatedAt *time.Time `json:"updated_at"`
}

func (s *server) loadAutoAssign(ctx context.Context, tenant string) (autoAssignSettings, error) {
	out := autoAssignSettings{Enabled: true, Excluded: []string{}}
	var last, by *string
	var at *time.Time
	err := s.db.QueryRow(ctx, `SELECT enabled, last_assignee, excluded, updated_by, updated_at FROM public.case_auto_assign WHERE tenant=$1`, tenant).
		Scan(&out.Enabled, &last, &out.Excluded, &by, &at)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	out.Last, out.UpdatedBy, out.UpdatedAt = deref(last), deref(by), at
	if out.Excluded == nil {
		out.Excluded = []string{}
	}
	return out, nil
}

// autoAssignNewCase gives a just-created case to the next Reviewer in the rotation. Best-effort: when it is off,
// there is no Reviewer, or anything fails, the case simply stays unassigned (the PM assigns it, or a Reviewer takes
// it from the pool). Returns who got it ("" = nobody).
func (s *server) autoAssignNewCase(r *http.Request, tenant, caseID, caseNumber string) string {
	ctx := r.Context()
	cfg, err := s.loadAutoAssign(ctx, tenant)
	if err != nil {
		log.Printf("auto-assign %s/%s: settings unavailable (%v) — left unassigned", tenant, caseNumber, err)
		return ""
	}
	if !cfg.Enabled {
		return ""
	}
	staff, err := s.reviewersCached(ctx, tenant)
	if err != nil {
		log.Printf("auto-assign %s/%s: staff directory unavailable (%v) — left unassigned", tenant, caseNumber, err)
		return ""
	}
	pool := inRotation(staff, cfg.Excluded)
	if len(pool) == 0 {
		return ""
	}
	ids := make([]string, len(pool))
	names := map[string]string{}
	for i, m := range pool {
		ids[i], names[m.ID] = m.ID, m.Username
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ""
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO public.case_auto_assign (tenant) VALUES ($1) ON CONFLICT (tenant) DO NOTHING`, tenant); err != nil {
		log.Printf("auto-assign %s/%s: %v — left unassigned", tenant, caseNumber, err)
		return ""
	}
	var enabled bool
	var last *string
	if err := tx.QueryRow(ctx, `SELECT enabled, last_assignee FROM public.case_auto_assign WHERE tenant=$1 FOR UPDATE`, tenant).Scan(&enabled, &last); err != nil || !enabled {
		return ""
	}
	who := nextInRotation(ids, deref(last))
	tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET assigned_to=$1, assigned_role='CASE_MANAGER', updated_at=now()
		WHERE id=$2 AND (assigned_to IS NULL OR assigned_to='unassigned-pool') AND status<>'AWAITING_PAYMENT'`, sanitizeTenant(tenant)), who, caseID)
	if err != nil || tag.RowsAffected() == 0 {
		return "" // already assigned by someone, waiting on its fee, or gone -- no turn used
	}
	if _, err := tx.Exec(ctx, `UPDATE public.case_auto_assign SET last_assignee=$2 WHERE tenant=$1`, tenant, who); err != nil {
		return ""
	}
	if err := tx.Commit(ctx); err != nil {
		return ""
	}
	name := names[who]
	s.logActivity(ctx, tenant, caseID, "MILESTONE", fmt.Sprintf("Assigned to %s (Reviewer) automatically, in turn", name))
	s.logAudit(ctx, tenant, caseID, "CASE_ASSIGNED", map[string]any{
		"by": "auto-assign", "assigned_to": who, "previous": "", "role": "CASE_MANAGER", "auto": true, "rule": "round_robin",
	})
	s.notify(r, tenant, who, "ASSIGNMENT", fmt.Sprintf("You were assigned %s (new case, your turn in the rotation)", caseNumber), "#/cases/"+caseID)
	return who
}

func canManageAssignment(p principal) bool {
	return hasAnyRole(p, "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN")
}

// getAutoAssign: GET /assignment/auto — whether new cases are assigned automatically, who is in the rotation (with
// their open caseload) and who gets the next one.
func (s *server) getAutoAssign(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !canManageAssignment(p) {
		http.Error(w, `{"error":"forbidden: the PM or the Tenant Admin sets up automatic assignment"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	cfg, err := s.loadAutoAssign(r.Context(), tenant)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "automatic assignment is not set up on this server yet"})
		return
	}
	staff, err := s.reviewersCached(r.Context(), tenant)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "staff directory unavailable: " + err.Error()})
		return
	}
	counts := s.openCaseCounts(r.Context(), tenant)
	type row struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		OpenCases  int    `json:"open_cases"`
		InRotation bool   `json:"in_rotation"`
	}
	rows := make([]row, 0, len(staff))
	ids := []string{}
	for _, m := range staff {
		in := !contains(cfg.Excluded, m.ID)
		rows = append(rows, row{ID: m.ID, Username: m.Username, OpenCases: counts[m.ID], InRotation: in})
		if in {
			ids = append(ids, m.ID)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID }) // the rotation's own order
	next := ""
	if cfg.Enabled {
		next = nextInRotation(ids, cfg.Last)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": cfg.Enabled, "rule": "round_robin", "reviewers": rows, "next": next,
		"updated_by": cfg.UpdatedBy, "updated_at": cfg.UpdatedAt,
	})
}

// setAutoAssign: PUT /assignment/auto {enabled?, excluded?} — turn it on or off, or change who is in the rotation.
func (s *server) setAutoAssign(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !canManageAssignment(p) {
		http.Error(w, `{"error":"forbidden: the PM or the Tenant Admin sets up automatic assignment"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Enabled  *bool    `json:"enabled"`
		Excluded []string `json:"excluded"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || (in.Enabled == nil && in.Excluded == nil) {
		http.Error(w, `{"error":"send enabled and/or excluded"}`, http.StatusBadRequest)
		return
	}
	cur, err := s.loadAutoAssign(r.Context(), tenant)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "automatic assignment is not set up on this server yet"})
		return
	}
	enabled, excluded := cur.Enabled, cur.Excluded
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.Excluded != nil {
		excluded = []string{}
		for _, id := range in.Excluded {
			if id != "" && !contains(excluded, id) {
				excluded = append(excluded, id)
			}
		}
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.case_auto_assign (tenant, enabled, excluded, updated_by, updated_at) VALUES ($1,$2,$3,$4,now())
		ON CONFLICT (tenant) DO UPDATE SET enabled=$2, excluded=$3, updated_by=$4, updated_at=now()`,
		tenant, enabled, excluded, displayName(p)); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	forgetReviewers(tenant)
	s.logAudit(r.Context(), tenant, "", "AUTO_ASSIGN_SETTINGS", map[string]any{
		"by": p.Subject, "enabled": enabled, "excluded": excluded, "was_enabled": cur.Enabled, "was_excluded": cur.Excluded,
	})
	s.getAutoAssign(w, r)
}
