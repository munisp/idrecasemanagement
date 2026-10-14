// org_notify.go — in-app notices for the organizations outside the office:
// a TPA and a health plan.
//
// A notice is addressed to the organization ("tpa:<id>", "plan:<id>"), not to
// one login, so every login linked to it reads it in the bell -- also a
// teammate who joins later, and staff acting for it. Staff broadcasts ("*",
// "role:...") never reach these accounts (notifTargets).
package main

import (
	"context"
	"fmt"
)

func (s *server) notifyOrg(ctx context.Context, tenant, kind, orgID, caseID, typ, body, link string) {
	if orgID == "" {
		return
	}
	_, _ = s.db.Exec(ctx, `
		INSERT INTO public.notifications (tenant, user_sub, type, body, link)
		VALUES ($1,$2,$3,$4,$5)`, tenant, kind+":"+orgID, typ, body, link)
	// What the organization was told is part of its trail.
	s.logAudit(ctx, tenant, caseID, typ, map[string]any{"org": kind + ":" + orgID, "notice": body, "by": "system"})
}

// notifTargetsFor is notifTargets plus the organizations the caller belongs to.
func (s *server) notifTargetsFor(ctx context.Context, tenant string, p principal) []string {
	out := notifTargets(p)
	rows, err := s.db.Query(ctx, `
		SELECT 'tpa:' || tpa_id::text FROM public.tpa_users WHERE tenant=$1 AND user_sub=$2
		UNION ALL
		SELECT 'plan:' || plan_id::text FROM public.health_plan_users WHERE tenant=$1 AND user_sub=$2`, tenant, p.Subject)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			out = append(out, t)
		}
	}
	return out
}

// tellRespondentOpened tells the plan (or the TPA of the provider) a dispute
// is against it, once the dispute is open -- its fee paid, or none due -- which
// is also when it appears on that organization's dashboard. Once per dispute.
func (s *server) tellRespondentOpened(ctx context.Context, tenant, caseID string) {
	var planID, tpaID, name, caseNumber, filer string
	if err := s.db.QueryRow(ctx, fmt.Sprintf(`
		UPDATE public.case_respondents rs SET notified_at=now()
		FROM tenant_%s.cases c
		WHERE rs.tenant=$1 AND rs.case_id=$2 AND c.id::text=rs.case_id AND rs.notified_at IS NULL
		  AND (rs.plan_id IS NOT NULL OR rs.tpa_id IS NOT NULL) AND c.status <> 'AWAITING_PAYMENT'
		RETURNING coalesce(rs.plan_id::text,''), coalesce(rs.tpa_id::text,''), rs.name, c.case_number,
		          coalesce(nullif(c.details->>'initiating_party',''), c.details->>'requester_org', '')`, sanitizeTenant(tenant)),
		tenant, caseID).Scan(&planID, &tpaID, &name, &caseNumber, &filer); err != nil {
		return
	}
	by := ""
	if filer != "" {
		by = " by " + filer
	}
	if planID != "" {
		s.notifyOrg(ctx, tenant, "plan", planID, caseID, "DISPUTE_AGAINST_YOU",
			fmt.Sprintf("New dispute against you: %s, filed%s — respond under Disputes against us", caseNumber, by), "#/healthplan")
		return
	}
	s.notifyOrg(ctx, tenant, "tpa", tpaID, caseID, "DISPUTE_AGAINST_CLIENT",
		fmt.Sprintf("New dispute against your client %s: %s, filed%s — respond under Against your clients", name, caseNumber, by), "#/tpa")
}

// tellFiler tells the organization that filed a dispute (a TPA or a plan)
// what happened on it.
func (s *server) tellFiler(ctx context.Context, tenant, caseID, typ, body string) {
	var tpaID, planID string
	if s.db.QueryRow(ctx, `
		SELECT coalesce(tpa_id::text,''), coalesce(health_plan_id::text,'')
		FROM public.case_origin WHERE tenant=$1 AND case_id=$2`, tenant, caseID).Scan(&tpaID, &planID) != nil {
		return
	}
	s.notifyOrg(ctx, tenant, "tpa", tpaID, caseID, typ, body, "#/cases/"+caseID)
	s.notifyOrg(ctx, tenant, "plan", planID, caseID, typ, body, "#/cases/"+caseID)
}
