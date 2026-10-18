package main

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Document sets, changed until they are filed (scripts/package_doc_history.sql): a TPA replaces any slot's file
// -- one that was not accepted, or one that was the wrong file -- or removes it, and nothing is lost. The file a
// slot held goes to package_doc_history with its verdict, who uploaded it and who replaced it; the TPA sees it as
// the slot's earlier versions. Every upload, replacement and removal is in the tamper-evident audit log, tagged
// with the TPA (auditOrg), so it shows on the TPA's own Audit log and the office's.

type prevPackageDoc struct{ filename, status string }

// uploaderName: who is uploading, as people read it.
func uploaderName(r *http.Request) string {
	if p, ok := r.Context().Value(ctxPrincipal{}).(principal); ok {
		return displayName(p)
	}
	return "system"
}

// endPackageDoc moves a slot's current file into its history (REPLACED or REMOVED). It reports what the slot
// held and whether it held anything.
func (s *server) endPackageDoc(r *http.Request, tenant, pkgID, slot, by, how string) (prevPackageDoc, bool) {
	var prev prevPackageDoc
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.package_doc_history (tenant, kind, package_id, slot, filename, object_key, status, reasons,
		       uploaded_by, uploaded_at, ended_by, ended_how)
		SELECT tenant, 'tpa', package_id, slot, coalesce(filename,''), coalesce(object_key,''), status, reasons,
		       coalesce(uploaded_by,''), coalesce(uploaded_at, created_at), $4, $5
		FROM public.tpa_package_docs WHERE tenant=$1 AND package_id=$2 AND slot=$3
		RETURNING filename, status`, tenant, pkgID, slot, by, how).Scan(&prev.filename, &prev.status)
	return prev, err == nil
}

// packageDocAudit: what every audit entry about a document set carries -- the TPA (so it shows on the TPA's own
// log), the client, the set and the slot.
func (s *server) packageDocAudit(r *http.Request, tenant, tpaID, pkgID, slot string) map[string]any {
	var client string
	_ = s.db.QueryRow(r.Context(), `
		SELECT coalesce(c.party_name,'') FROM public.tpa_packages p JOIN public.tpa_clients c ON c.id = p.client_id
		WHERE p.tenant=$1 AND p.id=$2`, tenant, pkgID).Scan(&client)
	by := ""
	if p, ok := r.Context().Value(ctxPrincipal{}).(principal); ok {
		by = p.Subject
	}
	return map[string]any{"by": by, "by_name": uploaderName(r), "tpa_id": tpaID, "package_id": pkgID,
		"client": client, "slot": slot}
}

// packageDocHistory: each slot's earlier files, newest first.
func (s *server) packageDocHistory(r *http.Request, tenant, pkgID string) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	rows, err := s.queryRows(r, `
		SELECT slot, filename, status, reasons, uploaded_by, coalesce(uploaded_at::text,'') AS uploaded_at,
		       ended_at::text AS ended_at, ended_by, ended_how
		FROM public.package_doc_history WHERE tenant=$1 AND package_id=$2 ORDER BY ended_at DESC LIMIT 300`, tenant, pkgID)
	if err != nil {
		return out // history is additive: a database not yet migrated still shows the set
	}
	for _, h := range rows {
		slot, _ := h["slot"].(string)
		out[slot] = append(out[slot], h)
	}
	return out
}

// tpaRemoveDoc: DELETE /tpa/packages/{packageId}/docs/{slot} -- takes a file out of a set that is not yet filed.
// The slot becomes MISSING; the file stays in its history.
func (s *server) tpaRemoveDoc(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	pkgID := chi.URLParam(r, "packageId")
	slot := strings.ToUpper(strings.TrimSpace(chi.URLParam(r, "slot")))
	if !tpaSlotValid(slot) {
		http.Error(w, `{"error":"slot must be one of: `+strings.Join(tpaSlots, ", ")+`"}`, http.StatusBadRequest)
		return
	}
	_, _, status, err := s.tpaPackageOwned(r, tenant, org.ID, pkgID)
	if err != nil {
		http.Error(w, `{"error":"package not found"}`, http.StatusNotFound)
		return
	}
	if status == "FILED" {
		http.Error(w, `{"error":"this document set has been filed with a dispute, so its documents can't be changed"}`, http.StatusConflict)
		return
	}
	prev, had := s.endPackageDoc(r, tenant, pkgID, slot, uploaderName(r), "REMOVED")
	if !had {
		http.Error(w, `{"error":"that slot has no document"}`, http.StatusNotFound)
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM public.tpa_package_docs WHERE tenant=$1 AND package_id=$2 AND slot=$3`,
		tenant, pkgID, slot); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	payload := s.packageDocAudit(r, tenant, org.ID, pkgID, slot)
	payload["filename"], payload["status_when_removed"] = prev.filename, prev.status
	s.logAudit(r.Context(), tenant, "", "PACKAGE_DOC_REMOVED", payload)
	s.packageDocReplaced(r, tenant, onboardingKinds["tpa"], pkgID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "removed", "slot": slot, "filename": prev.filename})
}
