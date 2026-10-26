// tpa_team.go — a TPA's own employees, and its provider groups.
//
// Employees: each login linked to a TPA (public.tpa_users) is one of its staff. A TPA admin adds an employee by
// name and email: the platform creates the login (role TPA, in the program's tenant group), links it to the TPA and
// emails the sign-in details to that address only. ADMIN manages the team; STAFF does everything else on the TPA page.
// Removing an employee unlinks the login and disables it (the account stays for the audit trail).
//
// Provider groups: TPA -> provider groups -> providers. A group is a practice or health system the TPA acts for;
// each provider (tpa_clients) belongs to at most one group, or none.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	chi "github.com/go-chi/chi/v5"
)

// tpaTeamAdmin: the caller may manage this TPA's team -- a TPA admin, or a platform/federal admin.
func (s *server) tpaTeamAdmin(r *http.Request, tenant, tpaID string) bool {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if hasAnyRole(p, "PLATFORM_ADMIN", "FEDERAL_ADMIN") {
		return true
	}
	var role string
	_ = s.db.QueryRow(r.Context(), `SELECT team_role FROM public.tpa_users WHERE tenant=$1 AND tpa_id=$2 AND user_sub=$3`,
		tenant, tpaID, p.Subject).Scan(&role)
	return role == "ADMIN"
}

// tpaOrgOr404 is the caller's TPA, or a 404 written for them.
func (s *server) tpaOrgOr404(w http.ResponseWriter, r *http.Request, roles ...string) (*tpaOrg, string, bool) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, roles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return nil, "", false
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return nil, "", false
	}
	return org, tenant, true
}

var tpaRoles = []string{"TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

// GET /tpa/team -- the TPA's employees.
func (s *server) tpaTeamList(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaOrgOr404(w, r, tpaRoles...)
	if !ok {
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	s.tpaTeamFillNames(r, tenant, org.ID)
	rows, _ := s.queryRows(r, `
		SELECT user_sub, team_role, coalesce(full_name,'') AS full_name, coalesce(email,'') AS email,
		       coalesce(username,'') AS username, linked_at, (user_sub = $3) AS is_me
		FROM public.tpa_users WHERE tenant=$1 AND tpa_id=$2 ORDER BY team_role, full_name, linked_at`, tenant, org.ID, p.Subject)
	writeJSON(w, http.StatusOK, map[string]any{"team": rows, "can_manage": s.tpaTeamAdmin(r, tenant, org.ID)})
}

// tpaTeamFillNames: logins linked before employees were kept here (the registrant, claim-code teammates) have no
// name or email on file; they are read from the sign-in service once and stored.
func (s *server) tpaTeamFillNames(r *http.Request, tenant, tpaID string) {
	rows, err := s.db.Query(r.Context(), `SELECT user_sub FROM public.tpa_users WHERE tenant=$1 AND tpa_id=$2 AND email IS NULL`, tenant, tpaID)
	if err != nil {
		return
	}
	var subs []string
	for rows.Next() {
		var sub string
		if rows.Scan(&sub) == nil {
			subs = append(subs, sub)
		}
	}
	rows.Close()
	if len(subs) == 0 {
		return
	}
	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		return
	}
	for _, sub := range subs {
		resp, err := s.kcDo(r.Context(), http.MethodGet, "/admin/realms/idre/users/"+url.PathEscape(sub), tok, nil)
		if err != nil {
			continue
		}
		var u struct {
			Username  string `json:"username"`
			Email     string `json:"email"`
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		}
		ok := resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&u) == nil
		resp.Body.Close()
		if !ok {
			continue
		}
		name := strings.TrimSpace(u.FirstName + " " + u.LastName)
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.tpa_users SET email=$4, username=coalesce(username, nullif($5,'')), full_name=coalesce(full_name, nullif($6,''))
			WHERE tenant=$1 AND tpa_id=$2 AND user_sub=$3`, tenant, tpaID, sub, strings.ToLower(u.Email), u.Username, name)
	}
}

// POST /tpa/team {full_name, email, team_role} -- add an employee: a login of their own, emailed to them.
func (s *server) tpaTeamAdd(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaOrgOr404(w, r, tpaRoles...)
	if !ok {
		return
	}
	if !s.tpaTeamAdmin(r, tenant, org.ID) {
		http.Error(w, `{"error":"only your organization's admins can add employees"}`, http.StatusForbidden)
		return
	}
	var in struct {
		FullName string `json:"full_name"`
		Email    string `json:"email"`
		TeamRole string `json:"team_role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	in.FullName, in.Email = strings.TrimSpace(in.FullName), strings.ToLower(strings.TrimSpace(in.Email))
	role := strings.ToUpper(strings.TrimSpace(in.TeamRole))
	if role == "" {
		role = "STAFF"
	}
	switch {
	case in.FullName == "":
		http.Error(w, `{"error":"enter the employee's name"}`, http.StatusBadRequest)
		return
	case !validMailbox(in.Email):
		http.Error(w, `{"error":"enter the employee's work email — their sign-in details are sent there"}`, http.StatusBadRequest)
		return
	case role != "ADMIN" && role != "STAFF":
		http.Error(w, `{"error":"team_role must be ADMIN or STAFF"}`, http.StatusBadRequest)
		return
	}
	tok, err := s.kcAdminToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "the sign-in service could not be reached — try again"})
		return
	}
	groupPath := "/tenant/" + tenant
	if _, err := s.kcEnsureGroup(r.Context(), tok, tenant); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "the sign-in service could not be reached — try again"})
		return
	}
	base := tpaUsernameFromEmail(in.Email)
	username := base
	var result map[string]any
	for attempt := 0; attempt < 5; attempt++ {
		result = s.kcCreateUser(r.Context(), tok, username, in.Email, "TPA", []string{groupPath})
		msg, failed := result["error"].(string)
		if !failed {
			break
		}
		if strings.Contains(msg, "already exists") && attempt == 0 && s.kcEmailTaken(r, tok, in.Email) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "that email already has an account on the portal — use a different work email"})
			return
		}
		if !strings.Contains(msg, "already exists") {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "the login could not be created: " + msg})
			return
		}
		suffix := make([]byte, 2)
		_, _ = rand.Read(suffix)
		username = base + hex.EncodeToString(suffix)
	}
	userID, _ := result["user_id"].(string)
	if _, failed := result["error"]; failed || userID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "the login could not be created — try again"})
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.tpa_users (tenant, user_sub, tpa_id, team_role, full_name, email, username, added_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, tenant, userID, org.ID, role, in.FullName, in.Email, username, displayName(p)); err != nil {
		_ = s.kcDeleteUser(r.Context(), tok, userID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the employee could not be added — try again"})
		return
	}
	tempPassword, _ := result["temporary_password"].(string)
	subject, body := tpaStaffWelcomeMail(org.Name, in.FullName, displayName(p), username, tempPassword, s.cfg.PortalBaseURL)
	if merr := s.sendMail(tenant, []string{in.Email}, nil, subject, body); merr != nil && !errors.Is(merr, errMailQueued) {
		// Nobody else holds the password: an account whose email never arrived is taken back.
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.tpa_users WHERE tenant=$1 AND user_sub=$2`, tenant, userID)
		_ = s.kcDeleteUser(r.Context(), tok, userID)
		s.logCorrespondence(r, tenant, "", "OUT", "tpa_staff_welcome", subject, tpaWelcomeLogNote, []string{in.Email}, nil, displayName(p), merr.Error())
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "the sign-in email could not be sent to that address, so the employee was not added. Check the address and try again."})
		return
	}
	s.logCorrespondence(r, tenant, "", "OUT", "tpa_staff_welcome", subject, tpaWelcomeLogNote, []string{in.Email}, nil, displayName(p))
	s.logAudit(r.Context(), tenant, "", "TPA_EMPLOYEE_ADDED", map[string]any{"tpa_id": org.ID, "user_id": userID,
		"username": username, "email": in.Email, "team_role": role, "by": p.Subject})
	writeJSON(w, http.StatusCreated, map[string]any{"user_sub": userID, "username": username, "team_role": role,
		"note": "sign-in details were emailed to " + in.Email})
}

// kcEmailTaken: an account with this email already exists in the realm.
func (s *server) kcEmailTaken(r *http.Request, tok, email string) bool {
	resp, err := s.kcDo(r.Context(), http.MethodGet, "/admin/realms/idre/users?exact=true&email="+url.QueryEscape(email), tok, nil)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var users []map[string]any
	return json.NewDecoder(resp.Body).Decode(&users) == nil && len(users) > 0
}

// PATCH /tpa/team/{sub} {team_role} -- make an employee an admin, or staff.
func (s *server) tpaTeamSetRole(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaOrgOr404(w, r, tpaRoles...)
	if !ok {
		return
	}
	if !s.tpaTeamAdmin(r, tenant, org.ID) {
		http.Error(w, `{"error":"only your organization's admins can change roles"}`, http.StatusForbidden)
		return
	}
	var in struct {
		TeamRole string `json:"team_role"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in)
	role := strings.ToUpper(strings.TrimSpace(in.TeamRole))
	if role != "ADMIN" && role != "STAFF" {
		http.Error(w, `{"error":"team_role must be ADMIN or STAFF"}`, http.StatusBadRequest)
		return
	}
	sub := chi.URLParam(r, "sub")
	if role == "STAFF" && s.tpaLastAdmin(r, tenant, org.ID, sub) {
		http.Error(w, `{"error":"your organization needs at least one admin — make someone else an admin first"}`, http.StatusConflict)
		return
	}
	res, err := s.db.Exec(r.Context(), `UPDATE public.tpa_users SET team_role=$4 WHERE tenant=$1 AND tpa_id=$2 AND user_sub=$3`,
		tenant, org.ID, sub, role)
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	s.logAudit(r.Context(), tenant, "", "TPA_EMPLOYEE_ROLE", map[string]any{"tpa_id": org.ID, "user_id": sub, "team_role": role, "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"user_sub": sub, "team_role": role})
}

// DELETE /tpa/team/{sub} -- remove an employee: the login is unlinked and disabled.
func (s *server) tpaTeamRemove(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaOrgOr404(w, r, tpaRoles...)
	if !ok {
		return
	}
	if !s.tpaTeamAdmin(r, tenant, org.ID) {
		http.Error(w, `{"error":"only your organization's admins can remove employees"}`, http.StatusForbidden)
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	sub := chi.URLParam(r, "sub")
	if sub == p.Subject {
		http.Error(w, `{"error":"you can't remove yourself — ask another admin"}`, http.StatusConflict)
		return
	}
	if s.tpaLastAdmin(r, tenant, org.ID, sub) {
		http.Error(w, `{"error":"your organization needs at least one admin — make someone else an admin first"}`, http.StatusConflict)
		return
	}
	res, err := s.db.Exec(r.Context(), `DELETE FROM public.tpa_users WHERE tenant=$1 AND tpa_id=$2 AND user_sub=$3`, tenant, org.ID, sub)
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// Only a login that belongs to no other organization is switched off.
	var elsewhere int
	_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM public.tpa_users WHERE user_sub=$1`, sub).Scan(&elsewhere)
	disabled := false
	if elsewhere == 0 {
		if tok, err := s.kcAdminToken(r.Context()); err == nil {
			if roles := s.kcUserRoles(r.Context(), tok, sub); len(roles) > 0 && onlyTPARole(roles) {
				disabled = s.kcSetUserEnabled(r.Context(), tok, sub, false) == nil
			}
		}
	}
	s.logAudit(r.Context(), tenant, "", "TPA_EMPLOYEE_REMOVED", map[string]any{"tpa_id": org.ID, "user_id": sub, "login_disabled": disabled, "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"removed": sub, "login_disabled": disabled})
}

// onlyTPARole: the account holds no role but TPA (and Keycloak's defaults), so disabling it takes nothing else away.
func onlyTPARole(roles []string) bool {
	for _, r := range roles {
		if r == "TPA" || strings.HasPrefix(r, "default-roles") || r == "offline_access" || r == "uma_authorization" {
			continue
		}
		return false
	}
	return true
}

// tpaLastAdmin: sub is the TPA's only admin.
func (s *server) tpaLastAdmin(r *http.Request, tenant, tpaID, sub string) bool {
	var others int
	var isAdmin bool
	_ = s.db.QueryRow(r.Context(), `
		SELECT count(*) FILTER (WHERE team_role='ADMIN' AND user_sub<>$3), bool_or(team_role='ADMIN' AND user_sub=$3)
		FROM public.tpa_users WHERE tenant=$1 AND tpa_id=$2`, tenant, tpaID, sub).Scan(&others, &isAdmin)
	return isAdmin && others == 0
}

func tpaStaffWelcomeMail(orgName, name, addedBy, username, tempPassword, portalBase string) (subject, body string) {
	subject = "Your portal account at " + orgName
	body = "Hello " + name + ",\n\n" +
		addedBy + " added you to " + orgName + "'s account on the dispute resolution portal. You can file, pay and " +
		"track disputes for the providers " + orgName + " acts for.\n\n" +
		"Sign in:   " + strings.TrimRight(portalBase, "/") + "/app.html\n" +
		"Username:  " + username + "\n" +
		"Password:  " + tempPassword + "\n\n" +
		"This password is temporary: you will be asked to choose a new one the first time you sign in.\n\n" +
		"If you don't work at " + orgName + ", you can ignore this email."
	return subject, body
}

// ---- provider groups ------------------------------------------------------------------------------------------

type tpaGroupIn struct {
	Name         *string `json:"name"`
	TIN          *string `json:"tin"`
	NPI          *string `json:"npi"`
	ContactEmail *string `json:"contact_email"`
	Status       *string `json:"status"`
}

// check trims the fields given and says what is wrong with them, "" when nothing is.
func (g *tpaGroupIn) check() string {
	trim := func(p *string) {
		if p != nil {
			*p = strings.TrimSpace(*p)
		}
	}
	trim(g.Name)
	trim(g.TIN)
	trim(g.NPI)
	trim(g.ContactEmail)
	switch {
	case g.Name != nil && *g.Name == "":
		return "enter the provider group's name"
	case g.Name != nil && len(*g.Name) > 200:
		return "the group's name is too long"
	case g.NPI != nil && *g.NPI != "" && !npiValid(*g.NPI):
		return "the group NPI is not a valid NPI (10 digits, check digit)"
	case g.ContactEmail != nil && *g.ContactEmail != "" && !validMailbox(*g.ContactEmail):
		return "the group's contact email is not a valid address"
	case g.Status != nil && *g.Status != "ACTIVE" && *g.Status != "SUSPENDED":
		return "status must be ACTIVE or SUSPENDED"
	}
	if g.TIN != nil && *g.TIN != "" {
		d := strings.Map(func(c rune) rune {
			if c >= '0' && c <= '9' {
				return c
			}
			return -1
		}, *g.TIN)
		if len(d) != 9 {
			return "the group TIN must be 9 digits"
		}
		*g.TIN = d[:2] + "-" + d[2:]
	}
	return ""
}

// GET /tpa/groups -- the TPA's provider groups, each with its providers counted.
func (s *server) tpaGroupsList(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaOrgOr404(w, r, append(tpaRoles, "FINANCE")...)
	if !ok {
		return
	}
	rows, _ := s.queryRows(r, `
		SELECT g.id, g.name, coalesce(g.tin,'') AS tin, coalesce(g.npi,'') AS npi, coalesce(g.contact_email,'') AS contact_email,
		       g.status, g.created_at,
		       (SELECT count(*) FROM public.tpa_clients c WHERE c.group_id=g.id) AS provider_count,
		       (SELECT count(*) FROM public.case_origin o JOIN public.tpa_clients c ON c.id=o.client_id WHERE c.group_id=g.id) AS case_count
		FROM public.tpa_provider_groups g WHERE g.tenant=$1 AND g.tpa_id=$2 ORDER BY lower(g.name)`, tenant, org.ID)
	writeJSON(w, http.StatusOK, map[string]any{"groups": rows})
}

// POST /tpa/groups {name, tin, npi, contact_email}
func (s *server) tpaGroupAdd(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaOrgOr404(w, r, tpaRoles...)
	if !ok {
		return
	}
	var in tpaGroupIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if in.Name == nil {
		http.Error(w, `{"error":"enter the provider group's name"}`, http.StatusBadRequest)
		return
	}
	if msg := in.check(); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.tpa_provider_groups (tenant, tpa_id, name, tin, npi, contact_email, created_by)
		VALUES ($1,$2,$3,nullif($4,''),nullif($5,''),nullif($6,''),$7) RETURNING id`,
		tenant, org.ID, *in.Name, str(in.TIN), str(in.NPI), str(in.ContactEmail), displayName(p)).Scan(&id); err != nil {
		if isUniqueViolation(err) {
			http.Error(w, `{"error":"you already have a provider group with that name"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"the group could not be added"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TPA_GROUP_ADDED", map[string]any{"tpa_id": org.ID, "group_id": id, "name": *in.Name, "by": p.Subject})
	writeJSON(w, http.StatusCreated, map[string]any{"group_id": id, "name": *in.Name})
}

// PATCH /tpa/groups/{groupId} -- change a group's details or suspend it (only the fields given change).
func (s *server) tpaGroupUpdate(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaOrgOr404(w, r, tpaRoles...)
	if !ok {
		return
	}
	var in tpaGroupIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if msg := in.check(); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	res, err := s.db.Exec(r.Context(), `
		UPDATE public.tpa_provider_groups SET
		  name = coalesce($4, name), tin = CASE WHEN $5::text IS NULL THEN tin ELSE nullif($5,'') END,
		  npi = CASE WHEN $6::text IS NULL THEN npi ELSE nullif($6,'') END,
		  contact_email = CASE WHEN $7::text IS NULL THEN contact_email ELSE nullif($7,'') END,
		  status = coalesce($8, status)
		WHERE tenant=$1 AND tpa_id=$2 AND id=$3`,
		tenant, org.ID, chi.URLParam(r, "groupId"), in.Name, in.TIN, in.NPI, in.ContactEmail, in.Status)
	if err != nil {
		if isUniqueViolation(err) {
			http.Error(w, `{"error":"you already have a provider group with that name"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"the group could not be updated"}`, http.StatusInternalServerError)
		return
	}
	if res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	s.logAudit(r.Context(), tenant, "", "TPA_GROUP_UPDATED", map[string]any{"tpa_id": org.ID, "group_id": chi.URLParam(r, "groupId"), "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PUT /tpa/clients/{clientId}/group {group_id} -- put a provider in a group ("" = no group).
func (s *server) tpaClientSetGroup(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaOrgOr404(w, r, tpaRoles...)
	if !ok {
		return
	}
	var in struct {
		GroupID string `json:"group_id"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in)
	if msg := s.tpaGroupUsable(r, tenant, org.ID, in.GroupID); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	res, err := s.db.Exec(r.Context(), `UPDATE public.tpa_clients SET group_id=nullif($4,'')::uuid WHERE tenant=$1 AND tpa_id=$2 AND id=$3`,
		tenant, org.ID, chi.URLParam(r, "clientId"), in.GroupID)
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	s.logAudit(r.Context(), tenant, "", "TPA_CLIENT_GROUP", map[string]any{"tpa_id": org.ID, "client_id": chi.URLParam(r, "clientId"), "group_id": in.GroupID, "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"client_id": chi.URLParam(r, "clientId"), "group_id": in.GroupID})
}

// tpaGroupUsable: "" when groupID is empty or one of this TPA's groups, else what is wrong.
func (s *server) tpaGroupUsable(r *http.Request, tenant, tpaID, groupID string) string {
	if groupID == "" {
		return ""
	}
	var n int
	if err := s.db.QueryRow(r.Context(), `SELECT count(*) FROM public.tpa_provider_groups WHERE tenant=$1 AND tpa_id=$2 AND id::text=$3`,
		tenant, tpaID, groupID).Scan(&n); err != nil || n == 0 {
		return fmt.Sprintf("provider group %s is not one of yours", groupID)
	}
	return ""
}
