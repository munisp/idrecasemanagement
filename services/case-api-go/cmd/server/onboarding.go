// onboarding.go — stakeholder onboarding: applications, role-based approval
// chains, and status. The durable state machine lives in Temporal
// (StakeholderOnboardingWorkflow); these endpoints are its front door.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	temporalclient "go.temporal.io/sdk/client"
)

// Stakeholder types: IDRE_ENTITY (certified dispute-resolution entity),
// PROVIDER_ORG, PAYER_ORG, STATE_AUDITOR_ORG, ADMIN_STAFF.
type OnboardingApplication struct {
	ID          string         `json:"id"`
	Tenant      string         `json:"tenant"`
	Type        string         `json:"type"`        // IDRE_ENTITY | PROVIDER_ORG | PAYER_ORG | ...
	LegalName   string         `json:"legal_name"`
	EIN         string         `json:"ein"`         // masked at rest
	NPI         string         `json:"npi,omitempty"`
	Payload     map[string]any `json:"payload"`     // type-specific fields (fee schedule, COI, banking…)
	Status      string         `json:"status"`
	SubmittedAt time.Time      `json:"submitted_at"`
}

// submitApplication: POST /v1/tenants/{tenant}/onboarding/applications
// Creates the application row and starts the durable onboarding workflow.
func (s *server) submitApplication(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in OnboardingApplication
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.LegalName == "" || in.Type == "" {
		http.Error(w, `{"error":"legal_name and type required"}`, http.StatusBadRequest)
		return
	}
	payload, _ := json.Marshal(in.Payload)
	var appID string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.stakeholder_applications
		  (tenant, type, legal_name, ein_masked, npi, payload, status)
		VALUES ($1,$2,$3,$4,$5,$6,'SUBMITTED') RETURNING id`,
		tenant, in.Type, in.LegalName, maskEIN(in.EIN), in.NPI, payload).Scan(&appID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	wfID := fmt.Sprintf("ONB-%s-%s", tenant, appID)
	_, err = s.tc.ExecuteWorkflow(r.Context(), temporalclient.StartWorkflowOptions{
		ID: wfID, TaskQueue: "idre-onboarding",
	}, "StakeholderOnboardingWorkflow", map[string]any{
		"tenant": tenant, "application_id": appID, "type": in.Type,
		"legal_name": in.LegalName, "ein": in.EIN, "npi": in.NPI, "payload": in.Payload,
	})
	if err != nil {
		http.Error(w, `{"error":"workflow start failed"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"application_id": appID, "workflow_id": wfID, "status": "SUBMITTED",
	})
}

func (s *server) listApplications(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), `
		SELECT id, tenant, type, legal_name, npi, status, submitted_at
		FROM public.stakeholder_applications
		WHERE tenant=$1 ORDER BY submitted_at DESC LIMIT 200`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []OnboardingApplication{}
	for rows.Next() {
		var a OnboardingApplication
		if rows.Scan(&a.ID, &a.Tenant, &a.Type, &a.LegalName, &a.NPI, &a.Status, &a.SubmittedAt) == nil {
			out = append(out, a)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// decideApplication: approve/reject signals the workflow; the workflow performs
// provisioning (Keycloak account + tenant group, TB accounts for IDREs, invite email).
func (s *server) decideApplication(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		Decision string `json:"decision"` // APPROVE | REJECT
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(in.Decision != "APPROVE" && in.Decision != "REJECT") {
		http.Error(w, `{"error":"decision must be APPROVE or REJECT"}`, http.StatusBadRequest)
		return
	}
	appID := chi.URLParam(r, "appId")

	// Approval authority matrix (enforced again inside the workflow):
	//   IDRE_ENTITY  -> FEDERAL_ADMIN only
	//   others       -> CASE_MANAGER of that tenant, or FEDERAL_ADMIN
	var appType, tenant string
	if err := s.db.QueryRow(r.Context(),
		`SELECT type, tenant FROM public.stakeholder_applications WHERE id=$1`, appID).
		Scan(&appType, &tenant); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	authorized := hasRole(p, "FEDERAL_ADMIN")
	if appType != "IDRE_ENTITY" && hasRole(p, "CASE_MANAGER") {
		for _, t := range p.Tenants {
			if t == tenant {
				authorized = true
			}
		}
	}
	if !authorized {
		http.Error(w, `{"error":"insufficient approval authority"}`, http.StatusForbidden)
		return
	}

	wfID := fmt.Sprintf("ONB-%s-%s", tenant, appID)
	if err := s.tc.SignalWorkflow(r.Context(), wfID, "", "DECISION", map[string]any{
		"decision": in.Decision, "reason": in.Reason, "approver": p.Subject,
	}); err != nil {
		http.Error(w, `{"error":"signal failed"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "decision signaled"})
}

func hasRole(p principal, role string) bool {
	for _, rr := range p.Roles {
		if rr == role {
			return true
		}
	}
	return false
}

func maskEIN(ein string) string {
	if len(ein) < 4 {
		return "****"
	}
	return "*****" + ein[len(ein)-4:]
}
