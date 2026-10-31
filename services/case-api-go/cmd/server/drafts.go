// drafts.go — save as you go: a signed-in user's unfinished dispute form.
//
// GET, PUT, DELETE /drafts/{form}. A draft belongs to the login that typed it: its owner comes from the verified token
// and is the only way to one, so nobody can read another's. It can hold patient details, so it is removed when the
// dispute is filed or discarded, and purged after 90 days unused. Autosaves are not audited (orgAuditQuiet): filing is.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	chi "github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

const filingDraftMaxBytes = 12 << 20 // a filing's own limit (maxFilingBodyBytes), and a little to spare

// filingDraftForms are the forms with drafts.
var filingDraftForms = map[string]bool{"tpa-file": true, "hp-file": true, "intake": true, "new-dispute": true}

func (s *server) filingDraft(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	form := chi.URLParam(r, "form")
	if !filingDraftForms[form] {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		var data json.RawMessage
		var at time.Time
		err := s.db.QueryRow(r.Context(), `SELECT data, updated_at FROM public.filing_drafts WHERE tenant=$1 AND user_sub=$2 AND form=$3`,
			tenant, p.Subject, form).Scan(&data, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]any{"draft": nil})
			return
		}
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"draft": data, "updated_at": at})
	case http.MethodPut:
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, filingDraftMaxBytes))
		if err != nil {
			http.Error(w, `{"error":"this draft is too large to save"}`, http.StatusRequestEntityTooLarge)
			return
		}
		if !json.Valid(raw) {
			http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
			return
		}
		var at time.Time
		if err := s.db.QueryRow(r.Context(), `
			INSERT INTO public.filing_drafts (tenant, user_sub, form, data) VALUES ($1,$2,$3,$4::jsonb)
			ON CONFLICT (tenant, user_sub, form) DO UPDATE SET data=excluded.data, updated_at=now() RETURNING updated_at`,
			tenant, p.Subject, form, string(raw)).Scan(&at); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"updated_at": at})
	case http.MethodDelete:
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.filing_drafts WHERE tenant=$1 AND user_sub=$2 AND form=$3`, tenant, p.Subject, form)
		w.WriteHeader(http.StatusNoContent)
	}
}

// purgeOldDrafts removes drafts left unused for 90 days.
func (s *server) purgeOldDrafts(ctx context.Context) {
	_, _ = s.db.Exec(ctx, `DELETE FROM public.filing_drafts WHERE updated_at < now() - interval '90 days'`)
}
