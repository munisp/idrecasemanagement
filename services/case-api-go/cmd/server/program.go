// Author: Patrick Munis at newwave
package main

// Program rules engine — per-tenant configuration that lets each state run its
// own dispute program (clocks, statuses, eligibility thresholds, numbering,
// fees, escalation thresholds, correspondence templates, deliverables) without
// code changes. Federal NSA remains the built-in default when a tenant has no
// program_rules row; FL AHCA CDR is seeded in scripts/program-rules.sql.
//
// Gaps closed here: G1 (clock packs), G2 (eligibility), G5 (dual status),
// G6 (escalation auto-trigger), G11 (numbering).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type FollowUp struct {
	Day    int    `json:"day"`
	Action string `json:"action"`
}

type ClockRule struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Basis string `json:"basis"` // key into program_dates (received_at, plan_notified_at, …)
	Days  int    `json:"days"`
	// MaxDays: the contract's hard limit when it is longer than the target
	// ("60 days, never more than 90"; "10 days, +3 for notification").
	// Past Days the clock is late; past MaxDays it is in breach.
	MaxDays  int        `json:"max_days"`
	DayType  string     `json:"day_type"` // calendar|business
	Cite     string     `json:"cite"`
	Breach   string     `json:"breach"`
	FollowUp []FollowUp `json:"follow_ups"`
	// StopOn: the program dates any one of which means the step was done. Once one is set the clock is met -- on time
	// or late -- and stops counting down. Without it a clock ran on after its step was done.
	StopOn []string `json:"stop_on,omitempty"`
	// Target: a working target (how long a step usually takes), not a contract deadline -- shown, never a breach.
	Target bool `json:"target,omitempty"`
}

// FeeStep: from EffectiveFrom on, the fee is Cents. EffectiveFrom is a day (YYYY-MM-DD, from midnight) or a day
// and time (YYYY-MM-DDTHH:MM), in the program's time zone. CardFeeCents (initial fees only) is the credit card
// transaction fee charged on top of Cents when the fee is paid by card; nothing is added for a bank account or check.
type FeeStep struct {
	EffectiveFrom string `json:"effective_from"`
	Cents         int64  `json:"cents"`
	CardFeeCents  int64  `json:"card_fee_cents,omitempty"`
}

// LateFee: a review invoice not paid within AfterDays of the case's Final Order is charged Cents on day
// AfterDays+1, and Cents again every EveryDays after that until it is paid (AHCA price sheet, line 7).
type LateFee struct {
	Cents     int64 `json:"cents"`
	AfterDays int   `json:"after_days"`
	EveryDays int   `json:"every_days"`
}

// lateChargesDue is how many late charges a review invoice owes daysSince days after its Final Order.
func (l *LateFee) lateChargesDue(daysSince int) int {
	if l == nil || l.Cents <= 0 || l.AfterDays < 0 || daysSince <= l.AfterDays {
		return 0
	}
	if l.EveryDays <= 0 {
		return 1
	}
	return 1 + (daysSince-l.AfterDays-1)/l.EveryDays
}

// stepStart is the moment a step takes effect, read in loc.
func stepStart(v string, loc *time.Location) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, strings.TrimSpace(v), loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// feeAt is the scheduled fee in force at the moment now (the latest step that has started), and whether the
// schedule covers that moment.
func feeAt(schedule []FeeStep, now time.Time, loc *time.Location) (int64, bool) {
	st, ok := stepAt(schedule, now, loc)
	return st.Cents, ok
}

// stepAt is the schedule's entry in force at now, and whether one is.
func stepAt(schedule []FeeStep, now time.Time, loc *time.Location) (FeeStep, bool) {
	var best time.Time
	var cur FeeStep
	found := false
	for _, st := range schedule {
		start, ok := stepStart(st.EffectiveFrom, loc)
		if !ok || start.After(now) {
			continue
		}
		if !found || start.After(best) {
			best, cur, found = start, st, true
		}
	}
	return cur, found
}

// location is the program's time zone (time_zone, an IANA name such as America/New_York): the clock fees and
// "today" are read on. Eastern when none is set.
func (c *ProgramConfig) location() *time.Location {
	if c != nil && c.TimeZone != "" {
		if loc, err := time.LoadLocation(c.TimeZone); err == nil {
			return loc
		}
	}
	return programZone("")
}

// feeOn is the scheduled fee in force on day (YYYY-MM-DD), and whether the schedule covers that day.
func feeOn(schedule []FeeStep, day string) (int64, bool) {
	best, found := "", false
	var cents int64
	for _, st := range schedule {
		if st.EffectiveFrom <= day && (!found || st.EffectiveFrom > best) {
			best, cents, found = st.EffectiveFrom, st.Cents, true
		}
	}
	return cents, found
}

type ThresholdRule struct {
	ProviderType string `json:"provider_type"`
	Contracted   *bool  `json:"contracted,omitempty"`
	MinCents     int64  `json:"min_cents"`
}

type ProgramConfig struct {
	// TimeZone: the program's clock (IANA name). Fees take effect, and "today" turns over, on this clock.
	TimeZone   string `json:"time_zone,omitempty"`
	CaseNumber struct {
		Pattern string `json:"pattern"` // e.g. "FL{yy}-{seq}"
		SeqPad  int    `json:"seq_pad"`
	} `json:"case_number"`
	Statuses struct {
		Internal []string `json:"internal"`
		Agency   []string `json:"agency"`
	} `json:"statuses"`
	Clocks      []ClockRule `json:"clocks"`
	Eligibility struct {
		Thresholds         []ThresholdRule `json:"thresholds"`
		FilingWindowMonths int             `json:"filing_window_months"`
		ProofOfTimeliness  []string        `json:"proof_of_timeliness"`
		Reasons            []string        `json:"ineligibility_reasons"`
		// ServicesAfter (YYYY-MM-DD): the program covers services rendered
		// strictly after this date. Empty = no floor.
		ServicesAfter string `json:"services_after"`
		// DisputeTypes: the kinds of claim dispute the program accepts
		// (denied in full/part, underpaid, overpaid). Empty = not checked.
		DisputeTypes []string `json:"dispute_types"`
	} `json:"eligibility"`
	Time struct {
		QuarterHours bool `json:"quarter_hours"` // FL: time recorded in decimal quarter hours only
	} `json:"time"`
	Fees struct {
		// InitialFeeCents is the fee in force today: loadProgram sets it from InitialFeeSchedule, so every filing is
		// charged the fee of the day it is filed. Without a schedule it is the single stored fee.
		InitialFeeCents int64 `json:"initial_fee_cents"`
		// InitialFeeSchedule: the fee and the day it takes effect, one entry per change (the program sets it each
		// year). The latest entry on or before a day is that day's fee.
		InitialFeeSchedule []FeeStep `json:"initial_fee_schedule,omitempty"`
		RefundWindowDays   int       `json:"refund_window_days"`
		InvoiceDueDays     *int      `json:"invoice_due_days"`
		// ReminderLeadDays: how many days before an invoice's due date the
		// Payment Reminder is drafted (default 7).
		ReminderLeadDays *int `json:"reminder_lead_days"`
		// InvoicedFilerOrgs are filing organizations accepted under a bulk
		// invoicing arrangement (AHCA 2026 workflow, "Payment before
		// submission"): their disputes open immediately on bulk acceptance
		// and the initial fee is invoiced later instead of collected up
		// front. Membership in this list is a fee-config change, so it is
		// protected by the migration-only guard trigger like the fee itself.
		InvoicedFilerOrgs []string `json:"invoiced_filer_orgs"`
		// LateFee: what an unpaid review invoice is charged after the Final Order (none when unset).
		LateFee *LateFee `json:"late_fee,omitempty"`
	} `json:"fees"`
	Escalation struct {
		AmountTriggerCents int64    `json:"amount_trigger_cents"`
		RouteRole          string   `json:"route_role"`
		Reasons            []string `json:"reasons"`
	} `json:"escalation"`
	NotesStreams     []string            `json:"notes_streams"`
	NotesStreamRoles map[string][]string `json:"notes_stream_roles"`
	FieldSchema      map[string][]string `json:"field_schema"`
	// OutcomeDefinitions: what each case outcome (field_schema.case_outcome) means, in the office's words.
	OutcomeDefinitions map[string]string `json:"outcome_definitions,omitempty"`
	// OutcomeGroups: the outcome structure (Open Cases, Closed Without Review, Decided With Determination,
	// Administrative Closure), each with the outcomes it holds.
	OutcomeGroups []OutcomeGroup      `json:"outcome_groups,omitempty"`
	Checklist     map[string][]string `json:"checklist"` // stage -> items; overrides the federal NSA default
	// ChecklistOwners: checklist item -> the role that ticks it. An item
	// with no owner here can be ticked by any case staff, as before.
	ChecklistOwners map[string]string `json:"checklist_owners"`
	// Correspondence was never on this struct, so getProgram's response
	// (what the portal's template dropdown reads) never carried it --
	// corrTemplates() below did its own separate raw DB read instead,
	// which is why sending correspondence always worked from server-side
	// code but the dropdown itself had always been empty, unnoticed until
	// the first real browser test of it this session.
	Correspondence struct {
		AgencyRecipients []string       `json:"agency_recipients"`
		Templates        []CorrTemplate `json:"templates"`
	} `json:"correspondence"`
	// The letters lettergen builds ({key, subject, filename, folder}) and the office's standard letterhead
	// wording -- the case page's Letters list and the Letterhead & invoice card read them.
	LetterTemplates []map[string]any `json:"letter_templates"`
	Letters         map[string]any   `json:"letters"`
	Billing         BillingConfig    `json:"billing"`        // service-fee invoicing engine (billing.go)
	Reconciliation  ReconConfig      `json:"reconciliation"` // recon engine + accounting adapters (recon.go)
	Bank            BankConfig       `json:"bank"`           // lockbox webhook + NACHA origination (bank.go)
}

// loadProgram returns nil when the tenant runs the built-in federal NSA program.
func (s *server) loadProgram(r *http.Request, tenant string) *ProgramConfig {
	var raw []byte
	err := s.db.QueryRow(r.Context(),
		`SELECT config FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw)
	if err != nil {
		return nil
	}
	var cfg ProgramConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	if fee, ok := feeAt(cfg.Fees.InitialFeeSchedule, time.Now(), cfg.location()); ok {
		cfg.Fees.InitialFeeCents = fee
	}
	return &cfg
}

// getProgram exposes the tenant's program config so the portal can render
// program-specific labels, statuses, and rules without hard-coding a state.
// tenant_label is looked up separately (public.tenant_labels, not the
// program config) and included either way -- it applies to every tenant,
// configured-program or Federal-NSA-default alike.
func (s *server) getProgram(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	cfg := s.loadProgram(r, tenant)
	var label string
	_ = s.db.QueryRow(r.Context(), `SELECT label FROM public.tenant_labels WHERE tenant=$1`, tenant).Scan(&label)
	brand := s.tenantBrand(r.Context(), tenant)
	if cfg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"program": "FEDERAL_NSA", "config": nil, "tenant_label": label, "brand_name": brand, "features": s.tenantFeatures(r, tenant)})
		return
	}
	// The templates the portal offers: the standard ones as edited on the
	// Templates screen, plus the ones added there, minus the retired.
	offered := []CorrTemplate{}
	for _, t := range s.corrTemplates(r, tenant) {
		if !t.Inactive {
			offered = append(offered, t)
		}
	}
	cfg.Correspondence.Templates = offered
	if p, ok := r.Context().Value(ctxPrincipal{}).(principal); ok && isExternalOnly(p) {
		cfg = publicProgram(cfg)
	}
	writeJSON(w, http.StatusOK, map[string]any{"program": "custom", "config": cfg, "tenant_label": label, "brand_name": brand, "features": s.tenantFeatures(r, tenant)})
}

// publicProgram is what an outside filer may know about the program: how
// cases are numbered, the public eligibility rules, the fee, and the status
// names. The rest -- banking, billing, reconciliation, letter wording and
// recipients, internal checklists and their owners -- is the office's own.
func publicProgram(cfg *ProgramConfig) *ProgramConfig {
	out := &ProgramConfig{}
	out.CaseNumber = cfg.CaseNumber
	out.Statuses = cfg.Statuses
	out.Eligibility = cfg.Eligibility
	out.Fees = cfg.Fees
	return out
}

// setTenantLabel: PUT /v1/tenants/{tenant}/program/label {"label":"AHCA"} --
// FEDERAL_ADMIN/PLATFORM_ADMIN only (same guard as program rules). Lives in
// its own table, not program_rules.config: a tenant with no custom program
// rules runs on Federal NSA defaults, and every "programmed vs federal"
// branch in this codebase (summaryReport, the dashboard's amount field,
// clock projection, ...) keys off whether that row exists at all --
// storing the label there would force creating one just to hold a name,
// silently flipping a tenant's program mode as a side effect. This way
// works for every tenant regardless of program status.
func (s *server) setTenantLabel(w http.ResponseWriter, r *http.Request) {
	p, ok := s.rulesAdminGuard(w, r)
	if !ok {
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		http.Error(w, `{"error":"label required"}`, http.StatusBadRequest)
		return
	}
	_, err := s.db.Exec(r.Context(), `
		INSERT INTO public.tenant_labels (tenant, label, updated_by, updated_at)
		VALUES ($1,$2,$3,now())
		ON CONFLICT (tenant) DO UPDATE SET label=$2, updated_by=$3, updated_at=now()`,
		tenant, label, displayName(p))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logActivity(r.Context(), tenant, "", "TENANT_LABEL_UPDATED",
		fmt.Sprintf("Tenant display label set to %q by %s", label, displayName(p)))
	s.logAudit(r.Context(), tenant, "", "TENANT_LABEL_UPDATED", map[string]any{"by": p.Subject, "label": label})
	writeJSON(w, http.StatusOK, map[string]string{"tenant_label": label})
}

// listTenantLabels: GET /v1/tenants/{tenant}/admin/tenant-labels -- powers
// the cross-tenant switcher (PLATFORM_ADMIN/FEDERAL_ADMIN/STATE_AUDITOR,
// the same roles that see it at all) so it can show "AHCA" instead of a
// generic "FL IDRE" for any tenant that has a label configured. The
// {tenant} segment in the URL is unused -- this spans every tenant, same
// convention as adminListTPAs.
func (s *server) listTenantLabels(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "PLATFORM_ADMIN", "FEDERAL_ADMIN", "STATE_AUDITOR") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT tenant, label FROM public.tenant_labels`)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var tenant, label string
		if rows.Scan(&tenant, &label) == nil {
			out[tenant] = label
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// programDates reads the case's program-specific date bag.
func (s *server) programDates(r *http.Request, tenant, caseID string) map[string]string {
	var raw []byte
	err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT program_dates FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)),
		caseID).Scan(&raw)
	if err != nil {
		return map[string]string{}
	}
	out := map[string]string{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// projectProgramClocks runs the generic date-driven clock engine over a case's
// program_dates. Calendar-day and business-day rules both supported.
func projectProgramClocks(rules []ClockRule, dates map[string]string, today time.Time, ext ...map[string]int) []ClockView {
	extended := map[string]int{} // clock → days the PM extended it (deadline_extensions.go)
	if len(ext) > 0 && ext[0] != nil {
		extended = ext[0]
	}
	out := []ClockView{}
	for _, rl := range rules {
		raw, ok := dates[rl.Basis]
		if !ok || raw == "" {
			continue // clock not started — basis event hasn't happened
		}
		basis, err := time.Parse("2006-01-02", raw[:minI(10, len(raw))])
		if err != nil {
			continue
		}
		var due time.Time
		var remaining int
		extra := extended[rl.Name]
		if rl.DayType == "business" {
			due = addBusinessDays(basis, rl.Days+extra)
			remaining = businessDaysBetween(today, due)
		} else {
			due = basis.AddDate(0, 0, rl.Days+extra)
			remaining = int(due.Sub(today).Hours() / 24)
		}
		cv := ClockView{
			Clock: rl.Name, Label: rl.Label, Basis: rl.DayType,
			TotalDays: rl.Days, Remaining: remaining,
			Due: due.Format("2006-01-02"), State: clockState(remaining, rl.Days+extra),
			ExtendedDays: extra, Extendable: !nonExtendableClocks[rl.Name],
			Cite:      rl.Cite,
			BasisNote: fmt.Sprintf("from %s %s", rl.Basis, raw[:minI(10, len(raw))]),
		}
		// Met: the step's own date is set. Its clock stops, saying whether it was on time.
		for _, k := range rl.StopOn {
			v := dates[k]
			if len(v) < 10 {
				continue
			}
			if met, err := time.Parse("2006-01-02", v[:10]); err == nil && !met.Before(basis) {
				cv.MetOn = met.Format("2006-01-02")
				cv.State = "met"
				if met.After(due) {
					cv.State = "met_late"
				}
				break
			}
		}
		if cv.MetOn != "" {
			out = append(out, cv)
			continue
		}
		// A working target past its day is "over target", never a breach.
		if rl.Target && remaining < 0 {
			cv.State = "over_target"
		}
		cv.Target = rl.Target
		// Past the target but inside the contract's hard limit is "late",
		// not yet a breach.
		if rl.MaxDays > rl.Days {
			hard := basis.AddDate(0, 0, rl.MaxDays+extra)
			if rl.DayType == "business" {
				hard = addBusinessDays(basis, rl.MaxDays+extra)
			}
			cv.HardDue = hard.Format("2006-01-02")
			if remaining < 0 && !today.After(hard) {
				cv.State = "late"
			}
		}
		out = append(out, cv)
	}
	return out
}

// caseProgramClocks: generic program clock projection (used when the tenant
// has a program_rules row; otherwise the federal NSA engine in engines.go runs).
func (s *server) caseProgramClocks(w http.ResponseWriter, r *http.Request, tenant, caseID string, cfg *ProgramConfig) bool {
	writeJSON(w, http.StatusOK, s.programClockViews(r, tenant, caseID, cfg))
	return true
}

// programClockViews: the case's program clocks, as the Clocks panel and the Steps show them.
func (s *server) programClockViews(r *http.Request, tenant, caseID string, cfg *ProgramConfig) []ClockView {
	dates := s.programDates(r, tenant, caseID)
	// "Date Received" is the day the fee was paid: a case still awaiting payment has not been received, so no
	// clock that runs from it has started (and none can breach) until the fee is paid.
	var status string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&status)
	if status == "AWAITING_PAYMENT" {
		delete(dates, "received_at")
	} else if _, ok := dates["received_at"]; !ok {
		// received_at falls back to opened_at (federal field) when unset
		var opened time.Time
		if err := s.db.QueryRow(r.Context(),
			fmt.Sprintf(`SELECT opened_at FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
			Scan(&opened); err == nil {
			dates["received_at"] = opened.Format("2006-01-02")
		}
	}
	// Truncate to midnight -- due dates are always midnight (parsed from
	// date-only strings / computed via AddDate on a midnight basis), so an
	// untruncated time.Now() carrying the live time-of-day under-counts
	// "remaining" by close to a full day depending on when the clock is
	// checked (confirmed live: FL's 60-day AGENCY_RECOMMENDATION clock was
	// off by one). engines.go's federal-default clock path already
	// truncates the same way (casesClocks/caseClocks) -- this path never
	// matched it.
	ext, extList := s.clockExtensions(r.Context(), tenant, caseID)
	views := projectProgramClocks(cfg.Clocks, dates, time.Now().Truncate(24*time.Hour), ext)
	for i := range views {
		views[i].Extensions = extList[views[i].Clock]
	}
	return views
}

// setProgramDate records a program-specific date (starts/restarts a clock).
func (s *server) setProgramDate(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	var in struct {
		Key   string `json:"key"`   // e.g. plan_notified_at, estimate_sent_at
		Value string `json:"value"` // YYYY-MM-DD
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Key == "" || in.Value == "" {
		http.Error(w, `{"error":"key and value required"}`, http.StatusBadRequest)
		return
	}
	if _, err := time.Parse("2006-01-02", in.Value); err != nil {
		http.Error(w, `{"error":"value must be YYYY-MM-DD"}`, http.StatusBadRequest)
		return
	}
	// The workflow records Date Received when it starts; for a payment-gated intake that is too early -- the
	// date is the day the fee is paid, and activatePaidIntake records it then.
	if in.Key == "received_at" {
		var status string
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), chi.URLParam(r, "caseId")).Scan(&status)
		if status == "AWAITING_PAYMENT" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "deferred", "reason": "Date Received is recorded when the initial fee is paid"})
			return
		}
	}
	// jsonb_build_object is polymorphic ("any"), so pgx's prepared-statement
	// protocol can't infer $2/$3's type on its own (confirmed live: SQLSTATE
	// 42P18 "could not determine data type of parameter $2") -- psql's
	// literal substitution and an explicitly-typed PREPARE both sidestep
	// this, which is why it looked fine testing by hand. Explicit casts are
	// the standard fix.
	res, err := s.db.Exec(r.Context(),
		fmt.Sprintf(`UPDATE tenant_%s.cases SET program_dates = program_dates || jsonb_build_object($2::text, $3::text), updated_at=now() WHERE id=$1`,
			sanitizeTenant(tenant)), id, in.Key, in.Value)
	if err != nil {
		slog.Error("setProgramDate db exec failed", "tenant", tenant, "case_id", id, "key", in.Key, "err", err)
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if res.RowsAffected() == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	s.logActivity(r.Context(), tenant, id, "PROGRAM_DATE", fmt.Sprintf("%s recorded as %s", in.Key, in.Value))
	s.logAudit(r.Context(), tenant, id, "PROGRAM_DATE_SET", map[string]any{
		"by": r.Context().Value(ctxPrincipal{}).(principal).Subject, "key": in.Key, "value": in.Value,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// setDualStatus updates internal and/or agency status (G5).
func (s *server) setDualStatus(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	var in struct {
		Internal string `json:"internal_status"`
		Agency   string `json:"agency_status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || (in.Internal == "" && in.Agency == "") {
		http.Error(w, `{"error":"internal_status and/or agency_status required"}`, http.StatusBadRequest)
		return
	}
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		// Matched without regard to case and stored as the program spells
		// it: the workflow sent "Determination sent to FL" against a list
		// that says "Determination Sent to FL", was refused, and retried
		// forever -- no case could reach that status.
		var ok bool
		if in.Internal != "" {
			if in.Internal, ok = canonStatus(cfg.Statuses.Internal, in.Internal); !ok {
				http.Error(w, `{"error":"unknown internal status for this program"}`, http.StatusBadRequest)
				return
			}
		}
		if in.Agency != "" {
			if in.Agency, ok = canonStatus(cfg.Statuses.Agency, in.Agency); !ok {
				http.Error(w, `{"error":"unknown agency status for this program"}`, http.StatusBadRequest)
				return
			}
		}
	}
	var matched int64
	if in.Internal != "" {
		var oldInternal string
		_ = s.db.QueryRow(r.Context(),
			fmt.Sprintf(`SELECT coalesce(internal_status,'') FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), id).Scan(&oldInternal)
		res, _ := s.db.Exec(r.Context(),
			fmt.Sprintf(`UPDATE tenant_%s.cases SET internal_status=$2, updated_at=now() WHERE id=$1`, sanitizeTenant(tenant)), id, in.Internal)
		matched += res.RowsAffected()
		if res.RowsAffected() > 0 {
			s.recordTransition(r.Context(), tenant, id, axisProgram, oldInternal, in.Internal, p.Subject, "status_update")
			// SOP outcomes: a case closed without review takes the outcome its closure means; a plan that never
			// responded makes this a default case, for good (the status moves on, the path doesn't).
			s.recordClosureOutcome(r.Context(), tenant, id, in.Internal)
			if in.Internal == "Plan - No Response" {
				s.setProgramDateToday(r.Context(), tenant, id, "plan_no_response_at")
			}
		}
	}
	if in.Agency != "" {
		res, _ := s.db.Exec(r.Context(),
			fmt.Sprintf(`UPDATE tenant_%s.cases SET agency_status=$2, updated_at=now() WHERE id=$1`, sanitizeTenant(tenant)), id, in.Agency)
		matched += res.RowsAffected()
	}
	if matched == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	s.logActivity(r.Context(), tenant, id, "STATUS_CHANGE",
		fmt.Sprintf("Status updated by %s — internal: %s, agency: %s", p.Subject, orDash(in.Internal), orDash(in.Agency)))
	s.logAudit(r.Context(), tenant, id, "CASE_STATUS_CHANGED", map[string]any{
		"by": p.Subject, "internal_status": in.Internal, "agency_status": in.Agency,
	})
	// The workflow moves a case by setting its status (here), not through the step signals: a case it has brought
	// to a specialist's step goes in that queue now, not the next time someone happens to open the case.
	s.ensureStepHelper(r, tenant, id)
	// Warn-only eligibility gate (G2 hardening): moving past the initial
	// review phase without an ELIGIBLE decision on record is legal risk —
	// billing downstream of a wrong "proceed" is unlawful — so the response
	// carries a loud warning. It does NOT block: dismissals, withdrawals and
	// opt-outs legitimately skip eligibility, and blocking them would strand
	// those cases.
	var warnings []string
	if cfg := s.loadProgram(r, tenant); cfg != nil && in.Internal != "" &&
		pastEligibilityGate(cfg, in.Internal) && !s.hasEligibleDecision(r, tenant, id) {
		warnings = append(warnings,
			"No ELIGIBLE decision on record for this case — run eligibility review before proceeding further (warn-only gate; this transition was allowed)")
		s.logActivity(r.Context(), tenant, id, "ELIGIBILITY_GATE_WARNING",
			fmt.Sprintf("Status moved to %q with no ELIGIBLE decision on record (warn-only)", in.Internal))
	}
	if len(warnings) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "warnings": warnings})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// checkEligibility (G2): computes the eligibility result from program rules —
// threshold matrix, 12-month filing window, explicit ineligibility flags —
// and stores the review with its evidence.
func (s *server) checkEligibility(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: the eligibility review is done by the Reviewer"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	if !s.stepGuard(w, r, tenant, id, "eligibility") {
		return
	}
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		http.Error(w, `{"error":"no program rules for tenant"}`, http.StatusBadRequest)
		return
	}
	// eligibility_reviews/cases carry no FK back to the case row, so without
	// this check a nonexistent case id would still insert a review and
	// return 200 -- the status-update side effects below would then also
	// silently match zero rows.
	var exists bool
	_ = s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM tenant_%s.cases WHERE id=$1)`, sanitizeTenant(tenant)), id).Scan(&exists)
	if !exists {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var in eligibilityInput
	var override struct {
		Override bool   `json:"override"`
		Reason   string `json:"override_reason"`
		Notes    string `json:"notes"` // the Reviewer's working notes, saved with the review (eligibility_notes.go)
	}
	// Decode twice: the decision input, then the override envelope (separate
	// type so eligibilityInput stays exactly the rule inputs).
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(raw, &in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	_ = json.Unmarshal(raw, &override)
	notes, notesOK := cleanEligibilityNote(override.Notes)
	if !notesOK {
		http.Error(w, `{"error":"notes are limited to 4000 characters"}`, http.StatusBadRequest)
		return
	}
	// A human-submitted date that does not parse must be rejected, not
	// ignored: evalEligibility skips a rule whose date is unusable, which
	// would turn a typo into a silent pass on the filing window.
	if msg := invalidEligibilityInput(in); msg != "" {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), http.StatusBadRequest)
		return
	}

	// A provider type the program does not list is a data-entry error, not
	// a finding. Recording it would close the dispute as INELIGIBLE on a
	// typo, so it is refused with the list of types that exist.
	if types := eligibilityProviderTypes(cfg); len(types) > 0 && !contains(types, in.ProviderType) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":          fmt.Sprintf("unknown provider_type %q: must be one of %s", in.ProviderType, strings.Join(types, ", ")),
			"provider_types": types,
		})
		return
	}

	// The filing window is measured on the day the dispute was filed (the case's own record, never the request's).
	in.FiledOn = ""
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce(opened_at::date::text,'') FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), id).Scan(&in.FiledOn)
	if in.FinalDeterminationAt != "" {
		in.DeterminationFrom = "entered by the Reviewer"
	}
	// Decision + persistence both live in triage.go, shared with the
	// auto-adjudication endpoint — one implementation, so the auto path
	// can never drift from what a human reviewer would compute.
	result, reason, evidence := evalEligibility(cfg, in)
	opts := eligibilityPersistOpts{ruleVersion: eligibilityRuleVersion(cfg)}
	bl := eligibilityBorderline(cfg, in, evidence)
	// Documents that disagree with what the case carries make any result
	// one a second person should look at.
	if conflicts := s.eligibilityConflicts(r, tenant, id); len(conflicts) > 0 {
		evidence["document_conflicts"] = conflicts
		bl = append(bl, conflicts...)
	}
	if len(bl) > 0 {
		opts.borderline, opts.borderlineReasons = true, bl
		evidence["borderline"] = true
		evidence["borderline_reasons"] = bl
	}
	// The filing window can only be tested against a date. Without one the
	// rule is skipped, so a result other than INELIGIBLE would rest on an
	// unchecked window -- refuse rather than record an ELIGIBLE that was
	// never tested for timeliness.
	if result != "INELIGIBLE" && cfg.Eligibility.FilingWindowMonths > 0 && in.FinalDeterminationAt == "" {
		http.Error(w, fmt.Sprintf(`{"error":"final determination date required: the %d-month filing window cannot be checked without it (the last EOB date, the service date or appeal documents are accepted as proof)"}`,
			cfg.Eligibility.FilingWindowMonths), http.StatusUnprocessableEntity)
		return
	}
	// No extensions: a dispute filed outside the window cannot be overridden
	// to ELIGIBLE. If the date was entered wrongly, correct the date.
	if override.Override && result == "INELIGIBLE" && noOverrideReasons[reason] {
		http.Error(w, `{"error":"this result cannot be overridden: the filing window is not extendable (AHCA no longer grants extensions). If the final determination date is wrong, correct it and recompute."}`, http.StatusUnprocessableEntity)
		return
	}
	// Override: INELIGIBLE → ELIGIBLE only, with a mandatory written reason.
	// The computed result is preserved in evidence so the record shows both
	// what the rules said and what the human decided.
	if override.Override && result == "INELIGIBLE" {
		if !validOverrideReason(override.Reason) {
			http.Error(w, `{"error":"override requires a written reason (min 10 chars) — the legal record must show why the computed result was set aside"}`, http.StatusUnprocessableEntity)
			return
		}
		evidence["computed_result"] = result
		evidence["computed_reason"] = reason
		opts.override, opts.overrideReason = true, strings.TrimSpace(override.Reason)
		result, reason = "ELIGIBLE", "override"
		s.logAudit(r.Context(), tenant, id, "ELIGIBILITY_OVERRIDE", map[string]any{
			"by": p.Subject, "computed_result": evidence["computed_result"],
			"computed_reason": evidence["computed_reason"], "override_reason": opts.overrideReason,
		})
	}
	// displayName(p), not p.Subject -- eligibility_reviews.decided_by is
	// shown directly to staff in the review-history table (unlike
	// logAudit's "by" above, which is an intentionally stable-identity
	// audit key).
	reviewID := s.persistEligibilityOutcome(r, tenant, id, displayName(p), result, reason, evidence, opts)
	if notes != "" && reviewID != "" {
		_, _ = s.insertEligibilityNote(r.Context(), tenant, id, reviewID, notes, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"review_id": reviewID, "result": result, "reason": reason, "evidence": evidence,
		"rule_version": opts.ruleVersion, "borderline": opts.borderline, "override": opts.override,
	})
}

// eligibilityHistory lists past reviews for a case — the UI shows how the
// current eligibility state was reached instead of leaving users guessing.
func (s *server) eligibilityHistory(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "caseId")
	rows, err := s.queryRows(r, `
		SELECT id, result, COALESCE(reason,'') AS reason, evidence, decided_by, created_at,
		       COALESCE(rule_version,'') AS rule_version, COALESCE(borderline,false) AS borderline,
		       COALESCE(override,false) AS override, COALESCE(override_reason,'') AS override_reason
		FROM public.eligibility_reviews WHERE tenant=$1 AND case_id=$2
		ORDER BY created_at DESC LIMIT 10`, tenant, id)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	notes := s.eligibilityNotesByReview(r, tenant, id)
	for _, rv := range rows {
		rid, _ := rv["id"].(string)
		if n := notes[rid]; n != nil {
			rv["notes"] = n
		} else {
			rv["notes"] = []map[string]any{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": rows})
}

// nextCaseNumber generates per-program case numbers (G11): "FL26-042" etc.
func (s *server) nextCaseNumber(r *http.Request, tenant string, cfg *ProgramConfig) string {
	// A counter that only moves forward. The number used to be "how many
	// cases exist, plus one", so deleting a case handed its number to the
	// next one -- and with it the old case's workflow id, audit trail and
	// invoice number. The counter starts above every number already issued.
	yy := time.Now().Format("06")
	var n int64
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		INSERT INTO public.case_sequences (tenant, yy, last)
		SELECT $1::text, $2::text, greatest(count(*), coalesce(max((regexp_match(case_number, '(\d{1,9})$'))[1]::bigint), 0)) + 1
		FROM tenant_%s.cases
		ON CONFLICT (tenant, yy) DO UPDATE SET last = public.case_sequences.last + 1
		RETURNING last`, sanitizeTenant(tenant)), tenant, yy).Scan(&n)
	if err != nil {
		// No counter table yet (older database): fall back to the count.
		_ = s.db.QueryRow(r.Context(),
			fmt.Sprintf(`SELECT count(*) + 1 FROM tenant_%s.cases`, sanitizeTenant(tenant))).Scan(&n)
	}
	seq := fmt.Sprintf("%0*d", cfg.CaseNumber.SeqPad, n)
	out := strings.ReplaceAll(cfg.CaseNumber.Pattern, "{yy}", yy)
	out = strings.ReplaceAll(out, "{seq}", seq)
	return out
}

// escalationTrigger (G6): auto-route large/fraud-suspect cases to the PM.
func (s *server) escalationTrigger(r *http.Request, tenant, caseID string, disputedCents int64) {
	cfg := s.loadProgram(r, tenant)
	if cfg == nil || cfg.Escalation.AmountTriggerCents <= 0 || disputedCents < cfg.Escalation.AmountTriggerCents {
		return
	}
	detail := fmt.Sprintf("Disputed amount $%d.%02d meets the program escalation trigger — routed to %s for independent medical review (F&WA screening)",
		disputedCents/100, disputedCents%100, cfg.Escalation.RouteRole)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.escalations (tenant, case_id, clock, level, escalated_to, detail)
		VALUES ($1,$2,'PROGRAM_TRIGGER',2,$3,$4)`, tenant, caseID, cfg.Escalation.RouteRole, detail)
	s.logActivity(r.Context(), tenant, caseID, "ESCALATION_AUTO", detail)
	// The same review the SOP prescribes for suspected fraud, waste or
	// abuse: the PM assigns an independent reviewer (sopdeadlines.go) -- unless
	// the tenant turned that review off; the escalation above stands either way.
	if !s.featureEnabled(r, tenant, "fwa_review") {
		s.notify(r, tenant, "*", "ESCALATION", detail, "#/cases/"+caseID)
		return
	}
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET details = jsonb_build_object('fwa', jsonb_build_object(
		  'status','REPORTED','reason',$2::text,'reported_by','system','reported_at',$3::text)) || details
		WHERE id::text=$1 AND NOT (details ? 'fwa')`, sanitizeTenant(tenant)), caseID, detail, time.Now().UTC().Format(time.RFC3339))
	s.notify(r, tenant, "*", "ESCALATION", detail, "#/cases/"+caseID)
}

// canonStatus finds v in the program's status list ignoring case and returns
// the list's own spelling.
func canonStatus(list []string, v string) (string, bool) {
	for _, x := range list {
		if strings.EqualFold(x, strings.TrimSpace(v)) {
			return x, true
		}
	}
	return v, false
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func minI(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// defaultBrand is the platform's name where a tenant has not set its own.
const defaultBrand = "Capitol Bridge"

// tenantBrand: the name the portal shows for a tenant (tenant_labels.brand_name).
func (s *server) tenantBrand(ctx context.Context, tenant string) string {
	var brand string
	_ = s.db.QueryRow(ctx, `SELECT coalesce(brand_name,'') FROM public.tenant_labels WHERE tenant=$1`, tenant).Scan(&brand)
	if brand == "" {
		return defaultBrand
	}
	return brand
}

// setTenantBrand: PUT /v1/tenants/{tenant}/program/brand {"brand_name":"Capitol Bridge"}
// -- the program's admins (same guard as the rules). Empty resets to the default.
func (s *server) setTenantBrand(w http.ResponseWriter, r *http.Request) {
	p, ok := s.rulesAdminGuard(w, r)
	if !ok {
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Brand string `json:"brand_name"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in) != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	brand := strings.TrimSpace(in.Brand)
	if len([]rune(brand)) > 60 {
		http.Error(w, `{"error":"keep the brand name to 60 characters"}`, http.StatusBadRequest)
		return
	}
	// The label row may not exist yet: keep the label the portal shows today.
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.tenant_labels (tenant, label, brand_name, updated_by, updated_at)
		VALUES ($1, upper($1) || ' IDRE', NULLIF($2,''), $3, now())
		ON CONFLICT (tenant) DO UPDATE SET brand_name=NULLIF($2,''), updated_by=$3, updated_at=now()`,
		tenant, brand, displayName(p)); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	shown := s.tenantBrand(r.Context(), tenant)
	s.logActivity(r.Context(), tenant, "", "TENANT_BRAND_UPDATED", fmt.Sprintf("Portal brand set to %q by %s", shown, displayName(p)))
	s.logAudit(r.Context(), tenant, "", "TENANT_BRAND_UPDATED", map[string]any{"by": p.Subject, "brand_name": shown})
	writeJSON(w, http.StatusOK, map[string]string{"brand_name": shown})
}

// publicBrand: GET /api/public/brand?tenant=fl -- for the public pages
// (home, file a dispute, registration), before anyone signs in.
func (s *server) publicBrand(w http.ResponseWriter, r *http.Request) {
	tenant := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tenant")))
	brand := defaultBrand
	if tenantCodeRe.MatchString(tenant) {
		brand = s.tenantBrand(r.Context(), tenant)
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]string{"brand_name": brand})
}

// tenantZone is the tenant's configured program time zone (Eastern when it has none).
func (s *server) tenantZone(r *http.Request, tenant string) *time.Location {
	return s.loadProgram(r, tenant).location()
}

// programToday is today's date (YYYY-MM-DD) on the program's clock.
func (s *server) programToday(ctx context.Context, tenant string) string {
	var tz string
	_ = s.db.QueryRow(ctx, `SELECT coalesce(config->>'time_zone','') FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&tz)
	return time.Now().In((&ProgramConfig{TimeZone: tz}).location()).Format("2006-01-02")
}

// stepDayOf is the day a recorded step actually happened: the "date" its dialog gave (YYYY-MM-DD, not after
// today), or today when none was given. The second result says what is wrong with a given date.
func stepDayOf(data map[string]any, today string) (string, string) {
	v, _ := data["date"].(string)
	v = strings.TrimSpace(v)
	if v == "" {
		return today, ""
	}
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return "", "the date must be a date (YYYY-MM-DD)"
	}
	if v > today {
		return "", "the date can't be later than today"
	}
	return v, ""
}

// markProgramDate records a program date (YYYY-MM-DD, today on the program's clock) unless the case already has it:
// the first time a step happened is when its clock was met.
func (s *server) markProgramDate(ctx context.Context, tenant, caseID, key string) {
	s.markProgramDateOn(ctx, tenant, caseID, key, s.programToday(ctx, tenant))
}

// markProgramDateOn is markProgramDate for a given day.
func (s *server) markProgramDateOn(ctx context.Context, tenant, caseID, key, day string) {
	_, _ = s.db.Exec(ctx, fmt.Sprintf(`
		UPDATE tenant_%s.cases SET program_dates = coalesce(program_dates,'{}'::jsonb) || jsonb_build_object($2::text, $3::text)
		WHERE id::text=$1 AND NOT (coalesce(program_dates,'{}'::jsonb) ? $2)`, sanitizeTenant(tenant)),
		caseID, key, day)
}

// OutcomeGroup: one heading of the program's outcome structure and the outcomes under it.
type OutcomeGroup struct {
	Group    string   `json:"group"`
	Outcomes []string `json:"outcomes"`
}

// outcomeGroupOf: the group an outcome belongs to ("" when none lists it).
func (c *ProgramConfig) outcomeGroupOf(outcome string) string {
	if c == nil {
		return ""
	}
	for _, g := range c.OutcomeGroups {
		for _, o := range g.Outcomes {
			if strings.EqualFold(o, outcome) {
				return g.Group
			}
		}
	}
	return ""
}

// closedWithoutReviewStatuses: the internal statuses a case closes with before any review (no invoice follows).
var closedWithoutReviewStatuses = map[string]bool{"Provider Closure Letter Issued": true, "Dismissed": true, "Plan Opt-Out": true,
	"Provider - Withdrawal": true, "Closed-Refunded": true, "Ineligible": true, "Withdrawn": true}

// setProgramDateToday records a program date as today, replacing any earlier one (a step that can recur, like an
// RFI response, is met by its latest occurrence).
func (s *server) setProgramDateToday(ctx context.Context, tenant, caseID, key string) {
	s.setProgramDateOn(ctx, tenant, caseID, key, s.programToday(ctx, tenant))
}

// setProgramDateOn is setProgramDateToday for a given day.
func (s *server) setProgramDateOn(ctx context.Context, tenant, caseID, key, day string) {
	_, _ = s.db.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET program_dates = coalesce(program_dates,'{}'::jsonb) || jsonb_build_object($2::text, $3::text) WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID, key, day)
}

// setCaseDetail sets one field of a case's details.
func (s *server) setCaseDetail(ctx context.Context, tenant, caseID, key, value string) {
	_, _ = s.db.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET details = coalesce(details,'{}'::jsonb) || jsonb_build_object($2::text, $3::text) WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID, key, value)
}

// addCaseNote keeps a note on the case (the internal stream) and on its activity timeline, as addNote does.
func (s *server) addCaseNote(ctx context.Context, tenant, caseID, author, body string) {
	if _, err := s.db.Exec(ctx, `INSERT INTO public.notes (tenant, record_type, record_id, stream, body, author) VALUES ($1,'CASE',$2,'internal',$3,$4)`,
		tenant, caseID, body, author); err != nil {
		return
	}
	_, _ = s.db.Exec(ctx, `INSERT INTO public.case_activities (tenant, case_id, type, body) VALUES ($1,$2,'NOTE',$3)`, tenant, caseID, body)
}
