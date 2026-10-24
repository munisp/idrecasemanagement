// casesummary.go — GET /cases/{caseId}/summary: the dispute in words, from the case's current data, for the top of the
// case page. What the dispute is, where it stands now (its stage and the step that is next, with whose it is), and --
// once decided -- the outcome: who prevailed, what was awarded, what was already paid, what is owed, who pays the
// review costs, and what has been billed and paid. Built fresh on every read: the page shows what the case is now,
// never what it was when someone last saved a field.
//
// Each fact comes from where it is decided: the outcome from the decision report (falling back to the case's
// recorded outcome), the review-cost party from the review invoices (falling back to "party billed"), the money from
// the invoices themselves.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	chi "github.com/go-chi/chi/v5"
)

type caseSummaryMoney struct {
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	Party       string `json:"party"`
	AmountCents int64  `json:"amount_cents"`
	Status      string `json:"status"`
	PaidOn      string `json:"paid_on,omitempty"`
	DueOn       string `json:"due_on,omitempty"`
}

var summaryFeeLabel = map[string]string{"INITIAL_FEE": "Initial review fee", "FULL_REVIEW": "Full review fee",
	"ADMIN_FEE": "Administrative fee", "DEFAULT": "Review fee"}

var summaryOutcome = map[string]string{"OVERTURNED": "the health plan's denial was overturned",
	"PARTIALLY_OVERTURNED": "the health plan's denial was partially overturned", "UPHELD": "the health plan's denial was upheld"}

func partyWord(p string) string {
	switch strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(p), " ", "_")) {
	case "HEALTH_PLAN", "PLAN":
		return "the health plan"
	case "PROVIDER":
		return "the provider"
	case "":
		return ""
	}
	return strings.ToLower(p)
}

func usdCents(c int64) string {
	neg := c < 0
	if neg {
		c = -c
	}
	s := fmt.Sprintf("$%s.%02d", groupThousands(c/100), c%100)
	if neg {
		return "-" + s
	}
	return s
}

func (s *server) caseSummary(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	if !s.caseVisible(r.Context(), p, tenant, id) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var num, status, internal, agency string
	var disputed int64
	var opened time.Time
	var detailsRaw, datesRaw []byte
	var claims *int
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT case_number, coalesce(status,''), coalesce(internal_status,''), coalesce(agency_status,''),
		       coalesce(disputed_amount_cents,0), opened_at, coalesce(details,'{}'::jsonb), coalesce(program_dates,'{}'::jsonb), num_claims
		FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), id).
		Scan(&num, &status, &internal, &agency, &disputed, &opened, &detailsRaw, &datesRaw, &claims); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var d map[string]any
	_ = json.Unmarshal(detailsRaw, &d)
	var dates map[string]any
	_ = json.Unmarshal(datesRaw, &dates)
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return strings.TrimSpace(v) }
	date := func(k string) string {
		v := str(dates, k)
		if t, err := time.Parse("2006-01-02", v); err == nil {
			return t.Format("January 2, 2006")
		}
		return ""
	}

	// The parties, by what the filing says.
	filerIsPlan := strings.EqualFold(str(d, "filing_party_type"), "HEALTH_PLAN")
	provider, plan := str(d, "provider_org"), str(d, "health_plan")
	if provider == "" && !filerIsPlan {
		provider = str(d, "requester_org")
	}
	if plan == "" && filerIsPlan {
		plan = str(d, "requester_org")
	}
	nClaims := 0
	if claims != nil {
		nClaims = *claims
	}
	if nClaims == 0 {
		if v, ok := d["claims_submitted"].(float64); ok {
			nClaims = int(v)
		}
	}

	// What the dispute is.
	what := fmt.Sprintf("%s disputes %s's handling of %s",
		orWord(provider, "The provider"), orWord(plan, "the health plan"),
		map[bool]string{true: "one claim", false: fmt.Sprintf("%d claims", nClaims)}[nClaims <= 1])
	if svc := str(d, "type_of_service"); svc != "" {
		what += " (" + strings.ToLower(svc) + ")"
	}
	if disputed > 0 {
		what += ", " + usdCents(disputed) + " in dispute"
	}
	what += "."
	if issue := str(d, "disputed_issue"); issue != "" {
		what += " The issue: " + strings.ToLower(issue) + "."
	}
	if str(d, "requester_org") != "" {
		filer := str(d, "requester_org")
		if filerIsPlan {
			what += " Filed by the health plan"
		} else if filer != provider {
			what += " Filed by " + filer + " for the provider"
		} else {
			what += " Filed by the provider"
		}
		what += " on " + opened.Format("January 2, 2006")
		if rec := date("received_at"); rec != "" {
			what += ", received " + rec
		}
		what += "."
	}

	// Where it stands: the program's own step list.
	stage, nextLabel, nextOwner := "", "", ""
	stepsDone, stepsTotal := 0, 0
	if f, err := s.workflowFacts(r.Context(), tenant, id); err == nil && s.loadProgram(r, tenant) != nil {
		steps := buildWorkflowSteps(f)
		stepsTotal = len(steps)
		for _, st := range steps {
			if st.State == "done" || st.State == "skipped" {
				stepsDone++
			}
			if st.State == "next" && nextLabel == "" {
				nextLabel, nextOwner = st.Label, st.Owner
			}
		}
	}
	closed := strings.HasPrefix(status, "CLOSED") || wfClosedStatuses[internal]

	// Eligibility.
	var eligResult, eligReason string
	_ = s.db.QueryRow(r.Context(), `SELECT result, coalesce(reason,'') FROM public.eligibility_reviews
		WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC LIMIT 1`, tenant, id).Scan(&eligResult, &eligReason)

	// The decision: the report first, then what was recorded on the case.
	var repOutcome string
	var awarded, alreadyPaid *int64
	_ = s.db.QueryRow(r.Context(), `SELECT coalesce(outcome,''), awarded_cents, already_paid_cents FROM public.decision_reports
		WHERE tenant=$1 AND case_id=$2`, tenant, id).Scan(&repOutcome, &awarded, &alreadyPaid)
	caseOutcome := str(d, "case_outcome")
	if strings.HasPrefix(strings.ToUpper(caseOutcome), "TBD") {
		caseOutcome = ""
	}
	if awarded == nil {
		if v, ok := d["final_amount_awarded_cents"].(float64); ok {
			a := int64(v)
			awarded = &a
		}
	}
	decided := repOutcome != "" || caseOutcome != "" || date("date_final_order_sent") != ""

	// Money: every fee billed on the case, and who pays the review costs.
	money := []caseSummaryMoney{}
	reviewPayer := ""
	if rows, err := s.db.Query(r.Context(), `
		SELECT kind, coalesce(party,''), amount_cents, status, coalesce(to_char(paid_at,'FMMonth FMDD, YYYY'),''),
		       coalesce(to_char(due_date,'FMMonth FMDD, YYYY'),'')
		FROM public.invoices WHERE tenant=$1 AND case_id::text=$2 AND status <> 'VOID' ORDER BY created_at`, tenant, id); err == nil {
		for rows.Next() {
			var m caseSummaryMoney
			if rows.Scan(&m.Kind, &m.Party, &m.AmountCents, &m.Status, &m.PaidOn, &m.DueOn) == nil {
				m.Label = orWord(summaryFeeLabel[m.Kind], strings.ToLower(strings.ReplaceAll(m.Kind, "_", " ")))
				if m.Kind != "INITIAL_FEE" && reviewPayer == "" {
					reviewPayer = partyWord(m.Party)
				}
				money = append(money, m)
			}
		}
		rows.Close()
	}
	if reviewPayer == "" {
		reviewPayer = partyWord(str(d, "party_billed"))
	}
	var billed, paid int64
	for _, m := range money {
		billed += m.AmountCents
		if m.Status == "PAID" || m.Status == "REFUNDED" {
			paid += m.AmountCents
		}
	}

	// The story: what it is, then where it stands, then -- when decided -- the result.
	story := []string{what}
	allPaid := len(money) > 0 && paid == billed
	before := len(story)
	switch {
	case closed && decided:
		line := "The case is complete: it has been decided"
		if allPaid {
			line += ", and every fee billed on it is paid"
		} else if billed > paid {
			line += ", with " + usdCents(billed-paid) + " in fees still unpaid"
		}
		story = append(story, line+".")
	case closed:
		line := "The case is closed (" + orWord(internal, strings.ToLower(status)) + ")."
		if eligResult == "INELIGIBLE" {
			line = "The case is closed: it was found ineligible" + map[bool]string{true: " (" + strings.ReplaceAll(eligReason, "_", " ") + ")", false: ""}[eligReason != ""] + "."
		}
		story = append(story, line)
	case nextLabel != "":
		stage = fmt.Sprintf("Step %d of %d", stepsDone+1, stepsTotal)
		line := fmt.Sprintf("Now: %s — with the %s.", nextLabel, orWord(strings.ToLower(nextOwner), "case team"))
		if internal != "" {
			line += " Status: " + internal + map[bool]string{true: " (Agency: " + agency + ")", false: ""}[agency != ""] + "."
		}
		story = append(story, line)
	case stepsTotal > 0:
		story = append(story, "All of the case's steps are done.")
	}
	nowText := ""
	if len(story) > before {
		nowText = story[len(story)-1]
	}
	eligText := ""
	if eligResult != "" && !closed {
		e := map[string]string{"ELIGIBLE": "The dispute was found eligible.", "HOLD_AOR": "Eligibility is on hold for the authorization of representation."}[eligResult]
		if eligResult == "INELIGIBLE" {
			e = "The dispute was found ineligible (" + strings.ReplaceAll(eligReason, "_", " ") + ")."
		}
		if e != "" {
			story = append(story, e)
			eligText = e
		}
	}
	var decision map[string]any
	decisionText := ""
	if decided {
		res := summaryOutcome[repOutcome]
		if res == "" && caseOutcome != "" {
			res = strings.ToLower(strings.TrimSuffix(caseOutcome, "."))
		}
		line := "Decided"
		if fo := date("date_final_order_sent"); fo != "" {
			line += " (final order sent " + fo + ")"
		}
		if res != "" {
			line += ": " + res
		}
		line += "."
		var owed *int64
		if awarded != nil {
			line += " " + usdCents(*awarded) + " awarded to the provider"
			if alreadyPaid != nil && *alreadyPaid > 0 {
				o := *awarded - *alreadyPaid
				if o < 0 {
					o = 0
				}
				owed = &o
				line += fmt.Sprintf("; the plan had already paid %s, so %s is owed", usdCents(*alreadyPaid), usdCents(o))
			}
			line += "."
		}
		if reviewPayer != "" {
			line += " The review costs are billed to " + reviewPayer + "."
		}
		story = append(story, line)
		decisionText = line
		decision = map[string]any{"outcome": orWord(repOutcome, caseOutcome), "result": res, "awarded_cents": awarded,
			"already_paid_cents": alreadyPaid, "owed_cents": owed, "review_costs_billed_to": reviewPayer,
			"final_order_sent": date("date_final_order_sent"), "recommendation_sent": date("date_recommendation_sent_to_agency")}
	}
	if len(money) > 0 {
		parts := []string{}
		for _, m := range money {
			p := fmt.Sprintf("%s %s to %s", m.Label, usdCents(m.AmountCents), orWord(partyWord(m.Party), "—"))
			switch m.Status {
			case "PAID":
				p += " — paid" + map[bool]string{true: " " + m.PaidOn, false: ""}[m.PaidOn != ""]
			case "OPEN":
				p += " — unpaid" + map[bool]string{true: ", due " + m.DueOn, false: ""}[m.DueOn != ""]
			default:
				p += " — " + strings.ToLower(strings.ReplaceAll(m.Status, "_", " "))
			}
			parts = append(parts, p)
		}
		story = append(story, "Fees: "+strings.Join(parts, "; ")+".")
	}

	// The case's outcome, by the program's own list and definitions: as recorded, or -- until someone records it --
	// what the case itself shows.
	outcomeName, recorded := caseOutcome, caseOutcome != ""
	if outcomeName == "" {
		switch {
		case closed && eligResult == "INELIGIBLE":
			outcomeName = "Ineligible - Closed"
		case strings.Contains(internal, "Withdraw"):
			outcomeName = "Withdrawn"
		case internal == "Dismissed":
			outcomeName = "Dismissed"
		case internal == "Plan Opt-Out":
			outcomeName = "Plan Opt-Out"
		case internal == "Closed-Refunded":
			outcomeName = "Refunded"
		case repOutcome == "OVERTURNED":
			outcomeName = "Full Award"
		case repOutcome == "PARTIALLY_OVERTURNED":
			outcomeName = "Partial Award"
		case repOutcome == "UPHELD":
			outcomeName = "No Award"
		case !closed:
			outcomeName = "TBD - In Process"
		}
	}
	outcomeDef, outcomeGroup := "", ""
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		outcomeDef = cfg.OutcomeDefinitions[outcomeName]
		outcomeGroup = cfg.outcomeGroupOf(outcomeName)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"outcome":     map[string]any{"name": outcomeName, "definition": outcomeDef, "recorded": recorded, "group": outcomeGroup},
		"case_number": num, "story": story, "stage": stage, "closed": closed, "decided": decided,
		// The same words, piece by piece, for a page that lays them out: what the dispute is, where it stands, the
		// eligibility finding and the decision.
		"what": what, "now": nowText, "eligibility_text": eligText, "decision_text": decisionText,
		"next": map[string]any{"label": nextLabel, "owner": nextOwner}, "steps_done": stepsDone, "steps_total": stepsTotal,
		"eligibility": map[string]any{"result": eligResult, "reason": eligReason}, "decision": decision,
		"money": money, "billed_cents": billed, "paid_cents": paid,
		"parties": map[string]any{"provider": provider, "health_plan": plan, "filed_by_plan": filerIsPlan, "claims": nClaims},
	})
}
