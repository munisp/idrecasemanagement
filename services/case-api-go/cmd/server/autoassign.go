package main

// autoassign.go — cases go to whoever in the rotation holds the fewest open cases, ties taken in turn (round
// robin), one rotation per tenant and role.
//
// Each tenant has a rotation for every role a case can be assigned as (public.case_auto_assign, scripts/auto_assign.sql):
// who got the last case, the people the PM has taken out of it (on leave, part-time), and whether it is on at all. The
// next case goes to the person with the fewest open cases (all the cases they hold, whatever the role); among equals,
// to the next after the last one in a fixed order (their Keycloak id). Strict turns alone would never catch up
// someone who starts with fewer cases (cases also arrive by hand, by taking one, and leave by closing), so the load
// comes first. Anyone suspended or taken out is skipped. On by default: a tenant with no row rotates.
//
//   - Reviewer: new cases are given out in turn as they are created (startAhcaCaseFull: the Florida intake in all its
//     forms; initiateCase: the federal one), filling only a case nobody holds yet. A case waiting on its initial fee
//     is locked until it is paid, so it gets nobody (and uses no turn) until the payment opens it
//     (activatePaidIntakeFor).
//   - Every role, Reviewer included: when the PM hands a case to a role, "Next in turn" picks that role's next person
//     (assignCase with next=true).
//
// The PM and the Tenant Admin change or undo any assignment as before (assignCase); a rotation simply carries on from
// where it is. Two cases given out at once (a bulk filing) cannot both go to the same person out of turn: choosing and
// recording the next person happens under a row lock on that rotation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// nextInRotation: the person after `last` in the fixed order of `ids` (wrapping round), or the first when `last` is
// gone or empty. "" when there is nobody. Pure, so it is tested.
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

// pickNext: who gets the next case — the fewest open cases (`load`), ties in turn after `last` — passing over `skip`
// (whoever holds the case now, so a hand-off always moves it) unless they are the only one. Pure, so it is tested.
func pickNext(ids []string, load map[string]int, last, skip string) string {
	cand := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != skip {
			cand = append(cand, id)
		}
	}
	if len(cand) == 0 {
		cand = ids
	}
	if len(cand) == 0 {
		return ""
	}
	least := load[cand[0]]
	for _, id := range cand {
		if load[id] < least {
			least = load[id]
		}
	}
	tied := []string{}
	for _, id := range cand {
		if load[id] == least {
			tied = append(tied, id)
		}
	}
	return nextInRotation(tied, last)
}

// inRotation: the people taking cases — eligible staff minus the ones taken out. Order is kept.
func inRotation(staff []staffMember, excluded []string) []staffMember {
	out := make([]staffMember, 0, len(staff))
	for _, m := range staff {
		if !contains(excluded, m.ID) {
			out = append(out, m)
		}
	}
	return out
}

// rotationStaff: the tenant's active staff holding a role, kept a minute (a bulk filing creates many cases in a row,
// and each lookup is several Keycloak calls).
var rotationCache = struct {
	sync.Mutex
	at   map[string]time.Time
	list map[string][]staffMember
}{at: map[string]time.Time{}, list: map[string][]staffMember{}}

func (s *server) rotationStaff(ctx context.Context, tenant, role string) ([]staffMember, error) {
	key := tenant + "|" + role
	rotationCache.Lock()
	if t, ok := rotationCache.at[key]; ok && time.Since(t) < time.Minute {
		l := rotationCache.list[key]
		rotationCache.Unlock()
		return l, nil
	}
	rotationCache.Unlock()
	l, err := s.staffWithRole(ctx, tenant, role)
	if err != nil {
		return nil, err
	}
	rotationCache.Lock()
	rotationCache.at[key], rotationCache.list[key] = time.Now(), l
	rotationCache.Unlock()
	return l, nil
}

// forgetRotationStaff drops the tenant's cached staff lists (all roles): after a rotation or a person's roles change.
func forgetRotationStaff(tenant string) {
	rotationCache.Lock()
	for k := range rotationCache.at {
		if strings.HasPrefix(k, tenant+"|") {
			delete(rotationCache.at, k)
		}
	}
	rotationCache.Unlock()
}

type autoAssignSettings struct {
	Enabled   bool       `json:"enabled"`
	Last      string     `json:"last_assignee"`
	Excluded  []string   `json:"excluded"`
	UpdatedBy string     `json:"updated_by"`
	UpdatedAt *time.Time `json:"updated_at"`
}

func (s *server) loadAutoAssign(ctx context.Context, tenant, role string) (autoAssignSettings, error) {
	out := autoAssignSettings{Enabled: true, Excluded: []string{}}
	var last, by *string
	var at *time.Time
	err := s.db.QueryRow(ctx, `SELECT enabled, last_assignee, excluded, updated_by, updated_at FROM public.case_auto_assign WHERE tenant=$1 AND role=$2`, tenant, role).
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

var (
	errRotationOff   = errors.New("rotation off")
	errRotationEmpty = errors.New("nobody in the rotation")
)

// rotationPool: the ids and names of the people in a role's rotation now, and its settings.
func (s *server) rotationPool(ctx context.Context, tenant, role string) (autoAssignSettings, []string, map[string]string, error) {
	cfg, err := s.loadAutoAssign(ctx, tenant, role)
	if err != nil {
		return cfg, nil, nil, err
	}
	staff, err := s.rotationStaff(ctx, tenant, role)
	if err != nil {
		return cfg, nil, nil, err
	}
	pool := inRotation(staff, cfg.Excluded)
	ids, names := make([]string, len(pool)), map[string]string{}
	for i, m := range pool {
		ids[i], names[m.ID] = m.ID, m.Username
	}
	return cfg, ids, names, nil
}

// takeTurn picks the next person in a role's rotation, passing over `skip`, and records the turn. `apply` runs in
// the same transaction and does the case update; false or an error leaves the turn unused.
func (s *server) takeTurn(ctx context.Context, tenant, role, skip string, apply func(tx pgx.Tx, who string) (bool, error)) (string, string, error) {
	_, ids, names, err := s.rotationPool(ctx, tenant, role)
	if err != nil {
		return "", "", err
	}
	if len(ids) == 0 {
		return "", "", errRotationEmpty
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO public.case_auto_assign (tenant, role) VALUES ($1,$2) ON CONFLICT (tenant, role) DO NOTHING`, tenant, role); err != nil {
		return "", "", err
	}
	var enabled bool
	var last *string
	if err := tx.QueryRow(ctx, `SELECT enabled, last_assignee FROM public.case_auto_assign WHERE tenant=$1 AND role=$2 FOR UPDATE`, tenant, role).Scan(&enabled, &last); err != nil {
		return "", "", err
	}
	if !enabled {
		return "", "", errRotationOff
	}
	// Counted under the lock: the taker before us has committed its case, so the load is current.
	who := pickNext(ids, s.openCaseCounts(ctx, tenant), deref(last), skip)
	if ok, err := apply(tx, who); err != nil || !ok {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE public.case_auto_assign SET last_assignee=$3 WHERE tenant=$1 AND role=$2`, tenant, role, who); err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return who, names[who], nil
}

// autoAssignNewCase gives a just-created case to the next Reviewer in the rotation. Best-effort: when it is off,
// there is no Reviewer, or anything fails, the case simply stays unassigned (the PM assigns it, or a Reviewer takes
// it from the pool). Returns who got it ("" = nobody).
func (s *server) autoAssignNewCase(r *http.Request, tenant, caseID, caseNumber string) string {
	ctx := r.Context()
	who, name, err := s.takeTurn(ctx, tenant, "CASE_MANAGER", "", func(tx pgx.Tx, who string) (bool, error) {
		tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET assigned_to=$1, assigned_role='CASE_MANAGER', updated_at=now()
			WHERE id=$2 AND (assigned_to IS NULL OR assigned_to='unassigned-pool') AND status<>'AWAITING_PAYMENT'`, sanitizeTenant(tenant)), who, caseID)
		return err == nil && tag.RowsAffected() > 0, err // 0 rows: already assigned, waiting on its fee, or gone -- no turn used
	})
	if err != nil && !errors.Is(err, errRotationOff) && !errors.Is(err, errRotationEmpty) {
		log.Printf("auto-assign %s/%s: %v — left unassigned", tenant, caseNumber, err)
	}
	if who == "" {
		return ""
	}
	s.logActivity(ctx, tenant, caseID, "MILESTONE", fmt.Sprintf("Assigned to %s (Reviewer) automatically, in turn", name))
	s.logAudit(ctx, tenant, caseID, "CASE_ASSIGNED", map[string]any{
		"by": "auto-assign", "assigned_to": who, "previous": "", "role": "CASE_MANAGER", "auto": true, "rule": "least_loaded_round_robin",
	})
	s.notify(r, tenant, who, "ASSIGNMENT", fmt.Sprintf("You were assigned %s (new case, your turn in the rotation)", caseNumber), "#/cases/"+caseID)
	return who
}

// nextInTurnPreview: who a role's rotation would give the next case to, without taking the turn (nil when it is
// off or empty). Shown in the assign picker.
func (s *server) nextInTurnPreview(ctx context.Context, tenant, role string) map[string]string {
	cfg, ids, names, err := s.rotationPool(ctx, tenant, role)
	if err != nil || !cfg.Enabled || len(ids) == 0 {
		return nil
	}
	who := pickNext(ids, s.openCaseCounts(ctx, tenant), cfg.Last, "")
	return map[string]string{"id": who, "username": names[who]}
}

func canManageAssignment(p principal) bool {
	return hasAnyRole(p, "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN")
}

// rotationRole: the role a request is about (?role= or the body's), Reviewer when not given.
func rotationRole(raw string) (string, bool) {
	role := strings.ToUpper(strings.TrimSpace(raw))
	if role == "" {
		role = "CASE_MANAGER"
	}
	_, ok := assignableRoles[role]
	return role, ok
}

// getAutoAssign: GET /assignment/auto?role=CODER — whether the role's rotation is on, who is in it (with their open
// caseload) and who gets the next case. Reviewer when no role is given.
func (s *server) getAutoAssign(w http.ResponseWriter, r *http.Request) {
	s.writeAutoAssign(w, r, r.URL.Query().Get("role"))
}

func (s *server) writeAutoAssign(w http.ResponseWriter, r *http.Request, rawRole string) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !canManageAssignment(p) {
		http.Error(w, `{"error":"forbidden: the PM or the Tenant Admin sets up automatic assignment"}`, http.StatusForbidden)
		return
	}
	role, ok := rotationRole(rawRole)
	if !ok {
		http.Error(w, `{"error":"unknown role"}`, http.StatusBadRequest)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	cfg, err := s.loadAutoAssign(r.Context(), tenant, role)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "automatic assignment is not set up on this server yet"})
		return
	}
	staff, err := s.rotationStaff(r.Context(), tenant, role)
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
		next = pickNext(ids, counts, cfg.Last, "")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"role": role, "role_label": assignableRoles[role], "new_cases": role == "CASE_MANAGER",
		"enabled": cfg.Enabled, "rule": "least_loaded_round_robin", "people": rows, "next": next,
		"updated_by": cfg.UpdatedBy, "updated_at": cfg.UpdatedAt,
	})
}

// setAutoAssign: PUT /assignment/auto {role?, enabled?, excluded?} — turn a role's rotation on or off, or change who
// is in it.
func (s *server) setAutoAssign(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !canManageAssignment(p) {
		http.Error(w, `{"error":"forbidden: the PM or the Tenant Admin sets up automatic assignment"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Role     string   `json:"role"`
		Enabled  *bool    `json:"enabled"`
		Excluded []string `json:"excluded"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || (in.Enabled == nil && in.Excluded == nil) {
		http.Error(w, `{"error":"send enabled and/or excluded"}`, http.StatusBadRequest)
		return
	}
	role, ok := rotationRole(in.Role)
	if !ok {
		http.Error(w, `{"error":"unknown role"}`, http.StatusBadRequest)
		return
	}
	cur, err := s.loadAutoAssign(r.Context(), tenant, role)
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
		INSERT INTO public.case_auto_assign (tenant, role, enabled, excluded, updated_by, updated_at) VALUES ($1,$2,$3,$4,$5,now())
		ON CONFLICT (tenant, role) DO UPDATE SET enabled=$3, excluded=$4, updated_by=$5, updated_at=now()`,
		tenant, role, enabled, excluded, displayName(p)); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	forgetRotationStaff(tenant)
	s.logAudit(r.Context(), tenant, "", "AUTO_ASSIGN_SETTINGS", map[string]any{
		"by": p.Subject, "role": role, "enabled": enabled, "excluded": excluded, "was_enabled": cur.Enabled, "was_excluded": cur.Excluded,
	})
	s.writeAutoAssign(w, r, role)
}
