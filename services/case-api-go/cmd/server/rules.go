// rules.go — tenant-scoped rule engine.
//
// Business policy lives in DATA (public.program_rules config.rules[]), not in
// code. A rule is:
//
//   {
//     "name": "intake-day13-incomplete",
//     "event": "sweep.intake",              // which hook fires it
//     "enabled": true,
//     "conditions": [                        // ALL must hold (AND)
//       {"field": "days_since_outreach", "op": "gte", "value": 13},
//       {"field": "status", "op": "in", "value": ["INSTRUCTED","DOCS_RECEIVED"]}
//     ],
//     "actions": [
//       {"type": "set_status", "params": {"status": "INELIGIBLE"}},
//       {"type": "notify", "params": {"kind": "SLA_BREACH",
//          "body": "Intake {{id}} incomplete at day 13 — issue ineligibility letter"}}
//     ]
//   }
//
// Events are fired by code at decision points (intake.advance, doc.upload,
// claims.imported, invoice.settled, sweep.intake). The engine evaluates
// conditions against a facts map; callers execute the returned actions through
// the small registry below. Adding a policy = inserting JSON, not a deploy.
//
// Semantics: conditions are conjunctive; an empty conditions list always
// matches. Unknown ops/fields fail CLOSED for block-type rules and are
// skipped otherwise — a misconfigured rule must never silently allow what an
// operator intended to forbid.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type ruleCond struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

type ruleAction struct {
	Type   string         `json:"type"`
	Params map[string]any `json:"params"`
}

type Rule struct {
	Name       string       `json:"name"`
	Event      string       `json:"event"`
	Enabled    *bool        `json:"enabled"`
	Conditions []ruleCond   `json:"conditions"`
	Actions    []ruleAction `json:"actions"`
}

func (r Rule) active() bool { return r.Enabled == nil || *r.Enabled }

// rulesFor loads the tenant's rules for one event, fresh from config every
// call (config edits take effect without a restart).
func (s *server) rulesFor(r *http.Request, tenant, event string) []Rule {
	var raw []byte
	if err := s.db.QueryRow(r.Context(),
		`SELECT config->'rules' FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw); err != nil {
		return nil
	}
	var all []Rule
	if json.Unmarshal(raw, &all) != nil {
		return nil
	}
	var out []Rule
	for _, ru := range all {
		if ru.Event == event && ru.active() {
			out = append(out, ru)
		}
	}
	return out
}

// toFloat coerces JSON numbers / numeric strings for comparisons.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// evalCond: one condition against the facts map. ok=false + known=true means
// the condition failed; known=false means the op/field was unrecognized.
func evalCond(c ruleCond, facts map[string]any) (ok, known bool) {
	fact, present := facts[c.Field]
	switch c.Op {
	case "is_null":
		return !present || fact == nil || fact == "", true
	case "not_null":
		return present && fact != nil && fact != "", true
	}
	if !present {
		return false, true // condition on an absent fact fails (except *_null ops)
	}
	switch c.Op {
	case "eq":
		return fmt.Sprint(fact) == fmt.Sprint(c.Value), true
	case "neq":
		return fmt.Sprint(fact) != fmt.Sprint(c.Value), true
	case "in":
		list, isList := c.Value.([]any)
		if !isList {
			return false, false
		}
		for _, item := range list {
			if fmt.Sprint(fact) == fmt.Sprint(item) {
				return true, true
			}
		}
		return false, true
	case "contains":
		return strings.Contains(fmt.Sprint(fact), fmt.Sprint(c.Value)), true
	case "gt", "gte", "lt", "lte":
		f, okF := toFloat(fact)
		v, okV := toFloat(c.Value)
		if !okF || !okV {
			return false, false
		}
		switch c.Op {
		case "gt":
			return f > v, true
		case "gte":
			return f >= v, true
		case "lt":
			return f < v, true
		default:
			return f <= v, true
		}
	case "days_older_than":
		// fact is an RFC3339 timestamp; passes when it is more than value days old.
		ts, err := time.Parse(time.RFC3339, fmt.Sprint(fact))
		threshold, okT := toFloat(c.Value)
		if err != nil || !okT {
			return false, false
		}
		return time.Since(ts) > time.Duration(threshold*24)*time.Hour, true
	}
	return false, false
}

// evalRule returns (matched, understood): matched = all conditions hold;
// understood = every condition used a known op (else the rule is suspect).
func evalRule(ru Rule, facts map[string]any) (matched, understood bool) {
	for _, c := range ru.Conditions {
		ok, known := evalCond(c, facts)
		if !known {
			return false, false
		}
		if !ok {
			return false, true
		}
	}
	return true, true
}

// fireRules evaluates a tenant's rules for an event and returns the actions
// of every matching rule. A rule with unrecognized conditions is treated as
// NOT matching, and the caller is told via suspectRules so it can fail closed
// on security-sensitive events (doc.upload blocks) while staying open on
// advisory ones.
func fireRules(rules []Rule, facts map[string]any) (actions []ruleAction, suspectRules []string) {
	for _, ru := range rules {
		matched, understood := evalRule(ru, facts)
		if !understood {
			suspectRules = append(suspectRules, ru.Name)
			continue
		}
		if matched {
			actions = append(actions, ru.Actions...)
		}
	}
	return actions, suspectRules
}

// expandTemplate fills {{field}} placeholders from facts (notification text).
func expandTemplate(tpl string, facts map[string]any) string {
	for k, v := range facts {
		tpl = strings.ReplaceAll(tpl, "{{"+k+"}}", fmt.Sprint(v))
	}
	return tpl
}
