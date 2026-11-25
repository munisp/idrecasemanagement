package main

// mail_preview.go — how a letter will read before it is sent.
//
// POST /cases/{caseId}/correspondence/preview takes what the compose screen holds (the same body as a draft) and
// answers with the email as draftCorrespondence would build it: the subject and text with the case's details filled
// in, who it goes to, whether it waits at the QA gate, and what each secure link will carry. Nothing is recorded,
// sent or minted: a link shows as a description of itself. Fill-ins the case can't answer ("[insert amount]") are
// listed, so a letter never goes out with them.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
)

// unfilledRe: a bracketed fill-in left in a letter ("[Filing Party full name]", "$[insert amount]").
var unfilledRe = regexp.MustCompile(`\[[A-Za-z][^\[\]\n]{1,60}\]`)

func unfilled(texts ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range texts {
		for _, m := range unfilledRe.FindAllString(t, -1) {
			if !seen[m] && !strings.HasPrefix(m, "[secure ") {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

func (s *server) previewCorrespondence(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Template     string   `json:"template"`
		Body         string   `json:"body"`
		To           []string `json:"to"`
		CC           []string `json:"cc"`
		RfiTo        string   `json:"rfi_to"`
		AutoShare    bool     `json:"auto_share"`
		AutoDownload bool     `json:"auto_download"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&in); err != nil || in.Template == "" {
		http.Error(w, `{"error":"template required"}`, http.StatusBadRequest)
		return
	}
	tpl := s.effectiveTemplate(r, tenant, caseID, in.Template)
	if tpl == nil {
		http.Error(w, `{"error":"unknown or retired template for this program"}`, http.StatusBadRequest)
		return
	}
	to, cc := nonNil(in.To), nonNil(in.CC)
	if len(to) == 0 {
		roles := tpl.To
		if tpl.Key == "rfi" {
			roles = []string{map[bool]string{true: "health_plan", false: "provider"}[in.RfiTo == "plan"]}
		}
		to = nonNil(s.resolveRecipients(r, tenant, caseID, roles))
	}
	if len(cc) == 0 {
		cc = nonNil(s.resolveRecipients(r, tenant, caseID, tpl.CC))
	}
	subject := s.renderTemplate(r, tenant, caseID, tpl.Subject)
	bodyText := in.Body
	if strings.TrimSpace(bodyText) == "" {
		bodyText = tpl.Body
	}
	if tpl.Key == "acceptance" && s.estimateDeclined(r, tenant, caseID) {
		bodyText = dropEstimateParagraphs(bodyText)
	}
	side := "[Provider Contact Name]"
	if (tpl.Key == "rfi" && in.RfiTo == "plan") || (tpl.Key != "rfi" && len(tpl.To) > 0 && (tpl.To[0] == "health_plan" || tpl.To[0] == "plan")) {
		side = "[Health Plan Contact Name]"
	}
	body := s.renderTemplate(r, tenant, caseID, strings.ReplaceAll(bodyText, "[contact name]", side))

	// The links, as what they will be.
	upload := "[secure upload link — created when the email is sent]"
	if tpl.Key == "rfi" && s.portalRFI(r.Context(), tenant, caseID, in.RfiTo) {
		upload = strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/app.html#/document-requests"
	}
	if strings.Contains(body, "{share_link}") {
		body = strings.ReplaceAll(body, "{share_link}", upload)
	} else if in.AutoShare {
		body += "\n\nSecure upload link: " + upload
	}
	carries := ""
	if in.AutoDownload || strings.Contains(body, "{download_link}") {
		carries = s.downloadLinkCarries(r, tenant, caseID, tpl.Key)
		link := "[secure download link — created when the email is sent]"
		if strings.Contains(body, "{download_link}") {
			body = strings.ReplaceAll(body, "{download_link}", link)
		} else {
			body += "\n\nSecure document download link: " + link
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"subject": subject, "body": body, "to": to, "cc": cc,
		"qa_role": tpl.QARole, "download_carries": carries,
		"unfilled": unfilled(subject, body),
	})
}

// downloadLinkCarries: what a letter's download link would give its reader, in words.
func (s *server) downloadLinkCarries(r *http.Request, tenant, caseID, tplKey string) string {
	if _, ok := letterBundles[tplKey]; ok {
		docs := s.bundleDocs(r.Context(), tenant, caseID, tplKey)
		if len(docs) == 0 {
			return "nothing yet — the documents for this letter are not filed"
		}
		names := make([]string, len(docs))
		for i, d := range docs {
			names[i] = d.name
		}
		return "one zip of: " + strings.Join(names, "; ")
	}
	if tplKey == "plan_notification" {
		return "the filing party's documents, as one zip"
	}
	var name string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT coalesce(d.filename, d.id::text) FROM tenant_%s.documents d
		WHERE d.case_id=$1 AND NOT d.sealed AND d.scan_status<>'BLOCKED' AND NOT `+internalDocSQL+`
		ORDER BY d.created_at DESC LIMIT 1`, sanitizeTenant(tenant)), caseID).Scan(&name)
	if name == "" {
		return "nothing yet — the case has no document to send"
	}
	return name
}
