package main

// mailsetup.go — GET /v1/admin/mail-setup: is the platform's mail set up, piece by piece, checked live -- the inbound
// domain at Resend (sending and receiving), its DNS records as the internet sees them, the webhook that reports
// received mail and deliveries, and the Microsoft app for Outlook. The portal's Mail setup page shows each with the
// steps to fix it. Read-only, and no secret leaves the server: only whether each one is set.

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"
)

// The webhook events the platform acts on (mailhook.go): received mail, and what became of mail sent.
var mailSetupEvents = []string{"email.received", "email.delivered", "email.bounced", "email.complained", "email.failed"}

type mailSetupRecord struct {
	Record   string `json:"record"` // DKIM, SPF, Receiving...
	Type     string `json:"type"`
	Name     string `json:"name"` // as Resend gives it: relative to the parent domain, e.g. resend._domainkey.mail-dev
	FullName string `json:"full_name"`
	Value    string `json:"value"`
	Priority int    `json:"priority,omitempty"`
	Status   string `json:"status"`
}

func (s *server) mailSetup(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	domain := s.cfg.MailboxDomain
	hookURL := strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/api/webhooks/mail"
	out := map[string]any{
		"mailbox_domain":   domain,
		"webhook_url":      hookURL,
		"sending":          map[string]any{"smtp_host": s.cfg.SMTPHost, "from": s.cfg.SMTPFrom, "configured": s.cfg.SMTPHost != ""},
		"webhook_secret":   s.cfg.MailWebhookSecret != "",
		"resend_key":       s.resendKey() != "",
		"outlook":          map[string]any{"client_id": s.cfg.OutlookClientID, "secret_set": s.cfg.OutlookClientSecret != "", "tenant": orWord(s.cfg.OutlookTenant, "common"), "redirect_url": s.cfg.OutlookRedirectURL, "admin_consent_url": s.outlookAdminConsentURL(), "on": s.outlookOn()},
		"inbound_endpoint": map[string]any{"url": strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/api/email/inbound", "token_set": s.cfg.EmailInboundToken != ""},
	}

	// The domain at Resend, with the DNS records it needs.
	dom := map[string]any{"found": false}
	if s.resendKey() != "" && domain != "" {
		var list struct {
			Data []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				Region string `json:"region"`
				Caps   struct {
					Sending   string `json:"sending"`
					Receiving string `json:"receiving"`
				} `json:"capabilities"`
			} `json:"data"`
		}
		if err := s.resendGet(ctx, "/domains", &list); err != nil {
			dom["error"] = err.Error()
		}
		for _, d := range list.Data {
			if !strings.EqualFold(d.Name, domain) {
				continue
			}
			dom = map[string]any{"found": true, "status": d.Status, "region": d.Region, "sending": d.Caps.Sending, "receiving": d.Caps.Receiving}
			var full struct {
				Records []struct {
					Record   string `json:"record"`
					Type     string `json:"type"`
					Name     string `json:"name"`
					Value    string `json:"value"`
					Priority int    `json:"priority"`
					Status   string `json:"status"`
				} `json:"records"`
			}
			if err := s.resendGet(ctx, "/domains/"+d.ID, &full); err == nil {
				parent := domain[strings.Index(domain, ".")+1:] // the zone the records go in: mail-dev.upi.dev -> upi.dev
				recs := []mailSetupRecord{}
				for _, x := range full.Records {
					recs = append(recs, mailSetupRecord{Record: x.Record, Type: x.Type, Name: x.Name, FullName: x.Name + "." + parent,
						Value: x.Value, Priority: x.Priority, Status: x.Status})
				}
				dom["records"] = recs
				dom["dns_zone"] = parent
			}
		}
	}
	out["domain"] = dom

	// What the internet sees for the domain's MX record (where mail for it is delivered).
	mx := map[string]any{"records": []string{}}
	if domain != "" {
		if recs, err := net.DefaultResolver.LookupMX(ctx, domain); err == nil {
			hosts := []string{}
			toResend := false
			for _, m := range recs {
				h := strings.TrimSuffix(strings.ToLower(m.Host), ".")
				hosts = append(hosts, h)
				if strings.HasSuffix(h, "rmta.net") || strings.Contains(h, "resend") || strings.Contains(h, "amazonses") {
					toResend = true
				}
			}
			mx = map[string]any{"records": hosts, "points_to_resend": toResend}
		} else {
			mx["error"] = "no MX record found for " + domain
		}
	}
	out["mx"] = mx

	// The webhook at Resend that calls this platform.
	hook := map[string]any{"found": false}
	if s.resendKey() != "" {
		var hooks struct {
			Data []struct {
				Endpoint string   `json:"endpoint"`
				Status   string   `json:"status"`
				Events   []string `json:"events"`
			} `json:"data"`
		}
		if err := s.resendGet(ctx, "/webhooks", &hooks); err != nil {
			hook["error"] = err.Error()
		}
		for _, h := range hooks.Data {
			if strings.TrimRight(h.Endpoint, "/") != hookURL {
				continue
			}
			missing := []string{}
			for _, e := range mailSetupEvents {
				if !contains(h.Events, e) {
					missing = append(missing, e)
				}
			}
			hook = map[string]any{"found": true, "status": h.Status, "events": h.Events, "missing_events": missing}
		}
	}
	hook["needed_events"] = mailSetupEvents
	out["webhook"] = hook
	writeJSON(w, http.StatusOK, out)
}
