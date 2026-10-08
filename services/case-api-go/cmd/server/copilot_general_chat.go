package main

// General (non-case-scoped) copilot chat — the floating Assistant widget's
// fallback when no case is open. copilot_chat.go's thread is grounded on
// ONE case's full record and is part of that case's audit trail; this one
// is grounded on the signed-in worker's OWN open-case/task queue (cheap,
// bounded queries — the same shape the widget's own fast-path intents
// already run) and is part of the worker's personal assistant history, not
// any case record. Same hard rules as the case thread: grounded-or-silent,
// one bounded call, advisory only.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

type generalCopilotCase struct {
	CaseNumber       string `json:"case_number"`
	Status           string `json:"status"`
	ServiceLine      string `json:"service_line,omitempty"`
	SLADaysRemaining int    `json:"sla_days_remaining"`
}

type generalCopilotTask struct {
	Subject string `json:"subject"`
	DueDate string `json:"due_date,omitempty"`
	Status  string `json:"status"`
}

type generalCopilotFacts struct {
	UserDisplayName string               `json:"user_display_name"`
	OpenCases       []generalCopilotCase `json:"my_open_cases"`
	OpenCasesTotal  int                  `json:"my_open_cases_total"`
	OpenTasks       []generalCopilotTask `json:"my_open_tasks"`
	OpenTasksTotal  int                  `json:"my_open_tasks_total"`
}

// gatherGeneralCopilotFacts mirrors gatherCopilotFacts's "platform-verified,
// nothing model-generated" discipline, scoped to the signed-in worker
// instead of one case. Same data the widget's own "my cases"/"my tasks"
// fast-path intents fetch directly — kept here as one more bounded query
// pair, not a reason to route those intents through the LLM instead.
func (s *server) gatherGeneralCopilotFacts(r *http.Request, tenant string, p principal) (generalCopilotFacts, error) {
	f := generalCopilotFacts{UserDisplayName: displayName(p)}
	tbl := sanitizeTenant(tenant)

	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT count(*) FROM tenant_%s.cases WHERE assigned_to=$1`, tbl), p.Subject).Scan(&f.OpenCasesTotal)
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT case_number, status, coalesce(service_line,''), %s AS sla_days_remaining
		FROM tenant_%s.cases c WHERE assigned_to=$1
		ORDER BY sla_days_remaining ASC LIMIT 15`, slaDaysSQL, tbl), p.Subject)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var c generalCopilotCase
			if rows.Scan(&c.CaseNumber, &c.Status, &c.ServiceLine, &c.SLADaysRemaining) == nil {
				f.OpenCases = append(f.OpenCases, c)
			}
		}
	}

	_ = s.db.QueryRow(r.Context(),
		`SELECT count(*) FROM public.tasks WHERE tenant=$1 AND assignee=$2 AND status<>'DONE'`,
		tenant, p.Subject).Scan(&f.OpenTasksTotal)
	trows, err := s.db.Query(r.Context(), `
		SELECT subject, coalesce(to_char(due_date,'YYYY-MM-DD'),''), status
		FROM public.tasks WHERE tenant=$1 AND assignee=$2 AND status<>'DONE'
		ORDER BY due_date NULLS LAST LIMIT 15`, tenant, p.Subject)
	if err == nil {
		defer trows.Close()
		for trows.Next() {
			var t generalCopilotTask
			if trows.Scan(&t.Subject, &t.DueDate, &t.Status) == nil {
				f.OpenTasks = append(f.OpenTasks, t)
			}
		}
	}
	return f, nil
}

func copilotGeneralChatPrompt(f generalCopilotFacts) string {
	return `You are the assistant inside a No Surprises Act / state dispute-resolution case
platform, chatting with ` + f.UserDisplayName + ` about THEIR OWN work queue — not any
single case in deep detail.

HARD RULES:
- Use ONLY the JSON data below. Never invent case numbers, amounts, dates, or details not present here.
- For anything about ONE case beyond what's listed here (eligibility posture, documents, deadlines in depth), tell them to open that case and ask its own case Assistant, which is grounded on that case's full record.
- A casual greeting gets a casual, friendly reply — this is a chat surface, not a report. Respond naturally.
- Be concise (under 100 words unless more detail is asked for).
- You are ADVISORY and cannot take any action or change any record.

THEIR DATA (JSON, platform-verified, refreshed this turn):
` + func() string { b, _ := json.Marshal(f); return string(b) }()
}

// copilotGeneralChatHistory handles GET /copilot/chat — the floating
// widget's thread when no case is open, newest-last for rendering.
func (s *server) copilotGeneralChatHistory(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, copilotRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT role, body, coalesce(model,''), created_at
		FROM (SELECT role, body, model, created_at FROM public.copilot_general_threads
		      WHERE tenant=$1 AND user_sub=$2 ORDER BY created_at DESC LIMIT 50) t
		ORDER BY created_at`, tenant, p.Subject)
	if err != nil {
		slog.Error("copilotGeneralChatHistory query failed", "tenant", tenant, "err", err)
		http.Error(w, `{"error":"db (copilot_general_threads migrated?)"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []copilotChatTurn{}
	for rows.Next() {
		var t copilotChatTurn
		if rows.Scan(&t.Role, &t.Body, &t.Model, &t.CreatedAt) == nil {
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": out})
}

// copilotGeneralChat handles POST /copilot/chat {"message": "..."} — the
// floating widget's fallback once a message doesn't match one of its fast,
// deterministic intents (my cases / my tasks / what's left). Same one-
// bounded-call, grounded-or-silent discipline as the case thread, just
// grounded on the worker's own queue instead of one case's record.
func (s *server) copilotGeneralChat(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, copilotRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	if s.cfg.CopilotEndpoint == "" {
		http.Error(w, `{"error":"copilot not configured (COPILOT_ENDPOINT empty)"}`, http.StatusServiceUnavailable)
		return
	}
	var in struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Message) == "" {
		http.Error(w, `{"error":"message required"}`, http.StatusBadRequest)
		return
	}
	in.Message = truncate(strings.TrimSpace(in.Message), 2000)

	facts, _ := s.gatherGeneralCopilotFacts(r, tenant, p)

	rows, err := s.db.Query(r.Context(), `
		SELECT role, body FROM (SELECT role, body FROM public.copilot_general_threads
		  WHERE tenant=$1 AND user_sub=$2 ORDER BY created_at DESC LIMIT $3) t
		ORDER BY created_at`, tenant, p.Subject, copilotChatMaxHistory)
	if err != nil {
		slog.Error("copilotGeneralChat history query failed", "tenant", tenant, "err", err)
		http.Error(w, `{"error":"db (copilot_general_threads migrated?)"}`, http.StatusInternalServerError)
		return
	}
	history := []map[string]string{}
	for rows.Next() {
		var role, body string
		if rows.Scan(&role, &body) == nil {
			history = append(history, map[string]string{"role": role, "content": body})
		}
	}
	rows.Close()

	reply, err := ollamaChatWithHistory(r.Context(), s.cfg.CopilotEndpoint, s.cfg.CopilotModel,
		copilotGeneralChatPrompt(facts), history, in.Message, 500)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "copilot model unreachable — no reply generated; the facts below are still authoritative",
			"detail": err.Error(), "facts": facts,
		})
		return
	}

	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.copilot_general_threads (tenant, user_sub, role, body) VALUES ($1,$2,'user',$3)`,
		tenant, p.Subject, in.Message)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.copilot_general_threads (tenant, user_sub, role, body, model) VALUES ($1,$2,'assistant',$3,$4)`,
		tenant, p.Subject, reply, s.cfg.CopilotModel)
	s.logAudit(r.Context(), tenant, "", "COPILOT_GENERAL_CHAT", map[string]any{
		"by": p.Subject, "model": s.cfg.CopilotModel,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"reply": reply, "model": s.cfg.CopilotModel, "advisory": true,
	})
}
