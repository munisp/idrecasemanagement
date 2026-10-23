package main

// onboarding_review.go — what TPA and health plan onboarding packages share:
// the package status recomputed from its slot verdicts, the reset when a
// document is replaced, and the staff review of documents doc-intel could
// not decide (NEEDS_REVIEW).
//
// doc-intel never accepts on doubt: an illegible scan, an ambiguous type, a
// slot the merged-PDF splitter could not find, or a plan offer without the
// QPA methodology all land in NEEDS_REVIEW. Without a person to decide, such
// a slot held its package back for good. Staff now see every one of them in
// a queue, open the document, and accept or reject it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// onboardingKind is one of the two onboarding rails.
type onboardingKind struct {
	prefix   string // table prefix: tpa | hp
	ownerCol string // the package's owner column
	label    string
}

var onboardingKinds = map[string]onboardingKind{
	"tpa": {"tpa", "client_id", "TPA"},
	"hp":  {"hp", "party_id", "Health plan"},
}

// onboardingReviewRoles decide documents doc-intel left for a person: the
// Reviewer owns intake, the PM is the QA gate.
var onboardingReviewRoles = []string{"CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

// refreshOnboardingPackage recomputes a package's status from its documents
// (READY once all of them are read) and then its owner's onboarding status.
// The same rule doc-intel applies after each verdict.
func (s *server) refreshOnboardingPackage(r *http.Request, tenant string, k onboardingKind, pkgID string) string {
	var owner, current string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT %s::text, status FROM public.%s_packages WHERE tenant=$1 AND id=$2`, k.ownerCol, k.prefix),
		tenant, pkgID).Scan(&owner, &current); err != nil || current == "FILED" {
		return current
	}
	// READY once every document has been read (packageFileable): a flagged one is the Reviewer's to look at, and
	// never holds the set back.
	var docs, reading int
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT count(*), count(*) FILTER (WHERE status IN ('PENDING','ANALYZING'))
		FROM public.%s_package_docs WHERE tenant=$1 AND package_id=$2`, k.prefix),
		tenant, pkgID).Scan(&docs, &reading)
	status := "VALIDATING"
	if docs > 0 && reading == 0 {
		status = "READY"
	}
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(
		`UPDATE public.%s_packages SET status=$3, updated_at=now() WHERE tenant=$1 AND id=$2`, k.prefix),
		tenant, pkgID, status)
	if k.prefix == "hp" {
		s.recomputeHpPartyStatus(r, tenant, owner)
	} else {
		s.recomputeClientStatus(r, tenant, owner)
	}
	return status
}

// packageDocReplaced: a new document went into a slot, so the package is
// not ready until doc-intel has read it (a READY package with an unread
// document in it would otherwise still file).
func (s *server) packageDocReplaced(r *http.Request, tenant string, k onboardingKind, pkgID string) {
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE public.%s_packages SET status='VALIDATING', updated_at=now()
		WHERE tenant=$1 AND id=$2 AND status<>'FILED'`, k.prefix), tenant, pkgID)
	var owner string
	if s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT %s::text FROM public.%s_packages WHERE tenant=$1 AND id=$2`, k.ownerCol, k.prefix),
		tenant, pkgID).Scan(&owner) == nil {
		if k.prefix == "hp" {
			s.recomputeHpPartyStatus(r, tenant, owner)
		} else {
			s.recomputeClientStatus(r, tenant, owner)
		}
	}
}

// onboardingReviews: GET /onboarding/reviews -- every slot waiting for a
// person, both rails, oldest first.
func (s *server) onboardingReviews(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, onboardingReviewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), `
		SELECT 'tpa', d.id::text, d.slot, coalesce(d.filename,''), d.reasons::text, coalesce(d.analyzed_at::text,''),
		       pk.id::text, pk.mode, o.name, c.party_name
		FROM public.tpa_package_docs d
		JOIN public.tpa_packages pk ON pk.id=d.package_id
		JOIN public.tpas o ON o.id=pk.tpa_id
		JOIN public.tpa_clients c ON c.id=pk.client_id
		WHERE d.tenant=$1 AND d.status='NEEDS_REVIEW' AND pk.status<>'FILED'
		UNION ALL
		SELECT 'hp', d.id::text, d.slot, coalesce(d.filename,''), d.reasons::text, coalesce(d.analyzed_at::text,''),
		       pk.id::text, pk.mode, o.name, c.party_name
		FROM public.hp_package_docs d
		JOIN public.hp_packages pk ON pk.id=d.package_id
		JOIN public.health_plans o ON o.id=pk.plan_id
		JOIN public.hp_parties c ON c.id=pk.party_id
		WHERE d.tenant=$1 AND d.status='NEEDS_REVIEW' AND pk.status<>'FILED'
		ORDER BY 6`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var kind, id, slot, fn, reasons, at, pkg, mode, org, party string
		if rows.Scan(&kind, &id, &slot, &fn, &reasons, &at, &pkg, &mode, &org, &party) == nil {
			out = append(out, map[string]any{
				"kind": kind, "doc_id": id, "slot": slot, "filename": fn, "reasons": json.RawMessage(reasons),
				"analyzed_at": at, "package_id": pkg, "mode": mode, "organization": org, "party": party,
				"rail": onboardingKinds[kind].label})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": out})
}

// onboardingReviewDoc looks up one NEEDS_REVIEW-able document of a rail.
func (s *server) onboardingReviewDoc(r *http.Request, tenant string, k onboardingKind, docID string) (pkgID, slot, status, objectKey, filename string, err error) {
	err = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT package_id::text, slot, status, coalesce(object_key,''), coalesce(filename,'')
		FROM public.%s_package_docs WHERE tenant=$1 AND id=$2`, k.prefix),
		tenant, docID).Scan(&pkgID, &slot, &status, &objectKey, &filename)
	return
}

// onboardingReviewFile: GET /onboarding/reviews/{kind}/{docId}/file -- the
// document itself, opened through the vault, so the reviewer can read it.
func (s *server) onboardingReviewFile(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, onboardingReviewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	k, ok := onboardingKinds[chi.URLParam(r, "kind")]
	if !ok {
		http.Error(w, `{"error":"kind must be tpa or hp"}`, http.StatusBadRequest)
		return
	}
	viewPkg, _, _, key, filename, err := s.onboardingReviewDoc(r, tenant, k, chi.URLParam(r, "docId"))
	if err != nil || key == "" {
		http.Error(w, `{"error":"document not found"}`, http.StatusNotFound)
		return
	}
	obj, err := s.docs.mc.GetObject(r.Context(), docBucket, key, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, `{"error":"object missing"}`, http.StatusNotFound)
		return
	}
	defer obj.Close()
	ct, err := io.ReadAll(obj)
	if err != nil {
		http.Error(w, `{"error":"object missing"}`, http.StatusNotFound)
		return
	}
	pt, err := s.vaultOpenDoc(r, tenant, key, ct)
	if err != nil {
		http.Error(w, `{"error":"decrypt failed"}`, http.StatusBadGateway)
		return
	}
	s.logAudit(r.Context(), tenant, "", "ONBOARDING_DOC_VIEWED", map[string]any{"kind": k.prefix, "doc_id": chi.URLParam(r, "docId"), "by": p.Subject,
		"org": s.packageOrg(r.Context(), tenant, k, viewPkg)})
	ctype := "application/octet-stream"
	if strings.HasPrefix(string(pt), "%PDF") {
		ctype = "application/pdf"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filename))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(pt)
}

// onboardingReviewDecide: POST /onboarding/reviews/{kind}/{docId}
// {"decision":"ACCEPT"|"REJECT","note":"..."} -- only for NEEDS_REVIEW.
func (s *server) onboardingReviewDecide(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, onboardingReviewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	k, ok := onboardingKinds[chi.URLParam(r, "kind")]
	if !ok {
		http.Error(w, `{"error":"kind must be tpa or hp"}`, http.StatusBadRequest)
		return
	}
	var in struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	in.Decision = strings.ToUpper(strings.TrimSpace(in.Decision))
	in.Note = strings.TrimSpace(in.Note)
	if in.Decision != "ACCEPT" && in.Decision != "REJECT" {
		http.Error(w, `{"error":"decision must be ACCEPT or REJECT"}`, http.StatusBadRequest)
		return
	}
	if in.Decision == "REJECT" && in.Note == "" {
		http.Error(w, `{"error":"say why the document is rejected -- the filer sees the note"}`, http.StatusBadRequest)
		return
	}
	docID := chi.URLParam(r, "docId")
	pkgID, slot, status, _, _, err := s.onboardingReviewDoc(r, tenant, k, docID)
	if err != nil {
		http.Error(w, `{"error":"document not found"}`, http.StatusNotFound)
		return
	}
	if status != "NEEDS_REVIEW" {
		http.Error(w, fmt.Sprintf(`{"error":"only a document waiting for review can be decided (this one is %s)"}`, status), http.StatusConflict)
		return
	}
	newStatus, code := "ACCEPTED", "STAFF_ACCEPTED"
	if in.Decision == "REJECT" {
		newStatus, code = "REJECTED", "STAFF_REJECTED"
	}
	who := displayName(p)
	reason, _ := json.Marshal([]map[string]string{{"code": code, "detail": strings.TrimSpace(who + ": " + in.Note)}})
	tag, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE public.%s_package_docs SET status=$3, reasons=reasons || $4::jsonb, analyzed_at=now()
		WHERE tenant=$1 AND id=$2 AND status='NEEDS_REVIEW'`, k.prefix),
		tenant, docID, newStatus, string(reason))
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, `{"error":"the document changed meanwhile -- reload"}`, http.StatusConflict)
		return
	}
	pkgStatus := s.refreshOnboardingPackage(r, tenant, k, pkgID)
	s.logAudit(r.Context(), tenant, "", "ONBOARDING_DOC_REVIEWED", map[string]any{
		"kind": k.prefix, "doc_id": docID, "package_id": pkgID, "slot": slot,
		"decision": in.Decision, "note": in.Note, "by": p.Subject, "org": s.packageOrg(r.Context(), tenant, k, pkgID)})
	writeJSON(w, http.StatusOK, map[string]any{"doc_id": docID, "slot": slot, "status": newStatus, "package_status": pkgStatus})
}

// packageOrg: the organization a package belongs to ("tpa:<id>" / "plan:<id>"), for its audit trail.
func (s *server) packageOrg(ctx context.Context, tenant string, k onboardingKind, pkgID string) string {
	col, kind := "tpa_id", "tpa"
	if k.prefix == "hp" {
		col, kind = "plan_id", "plan"
	}
	var id string
	if s.db.QueryRow(ctx, fmt.Sprintf(`SELECT %s::text FROM public.%s_packages WHERE tenant=$1 AND id::text=$2`, col, k.prefix), tenant, pkgID).Scan(&id) != nil {
		return ""
	}
	return kind + ":" + id
}
