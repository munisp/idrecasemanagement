package main

// workflowdoc.go — a case's steps as the office's "Streamline Workflows, Timelines, and Outcomes" document numbers
// them: the workflow the case is on (Acceptance with or without a cost estimate, Ineligible, Plan Opt-Out,
// Withdrawal, Dismissal, Default, Full Review), each of its numbered steps in the document's words, the tasks
// (wfStep) that carry each one out, and the clock that times it, so every step shows its own countdown.

import "strings"

type wfDocStep struct {
	N      int        `json:"n,omitempty"`      // the document's number; 0 for the intake before it
	Phase  string     `json:"phase,omitempty"`  // Phase 1: Intake & Eligibility ... (Full Review and Default)
	Title  string     `json:"title"`            // the document's words
	Detail string     `json:"detail,omitempty"` // its sub-point (the timeframe)
	Keys   []string   `json:"keys"`             // the tasks under it
	State  string     `json:"state"`            // done | next | upcoming | skipped | optional
	Note   string     `json:"note,omitempty"`   // what the case says about it
	Clock  string     `json:"clock,omitempty"`  // the program clock that times it
	View   *ClockView `json:"clock_view,omitempty"`
	Due    string     `json:"due,omitempty"` // a due date that is not a program clock (the invoice's)
	done   bool       // for a step with no task of its own: whether the case shows it happened
}

type wfDoc struct {
	Workflow string      `json:"workflow"` // the document's heading for the path the case is on
	Steps    []wfDocStep `json:"steps"`
}

const (
	phase1 = "Phase 1: Intake & Eligibility"
	phase2 = "Phase 2: Clinical & Coding Review"
	phase3 = "Phase 3: Legal & Quality Review"
	phase4 = "Phase 4: Determination & Final Order"
)

// buildDocSteps lays the document's workflow over the case's tasks. Pure: the facts and the built steps in.
func buildDocSteps(f wfFacts, steps []wfStep) wfDoc {
	state := map[string]string{}
	for _, st := range steps {
		state[st.Key] = st.State
	}
	letter := func(tpl string) []string { return []string{"letter:" + tpl, "qa:" + tpl, "send:" + tpl} }
	keys := func(groups ...[]string) []string {
		var out []string
		for _, g := range groups {
			out = append(out, g...)
		}
		return out
	}
	_, withdrawn := f.Act["PROVIDER_WITHDRAW"]
	inelig := f.HasElig && f.EligResult == "INELIGIBLE"
	estimate := wfEstimateRequested(f)
	_, permitted := f.Act["ESTIMATE_PERMISSION"]
	dismissed := (permitted && !wfFlag(f, "ESTIMATE_PERMISSION", "granted")) || f.Letter["dismissal"] != "" || f.Internal == "Dismissed"
	optedOut := wfOptedOut(f)
	defaulted := f.Internal == "Plan - No Response" || strings.Contains(f.Internal, "Default")
	responded := f.Dates["date_plan_responded"] != ""
	closed := wfClosedStatuses[f.Internal]
	_, rfi := f.Act["RFI_SENT"]

	var ds []wfDocStep
	add := func(d wfDocStep) { ds = append(ds, d) }
	closeCase := func() {
		add(wfDocStep{Title: "Case closed", Detail: "No invoice generated", done: closed})
	}

	add(wfDocStep{Title: "Initial fee paid and filing packet complete", Keys: []string{"fee", "packet"}})
	eligKeys := []string{"eligibility", "aor_confirm", "aor_revised", "eligibility_again"}
	add(wfDocStep{Phase: phase1, Title: "Eligibility Review", Detail: "Complete within 13 calendar days", Keys: eligKeys, Clock: "INITIAL_REVIEW"})

	workflow := ""
	switch {
	case inelig:
		workflow = "3. Ineligible Workflow"
		add(wfDocStep{Title: "Case deemed Ineligible", done: true})
		add(wfDocStep{Title: "Ineligibility Letter issued", Detail: "On or before day 13", Keys: keys(letter("ineligible"), letter("dismissal")), Clock: "INITIAL_REVIEW"})
		closeCase()
	default:
		add(wfDocStep{Title: "Case deemed Eligible", Keys: []string{"dispute_summary"}, done: f.HasElig && f.EligResult == "ELIGIBLE"})
		note := ""
		if _, ok := f.Act["ESTIMATE_CONFIRMED"]; ok || f.Letter["acceptance"] != "" {
			note = map[bool]string{true: "Cost estimate requested: Yes", false: "Cost estimate requested: No"}[estimate]
		}
		conf := wfDocStep{Title: "Confirm if Cost Estimate requested", Keys: []string{"estimate_confirm"}, Note: note}
		if !estimate {
			conf.Detail = "If no cost estimate is requested, no cost estimate email is sent and the cost estimate language is removed from the Acceptance letter and email"
		}
		add(conf)
		if estimate {
			add(wfDocStep{Title: "Acceptance and Cost Estimate issued", Detail: "On or before day 13",
				Keys: keys(letter("acceptance"), []string{"estimate"}, letter("estimate_cost")), Clock: "INITIAL_REVIEW"})
			add(wfDocStep{Title: "Provider submits authorization/permission", Detail: "Within 15 calendar days", Keys: []string{"permission"}, Clock: "PROVIDER_PERMISSION"})
			if dismissed && !withdrawn {
				workflow = "6. Dismissal Workflow"
				add(wfDocStep{Title: "No authorization received", done: true})
				add(wfDocStep{Title: "Dismissal Letter issued", Keys: letter("dismissal")})
				closeCase()
				break
			}
			add(wfDocStep{Title: "Health Plan Notification sent", Detail: "Same day or next business day", Keys: letter("plan_notification"), Clock: "PLAN_NOTIFICATION"})
		} else {
			add(wfDocStep{Title: "Acceptance and Health Plan Notification issued", Detail: "On or before day 13",
				Keys: keys(letter("acceptance"), letter("plan_notification")), Clock: "ACCEPTANCE_PLAN_NOTICE"})
		}
		switch {
		case withdrawn:
			workflow = "5. Withdrawal Workflow"
			add(wfDocStep{Title: "Provider or Health Plan submits withdrawal request", done: true})
			add(wfDocStep{Title: "Withdrawal request verified and confirmed by Capitol Bridge", Keys: []string{"withdrawal_confirm"}})
			add(wfDocStep{Title: "Withdrawal Letter issued", Keys: letter("withdrawal")})
			closeCase()
		case optedOut:
			workflow = "4. Plan Opt-Out Workflow"
			add(wfDocStep{Title: "Health Plan response received", Detail: "Within 15 calendar days", Keys: []string{"plan_response"}, Clock: "PLAN_RESPONSE"})
			add(wfDocStep{Title: "Health Plan elects to opt out and submits supporting documentation", done: true})
			add(wfDocStep{Title: "Opt-Out Determination Letter issued", Detail: "Same day or next business day", Keys: letter("plan_opt_out"), Clock: "OPT_OUT_LETTER"})
			closeCase()
		case workflow != "": // dismissed
		case defaulted:
			workflow = "7. Default Workflow"
			add(wfDocStep{Title: "Health Plan response period expires", Detail: "15 calendar days; a reminder of the due date is sent on the 13th day",
				Keys: []string{"plan_response"}, Clock: "PLAN_RESPONSE"})
			add(wfDocStep{Title: "No response received from Health Plan", done: true})
			add(wfDocStep{Title: "Default Determination issued",
				Keys: keys([]string{"coding_review", "clinical_review", "attorney_review", "invoice", "report_save", "report_generate",
					"report_cover_letter", "report_plan_invoice"}, letter("final_order")), Clock: "AGENCY_RECOMMENDATION"})
			add(wfDocStep{Phase: phase4, Title: "Await Final Order from AHCA", Detail: "Typically issued within 30 days",
				Keys: keys([]string{"final_order_issued"}, letter("final_order_rationale_copy"), []string{"outcome"}), Clock: "FINAL_ORDER"})
			add(wfDocStep{Title: "Invoice cycle begins", Detail: "35-day payment timeframe", Keys: []string{"payment"}})
		default:
			workflow = map[bool]string{true: "1. Acceptance Workflow (Cost Estimate Requested)", false: "2. Acceptance Workflow (No Cost Estimate Requested)"}[estimate]
			if responded {
				workflow = "8. Full Review Workflow"
			}
			add(wfDocStep{Title: "Health Plan response period", Detail: "Up to 15 calendar days; a reminder of the due date on the 13th day",
				Keys: []string{"plan_response"}, Clock: "PLAN_RESPONSE"})
			add(wfDocStep{Title: "Health Plan submits required documentation", done: responded})
			add(wfDocStep{Phase: phase2, Title: "Full Review Initiated", Detail: "Review clock begins upon receipt of complete Health Plan packet", done: responded})
			add(wfDocStep{Title: "Nurse/Coder review begins", Detail: "Approximately 15 days after receipt of plan packet", Clock: "REVIEW_START",
				done: f.Dates["coding_review_started_at"] != "" || f.Dates["clinical_review_started_at"] != "" || state["coding_review"] == "done"})
			add(wfDocStep{Title: "Requests for Information (RFI) issued as needed", State: "optional",
				Note: map[bool]string{true: "An RFI was sent", false: ""}[rfi]})
			add(wfDocStep{Title: "Clinical/Coding review completed", Detail: "Target review period: 5-6 days",
				Keys: []string{"coding_review", "clinical_review"}, Clock: "CLINICAL_CODING_REVIEW"})
			attorneyClock := "ATTORNEY_START"
			if f.Dates["attorney_review_started_at"] != "" {
				attorneyClock = "ATTORNEY_REVIEW"
			}
			add(wfDocStep{Phase: phase3, Title: "Attorney review", Detail: "Begins approximately 30 days after receipt of plan packet; target review period: 5-6 days",
				Keys: []string{"attorney_review"}, Clock: attorneyClock})
			add(wfDocStep{Title: "Program Manager (PM) review", Detail: "Target review period: 5 days",
				Keys: []string{"invoice", "report_save", "report_generate", "report_cover_letter", "report_plan_invoice"}, Clock: "PM_REVIEW"})
			add(wfDocStep{Phase: phase4, Title: "Determination submitted to AHCA", Detail: "Issued on or before the 60-day turnaround time requirement",
				Keys: letter("final_order"), Clock: "AGENCY_RECOMMENDATION"})
			add(wfDocStep{Title: "Await Final Order from AHCA", Detail: "Typically issued within 30 days",
				Keys: keys([]string{"final_order_issued"}, letter("final_order_rationale_copy"), []string{"outcome"}), Clock: "FINAL_ORDER"})
			add(wfDocStep{Title: "Invoice cycle begins", Detail: "35-day payment timeframe", Keys: []string{"payment"}})
		}
	}
	if workflow == "" {
		workflow = map[bool]string{true: "1. Acceptance Workflow (Cost Estimate Requested)", false: "2. Acceptance Workflow (No Cost Estimate Requested)"}[estimate]
	}
	// Phases are the Full Review and Default workflows' own; the short workflows are one list.
	phased := strings.HasPrefix(workflow, "1.") || strings.HasPrefix(workflow, "2.") || strings.HasPrefix(workflow, "7.") || strings.HasPrefix(workflow, "8.")
	out := wfDoc{Workflow: workflow}
	n := 0
	for i, d := range ds {
		// Only the tasks the case has: a step whose tasks are not on this case's list (an AOR hold not taken)
		// keeps the ones it has; one with none at all is shown only when it stands for something that happens.
		var have []string
		for _, k := range d.Keys {
			if _, ok := state[k]; ok {
				have = append(have, k)
			}
		}
		if len(d.Keys) > 0 && len(have) == 0 {
			continue
		}
		d.Keys = have
		if !phased {
			d.Phase = ""
		}
		if i > 0 {
			n++
			d.N = n
		}
		if d.State == "" {
			d.State = docState(d, state)
		}
		out.Steps = append(out.Steps, d)
	}
	if out.Steps == nil {
		out.Steps = []wfDocStep{}
	}
	return out
}

// docState: a step with tasks is as far as its tasks are; one without is done when the case shows it happened.
func docState(d wfDocStep, state map[string]string) string {
	if len(d.Keys) == 0 {
		if d.done {
			return "done"
		}
		return "upcoming"
	}
	done, skipped := 0, 0
	for _, k := range d.Keys {
		switch state[k] {
		case "next":
			return "next"
		case "done":
			done++
		case "skipped":
			skipped++
		}
	}
	switch {
	case done+skipped == len(d.Keys) && done > 0:
		return "done"
	case skipped == len(d.Keys):
		return "skipped"
	}
	return "upcoming"
}
