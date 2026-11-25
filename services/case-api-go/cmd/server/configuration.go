// configuration.go — the Configuration page: the program's settings in one place, edited by an admin instead of
// written into code or a migration.
//
//	GET  /program/configuration            every section, plus the dated pay rates and the roles they apply to
//	PUT  /program/configuration/{section}  fees | eligibility | clocks | billing
//
// Fees and billing were migration-only (the guard trigger on program_rules). An admin's save here is that same
// controlled change: one transaction that opens the guard for itself (SET LOCAL), records the old and new config in
// program_config_history (the trigger), and writes an audit entry naming the admin. The initial fee is a dated
// schedule (fees.initial_fee_schedule): each filing is charged the fee in force on the day it is filed, so next
// year's fee can be entered ahead of time and never re-prices a dispute already filed.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	chi "github.com/go-chi/chi/v5"
)

var configEditRoles = []string{"TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}
var configViewRoles = []string{"TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "PM", "FINANCE"}

// rateRoles: the staff roles whose time is billed, in the order the page lists them.
var rateRoles = []string{"CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE", "ATTORNEY", "ARBITRATOR", "FINANCE"}

// GET /program/configuration
func (s *server) getConfiguration(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, configViewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		http.Error(w, `{"error":"this tenant has no program settings"}`, http.StatusNotFound)
		return
	}
	loc := cfg.location()
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")
	sched := append([]FeeStep(nil), cfg.Fees.InitialFeeSchedule...)
	if len(sched) == 0 { // the single stored fee, shown as the schedule's first entry
		sched = []FeeStep{{EffectiveFrom: "2000-01-01", Cents: cfg.Fees.InitialFeeCents}}
	}
	sort.Slice(sched, func(i, j int) bool {
		a, _ := stepStart(sched[i].EffectiveFrom, loc)
		b, _ := stepStart(sched[j].EffectiveFrom, loc)
		return a.Before(b)
	})
	rates, _ := s.queryRows(r, `
		SELECT role, effective_from::text AS effective_from, rate_cents_per_hour, coalesce(set_by,'') AS set_by, set_at
		FROM public.time_rate_schedule WHERE tenant=$1 ORDER BY role, effective_from`, tenant)
	book := s.timeRateBook(r, tenant)
	current := map[string]any{}
	for _, role := range rateRoles {
		if c, ok := book.at(role, today); ok {
			current[role] = c
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"today": today, "now": now.Format("2006-01-02T15:04"), "time_zone": loc.String(), "zone_abbrev": now.Format("MST"),
		"can_edit":       hasAnyRole(p, configEditRoles...),
		"can_edit_rates": hasAnyRole(p, timeRateManageRoles...) || hasAnyRole(p, configEditRoles...),
		"fees": map[string]any{
			"initial_fee_schedule": sched, "initial_fee_today_cents": cfg.Fees.InitialFeeCents,
			"refund_window_days": cfg.Fees.RefundWindowDays, "invoice_due_days": cfg.Fees.InvoiceDueDays,
			"reminder_lead_days": cfg.Fees.ReminderLeadDays, "late_fee": cfg.Fees.LateFee,
			"card_fee_today_cents": cardFeeToday(cfg, now),
		},
		"eligibility": map[string]any{
			"thresholds": cfg.Eligibility.Thresholds, "filing_window_months": cfg.Eligibility.FilingWindowMonths,
			"services_after": cfg.Eligibility.ServicesAfter,
		},
		"outcomes":       outcomeList(cfg),
		"outcome_groups": cfg.OutcomeGroups,
		"clocks":         cfg.Clocks,
		"billing":        billingDefaults(cfg),
		"time":           map[string]any{"quarter_hours": cfg.Time.QuarterHours},
		"rates":          map[string]any{"roles": rateRoles, "schedule": rates, "current": current},
	})
}

// PUT /program/configuration/{section}
func (s *server) putConfiguration(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, configEditRoles...) {
		http.Error(w, `{"error":"only a tenant admin can change the configuration"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	section := chi.URLParam(r, "section")
	body := json.RawMessage{}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&body); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	var raw []byte
	if err := s.db.QueryRow(r.Context(), `SELECT config FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw); err != nil {
		http.Error(w, `{"error":"this tenant has no program settings"}`, http.StatusNotFound)
		return
	}
	var conf map[string]any
	if json.Unmarshal(raw, &conf) != nil {
		http.Error(w, `{"error":"the stored configuration could not be read"}`, http.StatusInternalServerError)
		return
	}
	sub := func(k string) map[string]any {
		m, _ := conf[k].(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		return m
	}
	cur := &ProgramConfig{}
	_ = json.Unmarshal(raw, cur)
	loc := cur.location()
	now := time.Now()
	var msg string
	switch section {
	case "general":
		var in struct {
			TimeZone string `json:"time_zone"`
		}
		_ = json.Unmarshal(body, &in)
		z, err := time.LoadLocation(strings.TrimSpace(in.TimeZone))
		if err != nil || !strings.Contains(in.TimeZone, "/") {
			msg = "choose a time zone by its place name, e.g. America/New_York"
			break
		}
		conf["time_zone"] = z.String()
		// The fee in force may change with the clock it is read on.
		if fees, ok := conf["fees"].(map[string]any); ok {
			conf["fees"] = mergedFees(fees, json.RawMessage(`{}`), now, z)
		}
	case "fees":
		if msg = checkFees(body, now, loc); msg == "" {
			conf["fees"] = mergedFees(sub("fees"), body, now, loc)
		}
	case "eligibility":
		var m map[string]any
		m, msg = applyEligibility(sub("eligibility"), body)
		conf["eligibility"] = m
	case "clocks":
		var cl []any
		cl, msg = applyClocks(conf["clocks"], body)
		conf["clocks"] = cl
	case "billing":
		var m map[string]any
		m, msg = applyBilling(sub("billing"), body)
		conf["billing"] = m
	case "outcomes":
		var list []string
		var defs map[string]string
		if list, defs, msg = checkOutcomes(body); msg == "" {
			fs := sub("field_schema")
			fs["case_outcome"] = list
			conf["field_schema"] = fs
			conf["outcome_definitions"] = defs
		}
	default:
		http.Error(w, `{"error":"section must be general, fees, eligibility, clocks, billing or outcomes"}`, http.StatusBadRequest)
		return
	}
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	out, _ := json.Marshal(conf)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())
	// The guard trigger lets fee and billing changes through only inside a transaction that says so; this is one.
	if _, err := tx.Exec(r.Context(), `SET LOCAL app.config_migration = 'on'`); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE public.program_rules SET config=$2::jsonb, updated_at=now() WHERE tenant=$1`, tenant, string(out)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the configuration could not be saved: " + err.Error()})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	var changed any
	_ = json.Unmarshal(body, &changed)
	s.logActivity(r.Context(), tenant, "", "CONFIG_UPDATED", fmt.Sprintf("Configuration (%s) changed by %s", section, displayName(p)))
	s.logAudit(r.Context(), tenant, "", "CONFIG_UPDATED", map[string]any{"section": section, "change": changed, "actor": p.Subject, "by": displayName(p)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "section": section})
}

// cardFeeToday: the card transaction fee in force now (0 when none).
func cardFeeToday(cfg *ProgramConfig, now time.Time) int64 {
	st, _ := stepAt(cfg.Fees.InitialFeeSchedule, now, cfg.location())
	return st.CardFeeCents
}

func isDate(v string) bool { _, err := time.Parse("2006-01-02", v); return err == nil }

// checkFees checks a fees change; "" when it can be saved.
func checkFees(body json.RawMessage, now time.Time, loc *time.Location) string {
	var in struct {
		Schedule         []FeeStep `json:"initial_fee_schedule"`
		RefundWindowDays *int      `json:"refund_window_days"`
		InvoiceDueDays   *int      `json:"invoice_due_days"`
		ReminderLeadDays *int      `json:"reminder_lead_days"`
		LateFee          *LateFee  `json:"late_fee"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return "the fees could not be read"
	}
	if l := in.LateFee; l != nil && l.Cents != 0 {
		switch {
		case l.Cents < 0 || l.Cents > 1_000_000:
			return "a late fee must be between $0 and $10,000"
		case l.AfterDays < 1 || l.AfterDays > 365:
			return "the late fee starts after 1 to 365 days from the Final Order"
		case l.EveryDays < 0 || l.EveryDays > 365:
			return "the late fee repeats every 0 (never) to 365 days"
		}
	}
	if in.Schedule != nil {
		if len(in.Schedule) == 0 {
			return "keep at least one fee in the schedule"
		}
		seen := map[int64]bool{}
		for _, st := range in.Schedule {
			start, ok := stepStart(st.EffectiveFrom, loc)
			switch {
			case !ok:
				return "each fee needs the date (and, if you like, the time) it takes effect"
			case seen[start.Unix()]:
				return "two fees start at the same moment (" + st.EffectiveFrom + ") — keep one"
			case st.Cents <= 0 || st.Cents > 10_000_000:
				return "a fee must be more than $0 and at most $100,000"
			case st.CardFeeCents < 0 || st.CardFeeCents > st.Cents:
				return "a card transaction fee can't be negative or more than the fee itself"
			}
			seen[start.Unix()] = true
		}
		if _, ok := feeAt(in.Schedule, now, loc); !ok {
			return "the schedule must have a fee in force now (" + now.In(loc).Format("Jan 2, 2006 3:04 PM MST") + ") — every entry starts later"
		}
	}
	for name, v := range map[string]*int{"refund window": in.RefundWindowDays, "invoice due": in.InvoiceDueDays, "reminder lead": in.ReminderLeadDays} {
		if v != nil && (*v < 0 || *v > 365) {
			return "the " + name + " days must be between 0 and 365"
		}
	}
	return ""
}

// mergedFees: the stored fees block with the change applied. initial_fee_cents is kept equal to today's fee for
// anything that reads it straight from the database.
func mergedFees(fees map[string]any, body json.RawMessage, now time.Time, loc *time.Location) map[string]any {
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	for _, k := range []string{"initial_fee_schedule", "refund_window_days", "invoice_due_days", "reminder_lead_days", "late_fee"} {
		if v, ok := in[k]; ok {
			fees[k] = v
		}
	}
	var sched []FeeStep
	if b, err := json.Marshal(fees["initial_fee_schedule"]); err == nil {
		_ = json.Unmarshal(b, &sched)
	}
	if c, ok := feeAt(sched, now, loc); ok {
		fees["initial_fee_cents"] = c
	}
	return fees
}

func applyEligibility(el map[string]any, body json.RawMessage) (map[string]any, string) {
	var in struct {
		Thresholds         []ThresholdRule `json:"thresholds"`
		FilingWindowMonths *int            `json:"filing_window_months"`
		ServicesAfter      *string         `json:"services_after"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return el, "the eligibility settings could not be read"
	}
	if in.Thresholds != nil {
		seen := map[string]bool{}
		for _, t := range in.Thresholds {
			k := t.ProviderType + "|" + fmt.Sprint(t.Contracted != nil && *t.Contracted, t.Contracted == nil)
			switch {
			case strings.TrimSpace(t.ProviderType) == "":
				return el, "each threshold needs its provider type"
			case t.MinCents < 0:
				return el, "a minimum amount can't be negative"
			case seen[k]:
				return el, "two thresholds for the same provider type and network status — keep one"
			}
			seen[k] = true
		}
		el["thresholds"] = in.Thresholds
	}
	if in.FilingWindowMonths != nil {
		if *in.FilingWindowMonths < 0 || *in.FilingWindowMonths > 120 {
			return el, "the filing window must be between 0 and 120 months"
		}
		el["filing_window_months"] = *in.FilingWindowMonths
	}
	if in.ServicesAfter != nil {
		if *in.ServicesAfter != "" && !isDate(*in.ServicesAfter) {
			return el, "the program start date must be a date (YYYY-MM-DD)"
		}
		el["services_after"] = *in.ServicesAfter
	}
	return el, ""
}

// applyClocks changes the days of clocks the program already has (by name); clocks are never added or removed here,
// since the workflow and the reports know them by name.
func applyClocks(stored any, body json.RawMessage) ([]any, string) {
	list, _ := stored.([]any)
	var in []struct {
		Name    string  `json:"name"`
		Days    *int    `json:"days"`
		MaxDays *int    `json:"max_days"`
		DayType *string `json:"day_type"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return list, "the deadlines could not be read"
	}
	for _, c := range in {
		var target map[string]any
		for _, x := range list {
			if m, ok := x.(map[string]any); ok && m["name"] == c.Name {
				target = m
			}
		}
		if target == nil {
			return list, "there is no deadline named " + c.Name
		}
		if c.Days != nil {
			if *c.Days <= 0 || *c.Days > 730 {
				return list, "a deadline must be between 1 and 730 days"
			}
			target["days"] = *c.Days
		}
		if c.MaxDays != nil {
			days := toInt64(target["days"])
			if *c.MaxDays != 0 && int64(*c.MaxDays) < days {
				return list, "a hard limit can't be shorter than the deadline (" + c.Name + ")"
			}
			target["max_days"] = *c.MaxDays
		}
		if c.DayType != nil {
			if *c.DayType != "calendar" && *c.DayType != "business" {
				return list, "day type must be calendar or business"
			}
			target["day_type"] = *c.DayType
		}
	}
	return list, ""
}

func applyBilling(b map[string]any, body json.RawMessage) (map[string]any, string) {
	var in BillingConfig
	if err := json.Unmarshal(body, &in); err != nil {
		return b, "the billing settings could not be read"
	}
	switch {
	case in.DueDays < 0 || in.DueDays > 365:
		return b, "invoice due days must be between 0 and 365"
	case in.TaxPct < 0 || in.TaxPct > 50:
		return b, "tax must be between 0% and 50%"
	case in.MinInvoiceCents < 0:
		return b, "the minimum invoice can't be negative"
	case in.BillToEmail != "" && !validMailbox(in.BillToEmail):
		return b, "the bill-to email is not a valid address"
	}
	// Only the billing settings themselves are copied, never other keys sent along.
	var given map[string]any
	_ = json.Unmarshal(body, &given)
	for _, k := range []string{"invoice_prefix", "due_days", "tax_pct", "currency", "consolidate", "block_on_unrated",
		"min_invoice_cents", "bill_to_name", "bill_to_email", "memo"} {
		if v, ok := given[k]; ok {
			b[k] = v
		}
	}
	return b, ""
}

// outcomeList: the program's case outcomes in order, each with its definition.
func outcomeList(cfg *ProgramConfig) []map[string]string {
	out := []map[string]string{}
	for _, name := range cfg.FieldSchema["case_outcome"] {
		out = append(out, map[string]string{"name": name, "definition": cfg.OutcomeDefinitions[name]})
	}
	return out
}

// checkOutcomes reads the outcomes list ([{name, definition}], in order) and says what is wrong with it.
func checkOutcomes(body json.RawMessage) ([]string, map[string]string, string) {
	var in []struct {
		Name       string `json:"name"`
		Definition string `json:"definition"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, nil, "the outcomes could not be read"
	}
	if len(in) == 0 {
		return nil, nil, "keep at least one outcome"
	}
	if len(in) > 50 {
		return nil, nil, "at most 50 outcomes"
	}
	list, defs, seen := []string{}, map[string]string{}, map[string]bool{}
	for _, o := range in {
		name, def := strings.TrimSpace(o.Name), strings.TrimSpace(o.Definition)
		switch {
		case name == "":
			return nil, nil, "each outcome needs a name"
		case len(name) > 120 || len(def) > 1000:
			return nil, nil, "an outcome's name or definition is too long"
		case seen[strings.ToLower(name)]:
			return nil, nil, "two outcomes are called " + name + " — keep one"
		}
		seen[strings.ToLower(name)] = true
		list = append(list, name)
		defs[name] = def
	}
	return list, defs, ""
}
