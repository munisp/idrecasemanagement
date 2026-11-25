// drafts.go — save as you go: a signed-in user's unfinished dispute forms.
//
// A login can keep several drafts of each form. Each has a short reference to quote (D-4F2A9C) and a summary for the
// list (client, provider, claim, amount). A draft has no case number: that is given when the dispute is filed.
//
//	GET    /drafts                    the caller's drafts, newest first (?form= narrows to one form)
//	POST   /drafts/{form}             start a draft          body {"data":{…}, "summary":{…}}
//	GET    /drafts/{form}/{draftId}   one draft
//	PUT    /drafts/{form}/{draftId}   save it                body {"data":{…}, "summary":{…}}
//	DELETE /drafts/{form}/{draftId}   discard it (or: it was filed)
//
// GET, PUT, DELETE /drafts/{form} (no id) still work on the caller's newest draft of that form, for a page loaded
// before multiple drafts existed.
//
// A draft belongs to the login that typed it: its owner comes from the verified token and is part of every lookup, so
// nobody can read another's. It can hold patient details, so it is removed when the dispute is filed or discarded,
// and purged after 90 days unused. Autosaves are not audited (orgAuditQuiet): filing is.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	chi "github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const filingDraftMaxBytes = 12 << 20 // a filing's own limit (maxFilingBodyBytes), and a little to spare

// filingDraftMaxPerForm caps one login's drafts of one form: enough for any real backlog, and a runaway script can't
// fill the table.
const filingDraftMaxPerForm = 50

// filingDraftForms are the forms with drafts.
var filingDraftForms = map[string]bool{"tpa-file": true, "hp-file": true, "intake": true, "new-dispute": true}

// filingDraftSummaryKeys are what a draft's list line may show. Patient details are deliberately not among them.
var filingDraftSummaryKeys = map[string]bool{"client": true, "provider": true, "other_party": true, "claim": true, "amount": true}

type filingDraftBody struct {
	Data    json.RawMessage   `json:"data"`
	Summary map[string]string `json:"summary"`
}

// cleanDraftSummary keeps the known keys, trimmed and capped.
func cleanDraftSummary(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		v = strings.TrimSpace(v)
		if !filingDraftSummaryKeys[k] || v == "" {
			continue
		}
		if len(v) > 120 {
			v = v[:120]
		}
		out[k] = v
	}
	return out
}

func newDraftRef() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "D-" + strings.ToUpper(hex.EncodeToString(b))
}

func readDraftBody(w http.ResponseWriter, r *http.Request) (filingDraftBody, bool) {
	var body filingDraftBody
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, filingDraftMaxBytes))
	if err != nil {
		http.Error(w, `{"error":"this draft is too large to save"}`, http.StatusRequestEntityTooLarge)
		return body, false
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Data) == 0 || !json.Valid(body.Data) {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return body, false
	}
	return body, true
}

func draftOwner(r *http.Request) (tenant, sub string) {
	return r.Context().Value(ctxTenant{}).(string), r.Context().Value(ctxPrincipal{}).(principal).Subject
}

// filingDraftList: the caller's drafts, newest first.
func (s *server) filingDraftList(w http.ResponseWriter, r *http.Request) {
	tenant, sub := draftOwner(r)
	form := r.URL.Query().Get("form")
	if form != "" && !filingDraftForms[form] {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT id::text, form, ref, summary, created_at, updated_at FROM public.filing_drafts
		WHERE tenant=$1 AND user_sub=$2 AND ($3='' OR form=$3) ORDER BY updated_at DESC LIMIT 200`, tenant, sub, form)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id, f, ref string
		var summary json.RawMessage
		var created, updated time.Time
		if rows.Scan(&id, &f, &ref, &summary, &created, &updated) != nil {
			continue
		}
		list = append(list, map[string]any{"id": id, "form": f, "ref": ref, "summary": summary, "created_at": created, "updated_at": updated})
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts": list})
}

// filingDraftItem: start, read, save or discard one draft.
func (s *server) filingDraftItem(w http.ResponseWriter, r *http.Request) {
	tenant, sub := draftOwner(r)
	form, id := chi.URLParam(r, "form"), chi.URLParam(r, "draftId")
	if !filingDraftForms[form] {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodPost:
		body, ok := readDraftBody(w, r)
		if !ok {
			return
		}
		var n int
		_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM public.filing_drafts WHERE tenant=$1 AND user_sub=$2 AND form=$3`,
			tenant, sub, form).Scan(&n)
		if n >= filingDraftMaxPerForm {
			http.Error(w, `{"error":"you have 50 drafts of this form: file or discard some before starting another"}`, http.StatusConflict)
			return
		}
		summary, _ := json.Marshal(cleanDraftSummary(body.Summary))
		for attempt := 0; attempt < 5; attempt++ {
			ref := newDraftRef()
			var newID string
			var at time.Time
			err := s.db.QueryRow(r.Context(), `
				INSERT INTO public.filing_drafts (tenant, user_sub, form, data, ref, summary) VALUES ($1,$2,$3,$4::jsonb,$5,$6::jsonb)
				RETURNING id::text, updated_at`, tenant, sub, form, string(body.Data), ref, string(summary)).Scan(&newID, &at)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				continue // the reference was taken: draw another
			}
			if err != nil {
				http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"id": newID, "ref": ref, "updated_at": at})
			return
		}
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
	case http.MethodGet:
		var data, summary json.RawMessage
		var ref string
		var at time.Time
		err := s.db.QueryRow(r.Context(), `
			SELECT data, ref, summary, updated_at FROM public.filing_drafts
			WHERE tenant=$1 AND user_sub=$2 AND form=$3 AND id::text=$4`, tenant, sub, form, id).Scan(&data, &ref, &summary, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, `{"error":"this draft no longer exists: it was filed or discarded"}`, http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "ref": ref, "summary": summary, "draft": data, "updated_at": at})
	case http.MethodPut:
		body, ok := readDraftBody(w, r)
		if !ok {
			return
		}
		summary, _ := json.Marshal(cleanDraftSummary(body.Summary))
		var at time.Time
		err := s.db.QueryRow(r.Context(), `
			UPDATE public.filing_drafts SET data=$5::jsonb, summary=$6::jsonb, updated_at=now()
			WHERE tenant=$1 AND user_sub=$2 AND form=$3 AND id::text=$4 RETURNING updated_at`,
			tenant, sub, form, id, string(body.Data), string(summary)).Scan(&at)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, `{"error":"this draft no longer exists: it was filed or discarded"}`, http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"updated_at": at})
	case http.MethodDelete:
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.filing_drafts WHERE tenant=$1 AND user_sub=$2 AND form=$3 AND id::text=$4`,
			tenant, sub, form, id)
		w.WriteHeader(http.StatusNoContent)
	}
}

// filingDraft: the routes without an id, kept for a page loaded before multiple drafts. They act on the caller's
// newest draft of the form.
func (s *server) filingDraft(w http.ResponseWriter, r *http.Request) {
	tenant, sub := draftOwner(r)
	form := chi.URLParam(r, "form")
	if !filingDraftForms[form] {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	const newest = `SELECT id FROM public.filing_drafts WHERE tenant=$1 AND user_sub=$2 AND form=$3 ORDER BY updated_at DESC LIMIT 1`
	switch r.Method {
	case http.MethodGet:
		var data json.RawMessage
		var at time.Time
		err := s.db.QueryRow(r.Context(), `SELECT data, updated_at FROM public.filing_drafts WHERE id=(`+newest+`)`,
			tenant, sub, form).Scan(&data, &at)
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
		err = s.db.QueryRow(r.Context(), `UPDATE public.filing_drafts SET data=$4::jsonb, updated_at=now() WHERE id=(`+newest+`) RETURNING updated_at`,
			tenant, sub, form, string(raw)).Scan(&at)
		if errors.Is(err, pgx.ErrNoRows) {
			err = s.db.QueryRow(r.Context(), `INSERT INTO public.filing_drafts (tenant, user_sub, form, data, ref) VALUES ($1,$2,$3,$4::jsonb,$5) RETURNING updated_at`,
				tenant, sub, form, string(raw), newDraftRef()).Scan(&at)
		}
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"updated_at": at})
	case http.MethodDelete:
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.filing_drafts WHERE id=(`+newest+`)`, tenant, sub, form)
		w.WriteHeader(http.StatusNoContent)
	}
}

// purgeOldDrafts removes drafts left unused for 90 days.
func (s *server) purgeOldDrafts(ctx context.Context) {
	_, _ = s.db.Exec(ctx, `DELETE FROM public.filing_drafts WHERE updated_at < now() - interval '90 days'`)
}
