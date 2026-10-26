// Author: Patrick Munis at newwave
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
	"net/http"
	"strings"
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

// tellRespondentOpened tells the other side a dispute is against it, once the
// reviewer has found the dispute eligible (eligibleForOtherSide) -- which is
// also when it appears on that side's dashboard. Two notices, each once per dispute:
//
//   - an email to the notice address the filer gave (and the registered
//     plan's contact), saying how to see the dispute and respond:
//     sign in when on the platform; otherwise register (a plan) or answer
//     through a secure link (a provider);
//   - a portal notice, when the side is a registered plan.
func (s *server) tellRespondentOpened(ctx context.Context, tenant, caseID string) {
	s.emailRespondentOpened(ctx, tenant, caseID)
	var planID, tpaID, name, caseNumber, filer string
	if err := s.db.QueryRow(ctx, fmt.Sprintf(`
		UPDATE public.case_respondents rs SET notified_at=now()
		FROM tenant_%s.cases c
		WHERE rs.tenant=$1 AND rs.case_id=$2 AND c.id::text=rs.case_id AND rs.notified_at IS NULL
		  AND rs.plan_id IS NOT NULL AND `+eligibleForOtherSide+`
		RETURNING rs.plan_id::text, '', rs.name, c.case_number,
		          coalesce(nullif(c.details->>'initiating_party',''), c.details->>'requester_org', '')`, sanitizeTenant(tenant)),
		tenant, caseID).Scan(&planID, &tpaID, &name, &caseNumber, &filer); err != nil {
		return
	}
	by := ""
	if filer != "" {
		by = " by " + filer
	}
	s.notifyOrg(ctx, tenant, "plan", planID, caseID, "DISPUTE_AGAINST_YOU",
		fmt.Sprintf("Dispute against you accepted for review: %s, filed%s — respond under Disputes against us", caseNumber, by), "#/healthplan")
}

// emailRespondentOpened is the email half of tellRespondentOpened.
func (s *server) emailRespondentOpened(ctx context.Context, tenant, caseID string) {
	var side, name, email, planID, tpaID, orgEmail, tpaName, caseNumber, filer string
	var amount int64
	if err := s.db.QueryRow(ctx, fmt.Sprintf(`
		UPDATE public.case_respondents rs SET emailed_at=now()
		FROM tenant_%s.cases c
		WHERE rs.tenant=$1 AND rs.case_id=$2 AND c.id::text=rs.case_id AND rs.emailed_at IS NULL
		  AND coalesce(rs.email,'') <> '' AND `+eligibleForOtherSide+`
		RETURNING rs.side, rs.name, rs.email, coalesce(rs.plan_id::text,''), '',
		          coalesce((SELECT h.contact_email FROM public.health_plans h WHERE h.id=rs.plan_id), ''), '',
		          c.case_number, coalesce(nullif(c.details->>'initiating_party',''), c.details->>'requester_org', ''),
		          coalesce(c.disputed_amount_cents,0)`, sanitizeTenant(tenant)),
		tenant, caseID).Scan(&side, &name, &email, &planID, &tpaID, &orgEmail, &tpaName, &caseNumber, &filer, &amount); err != nil {
		return
	}
	portal := strings.TrimRight(s.cfg.PortalBaseURL, "/")
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	var b strings.Builder
	fmt.Fprintf(&b, "A dispute filed against %s has been accepted for review by the Independent Dispute Resolution program: case %s", name, caseNumber)
	if filer != "" {
		fmt.Fprintf(&b, ", filed by %s", filer)
	}
	if amount > 0 {
		fmt.Fprintf(&b, ", disputed amount %s", usd(amount))
	}
	b.WriteString(".\n\nYou can see what the filer submitted and send your response")
	switch {
	case planID != "":
		fmt.Fprintf(&b, " in the portal. Sign in at %s/app.html and open Health plan → Disputes against us.", portal)
	case side == "HEALTH_PLAN":
		fmt.Fprintf(&b, " online. Register your plan at %s/dispute.html#register-plan using this email address "+
			"(or another address at the same domain); this dispute then appears under Disputes against us when you sign in.", portal)
		b.WriteString("\n\nIf your plan is already registered under another email domain, reply to this email and the case team will link it.")
	default:
		fmt.Fprintf(&b, " through this secure link (valid for 7 days): %s", s.autoShareLink(r, tenant, caseID, "upload", ""))
	}
	b.WriteString("\n\nThe case team will also send the formal notice of the dispute and its deadlines.")
	to := []string{email}
	if orgEmail != "" && !strings.EqualFold(orgEmail, email) {
		to = append(to, orgEmail)
	}
	subject := fmt.Sprintf("Dispute %s filed against %s", caseNumber, name)
	errs := []string{}
	if err := s.sendMail(tenant, to, nil, subject, b.String()); err != nil {
		errs = append(errs, err.Error())
	}
	s.logCorrespondence(r, tenant, caseID, "OUT", "respondent_notice", subject, b.String(), to, nil, "system:respondent-notice", errs...)
	s.logActivity(ctx, tenant, caseID, "RESPONDENT_NOTIFIED", fmt.Sprintf("The other side (%s) was emailed that the dispute was found eligible: %s", name, strings.Join(to, ", ")))
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

// tellRespondentClosed: a dispute the other side was already told of was found
// ineligible on a later review -- it is gone from that side's dashboard, so say
// why instead of letting it vanish. Once (the respondent row is marked).
func (s *server) tellRespondentClosed(ctx context.Context, tenant, caseID string) {
	var planID, tpaID, name, caseNumber string
	if s.db.QueryRow(ctx, fmt.Sprintf(`
		UPDATE public.case_respondents rs SET response_status = CASE WHEN rs.response_status='AWAITING' THEN 'CLOSED' ELSE rs.response_status END
		FROM tenant_%s.cases c
		WHERE rs.tenant=$1 AND rs.case_id=$2 AND c.id::text=rs.case_id AND rs.notified_at IS NOT NULL
		  AND rs.plan_id IS NOT NULL
		RETURNING rs.plan_id::text, '', rs.name, c.case_number`, sanitizeTenant(tenant)),
		tenant, caseID).Scan(&planID, &tpaID, &name, &caseNumber) != nil {
		return
	}
	msg := fmt.Sprintf("Dispute %s was found ineligible on review and will be closed — no response is needed", caseNumber)
	s.notifyOrg(ctx, tenant, "plan", planID, caseID, "DISPUTE_CLOSED", msg, "#/healthplan")
}
