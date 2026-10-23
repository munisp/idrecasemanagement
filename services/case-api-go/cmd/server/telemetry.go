package main

// telemetry.go — Phase 1 of the productivity program: the interaction
// baseline. POST /telemetry/events ingests batched portal events (privacy:
// keystroke COUNTS, never content); GET /telemetry/metrics aggregates them
// into per-workflow click/keystroke/session numbers so "we cut initiation
// from 140 keystrokes to 40" is a measured claim, not a vibe.

import (
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var telemClean = regexp.MustCompile(`[^a-z0-9_:/#-]`)

// telemetryIngest: POST /telemetry/events {events:[...]} — best-effort,
// cheap, and never the reason a portal interaction fails.

// telemActions: what the portal reports (telemetry.js). task_start / task_done / task_error carry a task name
// (target) and, for task_done/task_error, how long it took (meta.ms) and how much work (meta.clicks, meta.keys).
var telemActions = map[string]bool{"click": true, "keystroke": true, "submit": true, "navigate": true,
	"prefill_accept": true, "prefill_override": true, "next_action_accept": true, "next_action_override": true,
	"task_start": true, "task_done": true, "task_error": true}

func (s *server) telemetryIngest(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		SessionID string `json:"session_id"`
		Events    []struct {
			Workflow string         `json:"workflow"`
			Action   string         `json:"action"`
			Target   string         `json:"target"`
			Meta     map[string]any `json:"meta"`
		} `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.SessionID == "" {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if len(in.Events) > 200 {
		in.Events = in.Events[:200]
	}
	sess := telemClean.ReplaceAllString(in.SessionID, "")
	if len(sess) > 64 {
		sess = sess[:64]
	}
	// One round trip for the whole upload, and only the actions the metrics read.
	batch := &pgx.Batch{}
	for _, e := range in.Events {
		wf := telemClean.ReplaceAllString(e.Workflow, "")
		act := telemClean.ReplaceAllString(e.Action, "")
		if wf == "" || !telemActions[act] || len(wf) > 40 {
			continue
		}
		tgt := e.Target
		if len(tgt) > 120 {
			tgt = tgt[:120]
		}
		meta, _ := json.Marshal(e.Meta)
		if len(meta) > 1024 {
			meta = []byte("{}")
		}
		batch.Queue(`INSERT INTO public.interaction_events (tenant, actor_sub, session_id, workflow, action, target, meta)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, tenant, p.Subject, sess, wf, act, tgt, meta)
	}
	stored := 0
	if batch.Len() > 0 {
		br := s.db.SendBatch(r.Context(), batch)
		for i := 0; i < batch.Len(); i++ {
			if _, err := br.Exec(); err == nil {
				stored++
			}
		}
		_ = br.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": stored})
}

// telemetryMetrics: GET /telemetry/metrics?days=30 — per-workflow aggregates:
// sessions, clicks, keystrokes, submits, and the learning signals (prefill
// and next-action accept/override rates). Admin/manager roles only.
func (s *server) telemetryMetrics(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN", "PM", "CASE_MANAGER") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 365 {
		days = d
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT workflow,
		       count(DISTINCT session_id) AS sessions,
		       count(*) FILTER (WHERE action='click') AS clicks,
		       coalesce(sum((meta->>'keys')::int) FILTER (WHERE action='keystroke'), 0) AS keystrokes,
		       count(*) FILTER (WHERE action='submit') AS submits,
		       count(*) FILTER (WHERE action='prefill_accept') AS prefill_accept,
		       count(*) FILTER (WHERE action='prefill_override') AS prefill_override,
		       count(*) FILTER (WHERE action='next_action_accept') AS na_accept,
		       count(*) FILTER (WHERE action='next_action_override') AS na_override
		FROM public.interaction_events
		WHERE tenant=$1 AND created_at > now() - make_interval(days => $2)
		GROUP BY workflow ORDER BY sessions DESC`, tenant, days)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var wf string
		var sessions, clicks, keystrokes, submits, pa, po, naa, nao int64
		if rows.Scan(&wf, &sessions, &clicks, &keystrokes, &submits, &pa, &po, &naa, &nao) != nil {
			continue
		}
		row := map[string]any{
			"workflow": wf, "sessions": sessions, "clicks": clicks,
			"keystrokes": keystrokes, "submits": submits,
			"prefill_accept": pa, "prefill_override": po,
			"next_action_accept": naa, "next_action_override": nao,
		}
		if sessions > 0 {
			row["clicks_per_session"] = float64(clicks) / float64(sessions)
			row["keystrokes_per_session"] = float64(keystrokes) / float64(sessions)
		}
		if pa+po > 0 {
			row["prefill_accept_rate"] = float64(pa) / float64(pa+po)
		}
		if naa+nao > 0 {
			row["next_action_accept_rate"] = float64(naa) / float64(naa+nao)
		}
		out = append(out, row)
	}
	rows.Close()
	// Each task (filing a dispute, approving a draft, sending a letter...): how many were done, how many refused,
	// and what a typical one took -- the median, so one left open over lunch doesn't skew it.
	tasks := []map[string]any{}
	if trows, err := s.db.Query(r.Context(), `
		SELECT workflow, target,
		       count(*) FILTER (WHERE action='task_done') AS done,
		       count(*) FILTER (WHERE action='task_error') AS errors,
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY (meta->>'ms')::float8) FILTER (WHERE action='task_done' AND meta ? 'ms') AS median_ms,
		       avg((meta->>'clicks')::float8) FILTER (WHERE action='task_done' AND meta ? 'clicks') AS avg_clicks,
		       avg((meta->>'keys')::float8) FILTER (WHERE action='task_done' AND meta ? 'keys') AS avg_keys,
		       count(*) FILTER (WHERE action='task_done' AND (meta->>'prefilled')::int > 0) AS with_prefill
		FROM public.interaction_events
		WHERE tenant=$1 AND created_at > now() - make_interval(days => $2) AND action IN ('task_done','task_error') AND target <> ''
		GROUP BY workflow, target ORDER BY done DESC, errors DESC LIMIT 100`, tenant, days); err == nil {
		for trows.Next() {
			var wf, task string
			var done, errs, withPrefill int64
			var med, clicks, keys *float64
			if trows.Scan(&wf, &task, &done, &errs, &med, &clicks, &keys, &withPrefill) == nil {
				t := map[string]any{"workflow": wf, "task": task, "done": done, "errors": errs, "with_prefill": withPrefill}
				if med != nil {
					t["median_seconds"] = *med / 1000
				}
				if clicks != nil {
					t["avg_clicks"] = *clicks
				}
				if keys != nil {
					t["avg_keys"] = *keys
				}
				tasks = append(tasks, t)
			}
		}
		trows.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "workflows": out, "tasks": tasks, "generated_at": time.Now().UTC()})
}
