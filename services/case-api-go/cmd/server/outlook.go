// outlook.go — a staff member's own Outlook account, connected through Microsoft Graph and synchronised into their
// mail history (mailboxes.go).
//
//	connect     GET /me/outlook/connect -> Microsoft's sign-in (authorization code flow); the callback
//	            GET /api/oauth/outlook/callback stores the tokens, sealed by the vault
//	sync        every 5 minutes, and on demand (POST /me/outlook/sync): the inbox and sent items received or sent since
//	            the last sync (the first sync: the last 30 days), with their attachments; nothing is filed twice
//	send        POST /me/mail/send with via=outlook sends through the person's own account (Graph sendMail)
//	disconnect  DELETE /me/outlook forgets the tokens; the mail already synchronised stays in the history
//
// Needs OUTLOOK_CLIENT_ID, OUTLOOK_CLIENT_SECRET and OUTLOOK_REDIRECT_URL (OUTLOOK_TENANT: "common" for any Microsoft
// account, or one organization's directory id). Off while they are empty.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	graphAPI      = "https://graph.microsoft.com/v1.0"
	outlookScopes = "offline_access User.Read Mail.ReadWrite Mail.Send"
	// Reading a shared mailbox the signed-in account has access to (it cannot sign in itself). Asked for only then:
	// personal Microsoft accounts have no shared mailboxes and refuse these scopes.
	outlookSharedScopes = outlookScopes + " Mail.ReadWrite.Shared Mail.Send.Shared"
	// The owner of a program's company mailbox connection (a person's connection is under their Keycloak id).
	companySub          = "company"
	outlookFirstWindow  = 30 * 24 * time.Hour
	outlookSyncInterval = 5 * time.Minute
	outlookPageLimit    = 20 // pages of 50 per folder per sync
)

// outlookScopesFor: what to ask Microsoft for -- an account's own mail, or also a shared mailbox's.
func outlookScopesFor(sharedMailbox string) string {
	if sharedMailbox != "" {
		return outlookSharedScopes
	}
	return outlookScopes
}

func (s *server) outlookOn() bool {
	return s.cfg.OutlookClientID != "" && s.cfg.OutlookClientSecret != "" && s.cfg.OutlookRedirectURL != ""
}

func (s *server) outlookLogin(path string) string {
	return "https://login.microsoftonline.com/" + url.PathEscape(orWord(s.cfg.OutlookTenant, "common")) + "/oauth2/v2.0/" + path
}

type outlookState struct {
	Connected  bool       `json:"connected"`
	Status     string     `json:"status,omitempty"`
	Email      string     `json:"email,omitempty"`
	Name       string     `json:"display_name,omitempty"`
	LastSyncAt *time.Time `json:"last_sync_at,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	Mailbox    string     `json:"mailbox,omitempty"` // a shared mailbox read through Email's access (the company's)
}

func (s *server) outlookStatus(ctx context.Context, tenant, sub string) outlookState {
	var o outlookState
	if s.db.QueryRow(ctx, `SELECT status, email, display_name, last_sync_at, last_error, mailbox FROM public.outlook_connections
		WHERE tenant=$1 AND user_sub=$2 AND status <> 'DISCONNECTED'`, tenant, sub).Scan(&o.Status, &o.Email, &o.Name, &o.LastSyncAt, &o.LastError, &o.Mailbox) == nil {
		o.Connected = true
	}
	return o
}

// The OAuth state: tenant|sub|expiry|mailbox|by, signed with the client secret, so the callback knows whose account it
// is (a person's, or the company's: which mailbox, and which admin connected it) and that the flow started here.
type oauthState struct{ tenant, sub, mailbox, by string }

func (s *server) outlookSignState(st oauthState, exp time.Time) string {
	payload := strings.Join([]string{st.tenant, st.sub, strconv.FormatInt(exp.Unix(), 10), st.mailbox, st.by}, "|")
	mac := hmac.New(sha256.New, []byte("outlook-state:"+s.cfg.OutlookClientSecret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *server) outlookVerifyState(state string, now time.Time) (oauthState, bool) {
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		return oauthState{}, false
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	sig, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	if err1 != nil || err2 != nil {
		return oauthState{}, false
	}
	mac := hmac.New(sha256.New, []byte("outlook-state:"+s.cfg.OutlookClientSecret))
	mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return oauthState{}, false
	}
	f := strings.Split(string(raw), "|")
	if len(f) != 5 {
		return oauthState{}, false
	}
	exp, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || now.Unix() > exp {
		return oauthState{}, false
	}
	return oauthState{tenant: f[0], sub: f[1], mailbox: f[3], by: f[4]}, true
}

// outlookAdminConsentURL: the link a person hands their organization's IT admin when Microsoft says "Need admin
// approval". Opening it, the admin approves the app once for everyone in their organization; Microsoft then returns
// them to the callback with admin_consent=True.
func (s *server) outlookAdminConsentURL() string {
	if !s.outlookOn() {
		return ""
	}
	q := url.Values{"client_id": {s.cfg.OutlookClientID}, "redirect_uri": {s.cfg.OutlookRedirectURL}}
	return "https://login.microsoftonline.com/organizations/adminconsent?" + q.Encode()
}

// outlookAuthorizeURL: Microsoft's sign-in page for connecting an account, valid for 15 minutes.
func (s *server) outlookAuthorizeURL(st oauthState) string {
	q := url.Values{
		"client_id": {s.cfg.OutlookClientID}, "response_type": {"code"}, "redirect_uri": {s.cfg.OutlookRedirectURL},
		"response_mode": {"query"}, "scope": {outlookScopesFor(st.mailbox)}, "prompt": {"select_account"},
		"state": {s.outlookSignState(st, time.Now().Add(15*time.Minute))},
	}
	return s.outlookLogin("authorize") + "?" + q.Encode()
}

// outlookConnect: GET /me/outlook/connect -- the Microsoft sign-in address to send the person to.
func (s *server) outlookConnect(w http.ResponseWriter, r *http.Request) {
	m, p, err := s.myMailbox(r)
	if err != nil {
		mailErr(w, err)
		return
	}
	if !s.outlookOn() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Outlook isn't set up for this platform yet (OUTLOOK_CLIENT_ID)"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": s.outlookAuthorizeURL(oauthState{tenant: m.Tenant, sub: p.Subject, by: p.Subject})})
}

type graphToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func (s *server) outlookToken(ctx context.Context, scopes string, form url.Values) (graphToken, error) {
	form.Set("client_id", s.cfg.OutlookClientID)
	form.Set("client_secret", s.cfg.OutlookClientSecret)
	form.Set("scope", scopes)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.outlookLogin("token"), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var t graphToken
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return t, err
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t)
	if resp.StatusCode != http.StatusOK || t.AccessToken == "" {
		return t, fmt.Errorf("Microsoft refused the sign-in: %s", orWord(firstLine(t.Description), orWord(t.Error, resp.Status)))
	}
	return t, nil
}

// outlookDeclineResult: why Microsoft sent the person back without an account -- their organization requires an
// admin's approval first (AADSTS65001 / 90094, consent_required), or they cancelled.
func outlookDeclineResult(errCode, desc string) string {
	if errCode == "consent_required" || strings.Contains(desc, "AADSTS65001") || strings.Contains(desc, "AADSTS90094") ||
		strings.Contains(strings.ToLower(desc), "admin") {
		return "needs-admin"
	}
	return "declined"
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// outlookCallback: GET /api/oauth/outlook/callback -- Microsoft sends the person back here with a code.
func (s *server) outlookCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if ac := q.Get("admin_consent"); ac != "" && q.Get("state") == "" {
		// Back from the approval link (outlookAdminConsentURL): no one's account is connected here, the organization
		// has only approved the app.
		result := "admin-approved"
		if !strings.EqualFold(ac, "true") || q.Get("error") != "" {
			result = "admin-declined"
		}
		slog.Info("outlook admin consent", "result", result, "tenant", q.Get("tenant"), "error", q.Get("error"))
		http.Redirect(w, r, strings.TrimRight(s.cfg.PortalBaseURL, "/")+"/app.html#/inbox?outlook="+result, http.StatusFound)
		return
	}
	st, ok := s.outlookVerifyState(q.Get("state"), time.Now())
	tenant, sub := st.tenant, st.sub
	back := func(result string) {
		page := "#/inbox" // a person's own Outlook: back to their email; the company's: back to the mail log
		if sub == companySub {
			page = "#/mail"
		}
		http.Redirect(w, r, strings.TrimRight(s.cfg.PortalBaseURL, "/")+"/app.html"+page+"?outlook="+url.QueryEscape(result), http.StatusFound)
	}
	if !s.outlookOn() {
		back("not-configured")
		return
	}
	if !ok {
		back("expired")
		return
	}
	if e := q.Get("error"); e != "" {
		slog.Info("outlook connect declined", "error", e, "detail", q.Get("error_description"))
		back(outlookDeclineResult(e, q.Get("error_description")))
		return
	}
	t, err := s.outlookToken(r.Context(), outlookScopesFor(st.mailbox), url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {s.cfg.OutlookRedirectURL}})
	if err != nil {
		slog.Warn("outlook token exchange failed", "err", err)
		back("failed")
		return
	}
	var me struct {
		ID          string `json:"id"`
		Mail        string `json:"mail"`
		UPN         string `json:"userPrincipalName"`
		DisplayName string `json:"displayName"`
	}
	if err := graphJSON(r.Context(), http.MethodGet, graphAPI+"/me?$select=id,mail,userPrincipalName,displayName", t.AccessToken, nil, &me); err != nil {
		back("failed")
		return
	}
	account := strings.ToLower(orWord(me.Mail, me.UPN))
	mailbox := ""
	if st.mailbox != "" && !strings.EqualFold(st.mailbox, account) {
		// A shared mailbox: the admin who signed in must be able to open it.
		mailbox = strings.ToLower(st.mailbox)
		if err := graphJSON(r.Context(), http.MethodGet, graphAPI+"/users/"+url.PathEscape(mailbox)+"/mailFolders/inbox?$select=id", t.AccessToken, nil, nil); err != nil {
			slog.Info("company mailbox not accessible", "mailbox", mailbox, "account", account, "err", err)
			back("no-access")
			return
		}
	}
	if err := s.outlookStoreTokens(r.Context(), tenant, sub, t, me.ID, account, me.DisplayName, mailbox, st.by); err != nil {
		slog.Error("outlook tokens not stored", "err", err)
		back("failed")
		return
	}
	event := "OUTLOOK_CONNECTED"
	if sub == companySub {
		event = "COMPANY_OUTLOOK_CONNECTED"
	}
	s.logAudit(r.Context(), tenant, "", event, map[string]any{"by": st.by, "account": account, "mailbox": orWord(mailbox, account)})
	go s.syncOutlook(context.Background(), tenant, sub) // the first sync starts now, not in 5 minutes
	back("connected")
}

// mailbox and by are given on connect (the shared mailbox read, or "" for the account's own; who connected it) and ""
// on a token refresh, which keeps them.
func (s *server) outlookStoreTokens(ctx context.Context, tenant, sub string, t graphToken, msID, email, name, mailbox, by string) error {
	key := fmt.Sprintf("%s/outlook/%s", tenant, sub)
	at, err := s.vaultSealDoc(nil, tenant, key+"/access", []byte(t.AccessToken))
	if err != nil {
		return err
	}
	var rt []byte
	if t.RefreshToken != "" {
		if rt, err = s.vaultSealDoc(nil, tenant, key+"/refresh", []byte(t.RefreshToken)); err != nil {
			return err
		}
	}
	exp := time.Now().Add(time.Duration(t.ExpiresIn-60) * time.Second)
	_, err = s.db.Exec(ctx, `
		INSERT INTO public.outlook_connections (tenant, user_sub, ms_user_id, email, display_name, access_token_sealed, refresh_token_sealed, token_expires_at, status, mailbox, connected_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'ACTIVE',$9,$10)
		ON CONFLICT (tenant, user_sub) DO UPDATE SET
		  mailbox = CASE WHEN EXCLUDED.connected_by <> '' THEN EXCLUDED.mailbox ELSE outlook_connections.mailbox END,
		  connected_by = CASE WHEN EXCLUDED.connected_by <> '' THEN EXCLUDED.connected_by ELSE outlook_connections.connected_by END,
		  connected_at = CASE WHEN EXCLUDED.connected_by <> '' THEN now() ELSE outlook_connections.connected_at END,
		  ms_user_id = CASE WHEN EXCLUDED.ms_user_id <> '' THEN EXCLUDED.ms_user_id ELSE outlook_connections.ms_user_id END,
		  email = CASE WHEN EXCLUDED.email <> '' THEN EXCLUDED.email ELSE outlook_connections.email END,
		  display_name = CASE WHEN EXCLUDED.display_name <> '' THEN EXCLUDED.display_name ELSE outlook_connections.display_name END,
		  access_token_sealed = EXCLUDED.access_token_sealed,
		  refresh_token_sealed = COALESCE(EXCLUDED.refresh_token_sealed, outlook_connections.refresh_token_sealed),
		  token_expires_at = EXCLUDED.token_expires_at, status = 'ACTIVE', last_error = '', updated_at = now()`,
		tenant, sub, msID, email, name, at, rt, exp, mailbox, by)
	return err
}

// outlookAccess: a valid access token for the person, refreshed when it has expired.
func (s *server) outlookAccess(ctx context.Context, tenant, sub string) (string, error) {
	var at, rt []byte
	var exp *time.Time
	var status, mailbox string
	if err := s.db.QueryRow(ctx, `SELECT access_token_sealed, refresh_token_sealed, token_expires_at, status, mailbox FROM public.outlook_connections
		WHERE tenant=$1 AND user_sub=$2`, tenant, sub).Scan(&at, &rt, &exp, &status, &mailbox); err != nil || status == "DISCONNECTED" {
		return "", errors.New("Outlook is not connected")
	}
	key := fmt.Sprintf("%s/outlook/%s", tenant, sub)
	if exp != nil && time.Now().Before(*exp) && len(at) > 0 {
		pt, err := s.vaultOpenDoc(nil, tenant, key+"/access", at)
		if err == nil {
			return string(pt), nil
		}
	}
	if len(rt) == 0 {
		return "", errors.New("the Outlook sign-in has expired: connect again")
	}
	refresh, err := s.vaultOpenDoc(nil, tenant, key+"/refresh", rt)
	if err != nil {
		return "", err
	}
	t, err := s.outlookToken(ctx, outlookScopesFor(mailbox), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {string(refresh)}})
	if err != nil {
		if status == "ACTIVE" {
			s.outlookBrokenNotice(ctx, tenant, sub, "the Microsoft sign-in has expired")
		}
		_, _ = s.db.Exec(ctx, `UPDATE public.outlook_connections SET status='ERROR', last_error=$3, updated_at=now() WHERE tenant=$1 AND user_sub=$2`,
			tenant, sub, truncate("sign-in expired: "+err.Error(), 500))
		return "", err
	}
	if err := s.outlookStoreTokens(ctx, tenant, sub, t, "", "", "", "", ""); err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

// outlookBase: where the connection's mail is in Graph -- the signed-in account's own (/me), or the shared mailbox the
// company connection reads (/users/{mailbox}).
func (s *server) outlookBase(ctx context.Context, tenant, sub string) string {
	var mailbox string
	_ = s.db.QueryRow(ctx, `SELECT mailbox FROM public.outlook_connections WHERE tenant=$1 AND user_sub=$2`, tenant, sub).Scan(&mailbox)
	if mailbox != "" {
		return graphAPI + "/users/" + url.PathEscape(mailbox)
	}
	return graphAPI + "/me"
}

// graphJSON: one Graph call; out may be nil.
func graphJSON(ctx context.Context, method, u, token string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Prefer", `outlook.body-content-type="html"`)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = json.Unmarshal(b, &e)
		return fmt.Errorf("Outlook (HTTP %d): %s", resp.StatusCode, orWord(e.Error.Message, e.Error.Code))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

type graphRecipient struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type graphMessage struct {
	ID                string           `json:"id"`
	Subject           string           `json:"subject"`
	From              *graphRecipient  `json:"from"`
	ToRecipients      []graphRecipient `json:"toRecipients"`
	CcRecipients      []graphRecipient `json:"ccRecipients"`
	ReceivedDateTime  string           `json:"receivedDateTime"`
	SentDateTime      string           `json:"sentDateTime"`
	InternetMessageID string           `json:"internetMessageId"`
	ConversationID    string           `json:"conversationId"`
	IsRead            bool             `json:"isRead"`
	HasAttachments    bool             `json:"hasAttachments"`
	Body              struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
}

func graphAddrs(l []graphRecipient) []string {
	out := make([]string, 0, len(l))
	for _, r := range l {
		if a := strings.ToLower(strings.TrimSpace(r.EmailAddress.Address)); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// outlookFolderURL: the folder's messages received (or sent) after `since`, oldest first, so a sync that stops early
// resumes where it stopped.
func outlookFolderURL(base, folder string, since time.Time) string {
	field := "receivedDateTime"
	if folder == "sentitems" {
		field = "sentDateTime"
	}
	q := url.Values{
		"$filter":  {fmt.Sprintf("%s gt %s", field, since.UTC().Format("2006-01-02T15:04:05Z"))},
		"$orderby": {field + " asc"},
		"$top":     {"50"},
		"$select":  {"id,subject,from,toRecipients,ccRecipients,receivedDateTime,sentDateTime,internetMessageId,conversationId,isRead,hasAttachments,body"},
	}
	return base + "/mailFolders/" + folder + "/messages?" + q.Encode()
}

// syncOutlook imports one person's new Outlook mail. Returns how many messages were filed.
func (s *server) syncOutlook(ctx context.Context, tenant, sub string) (int, error) {
	token, err := s.outlookAccess(ctx, tenant, sub)
	if err != nil {
		return 0, err
	}
	base := s.outlookBase(ctx, tenant, sub)
	var mbID string
	_ = s.db.QueryRow(ctx, `SELECT id FROM public.user_mailboxes WHERE tenant=$1 AND user_sub=$2`, tenant, sub).Scan(&mbID)
	var inboxThrough, sentThrough *time.Time
	_ = s.db.QueryRow(ctx, `SELECT inbox_synced_through, sent_synced_through FROM public.outlook_connections WHERE tenant=$1 AND user_sub=$2`,
		tenant, sub).Scan(&inboxThrough, &sentThrough)
	filed, fresh := 0, 0 // fresh: new inbox mail after the first sync (the first one brings in the last 30 days)
	var syncErr error
	var prevStatus string
	_ = s.db.QueryRow(ctx, `SELECT status FROM public.outlook_connections WHERE tenant=$1 AND user_sub=$2`, tenant, sub).Scan(&prevStatus)
	for _, f := range []struct {
		graph, folder, dir, col string
		through                 *time.Time
	}{{"inbox", "inbox", "IN", "inbox_synced_through", inboxThrough}, {"sentitems", "sent", "OUT", "sent_synced_through", sentThrough}} {
		if sub == companySub && f.dir == "OUT" {
			continue // the company mailbox is read for what comes in; its sent items are people's own business
		}
		since := time.Now().Add(-outlookFirstWindow)
		if f.through != nil {
			since = *f.through
		}
		next := outlookFolderURL(base, f.graph, since)
		newest := since
		for page := 0; next != "" && page < outlookPageLimit; page++ {
			var res struct {
				Value    []graphMessage `json:"value"`
				NextLink string         `json:"@odata.nextLink"`
			}
			if err := graphJSON(ctx, http.MethodGet, next, token, nil, &res); err != nil {
				syncErr = err
				break
			}
			for _, gm := range res.Value {
				at, ok := parseProviderTime(orWord(gm.ReceivedDateTime, gm.SentDateTime))
				if f.dir == "OUT" {
					at, ok = parseProviderTime(orWord(gm.SentDateTime, gm.ReceivedDateTime))
				}
				if !ok {
					at = time.Now()
				}
				if n, err := s.fileOutlookMessage(ctx, tenant, sub, mbID, token, base, f.folder, f.dir, gm, at); err == nil {
					filed += n
					if f.dir == "IN" && f.through != nil {
						fresh += n
					}
				}
				if at.After(newest) {
					newest = at
				}
			}
			next = res.NextLink
		}
		if newest.After(since) {
			_, _ = s.db.Exec(ctx, fmt.Sprintf(`UPDATE public.outlook_connections SET %s=$3 WHERE tenant=$1 AND user_sub=$2`, f.col), tenant, sub, newest)
		}
	}
	errText := ""
	status := "ACTIVE"
	if syncErr != nil {
		errText, status = truncate(syncErr.Error(), 500), "ERROR"
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.outlook_connections SET last_sync_at=now(), last_error=$3, status=$4, updated_at=now() WHERE tenant=$1 AND user_sub=$2 AND status <> 'DISCONNECTED'`,
		tenant, sub, errText, status)
	if fresh > 0 && sub != companySub { // the company mailbox's mail is announced as it is filed (mailfiling.go)
		msg := fmt.Sprintf("%d new emails in your Outlook", fresh)
		if fresh == 1 {
			msg = "1 new email in your Outlook"
		}
		s.notifyCtx(ctx, tenant, sub, "OUTLOOK_NEW_MAIL", msg, "#/inbox?source=OUTLOOK")
	}
	if status == "ERROR" && prevStatus == "ACTIVE" { // it just stopped working: say so once, not every 5 minutes
		s.outlookBrokenNotice(ctx, tenant, sub, errText)
	}
	return filed, syncErr
}

// fileOutlookMessage stores one Graph message (once) with its attachments. Returns 1 when it was new.
func (s *server) fileOutlookMessage(ctx context.Context, tenant, sub, mbID, token, base, folder, dir string, gm graphMessage, at time.Time) (int, error) {
	fromName, fromAddr := "", ""
	if gm.From != nil {
		fromName, fromAddr = gm.From.EmailAddress.Name, strings.ToLower(gm.From.EmailAddress.Address)
	}
	html, text := "", gm.Body.Content
	if strings.EqualFold(gm.Body.ContentType, "html") {
		html, text = gm.Body.Content, htmlToText(gm.Body.Content)
	}
	toJ, _ := json.Marshal(graphAddrs(gm.ToRecipients))
	ccJ, _ := json.Marshal(graphAddrs(gm.CcRecipients))
	status := "SYNCED"
	var mb any
	if mbID != "" {
		mb = mbID
	}
	var id string
	err := s.db.QueryRow(ctx, `INSERT INTO public.mail_messages
		(tenant, user_sub, mailbox_id, source, direction, folder, provider_message_id, internet_message_id, conversation_id,
		 from_addr, from_name, to_addrs, cc_addrs, subject, body_text, body_html, has_attachments, status, is_read, occurred_at)
		VALUES ($1,$2,$3,'OUTLOOK',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (user_sub, source, provider_message_id) WHERE provider_message_id <> '' DO NOTHING RETURNING id`,
		tenant, sub, mb, dir, folder, gm.ID, gm.InternetMessageID, gm.ConversationID, fromAddr, fromName, toJ, ccJ,
		gm.Subject, text, html, gm.HasAttachments, status, gm.IsRead || dir == "OUT", at).Scan(&id)
	if err != nil {
		return 0, err // already filed (or the insert failed): nothing new
	}
	s.mailEvent(ctx, id, "SYNCED", "from Outlook "+folder, "outlook")
	if gm.HasAttachments {
		s.fileOutlookAttachments(ctx, tenant, sub, id, token, base, gm.ID)
	}
	if dir == "IN" {
		// Outlook mail about a dispute is filed under it. The rest of a person's Outlook stays in their own inbox; the
		// rest of the company mailbox goes to the mail log's general inbound section, like the platform's own addresses.
		s.fileInbound(ctx, inboundMail{Tenant: tenant, From: fromAddr, To: graphAddrs(gm.ToRecipients), Subject: gm.Subject, Body: text,
			MessageID: gm.InternetMessageID, MailboxIDs: []string{id}, By: "outlook", Owners: []string{sub}, NotifyUnmatched: sub == companySub}, sub == companySub)
	}
	return 1, nil
}

func (s *server) fileOutlookAttachments(ctx context.Context, tenant, sub, messageID, token, base, graphID string) {
	var res struct {
		Value []struct {
			Type        string `json:"@odata.type"`
			ID          string `json:"id"`
			Name        string `json:"name"`
			ContentType string `json:"contentType"`
			Size        int64  `json:"size"`
			Content     string `json:"contentBytes"`
		} `json:"value"`
	}
	if err := graphJSON(ctx, http.MethodGet, base+"/messages/"+url.PathEscape(graphID)+"/attachments", token, nil, &res); err != nil {
		slog.Warn("outlook attachments not fetched", "err", err)
		return
	}
	for _, a := range res.Value {
		status, key := "STORED", ""
		switch {
		case !strings.HasSuffix(a.Type, "fileAttachment"):
			status = "FAILED" // an attached item or reference link, not a file
		case a.Size > maxMailAttachment:
			status = "TOO_LARGE"
		default:
			data, err := base64.StdEncoding.DecodeString(a.Content)
			if err == nil {
				key, err = s.sealMailAttachment(ctx, tenant, sub, messageID, a.ID, data)
			}
			if err != nil {
				status = "FAILED"
			}
		}
		_, _ = s.db.Exec(ctx, `INSERT INTO public.mail_attachments (message_id, filename, content_type, size_bytes, object_key, status) VALUES ($1,$2,$3,$4,$5,$6)`,
			messageID, orWord(a.Name, "attachment"), orWord(a.ContentType, "application/octet-stream"), a.Size, key, status)
	}
}

// outlookBrokenNotice tells the connection's owner (the tenant admins, for the company mailbox) that it stopped working.
func (s *server) outlookBrokenNotice(ctx context.Context, tenant, sub, why string) {
	if sub == companySub {
		s.notifyCtx(ctx, tenant, "role:TENANT_ADMIN", "COMPANY_OUTLOOK_ERROR",
			"The company Outlook mailbox stopped syncing ("+truncate(why, 120)+"). Reconnect it in the mail log.", "#/mail")
		return
	}
	s.notifyCtx(ctx, tenant, sub, "OUTLOOK_ERROR", "Your Outlook stopped syncing ("+truncate(why, 120)+"). Reconnect it in My email.", "#/inbox")
}

// outlookSyncNow: POST /me/outlook/sync
func (s *server) outlookSyncNow(w http.ResponseWriter, r *http.Request) {
	m, p, err := s.myMailbox(r)
	if err != nil {
		mailErr(w, err)
		return
	}
	n, err := s.syncOutlook(r.Context(), m.Tenant, p.Subject)
	if err != nil {
		writeJSON(w, http.StatusFailedDependency, map[string]any{"error": err.Error(), "filed": n})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"filed": n, "outlook": s.outlookStatus(r.Context(), m.Tenant, p.Subject)})
}

// outlookDisconnect: DELETE /me/outlook -- the tokens are erased; mail already synchronised stays in the history.
func (s *server) outlookDisconnect(w http.ResponseWriter, r *http.Request) {
	m, p, err := s.myMailbox(r)
	if err != nil {
		mailErr(w, err)
		return
	}
	_, _ = s.db.Exec(r.Context(), `UPDATE public.outlook_connections SET status='DISCONNECTED', access_token_sealed=NULL, refresh_token_sealed=NULL,
		token_expires_at=NULL, updated_at=now() WHERE tenant=$1 AND user_sub=$2`, m.Tenant, p.Subject)
	s.logAudit(r.Context(), m.Tenant, "", "OUTLOOK_DISCONNECTED", map[string]any{"by": p.Username})
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// sweepOutlook synchronises every connected account; started from main, every outlookSyncInterval.
func (s *server) sweepOutlook(ctx context.Context) int {
	if !s.outlookOn() {
		return 0
	}
	rows, err := s.db.Query(ctx, `SELECT tenant, user_sub FROM public.outlook_connections WHERE status='ACTIVE'`)
	if err != nil {
		return 0
	}
	type conn struct{ tenant, sub string }
	conns := []conn{}
	for rows.Next() {
		var c conn
		if rows.Scan(&c.tenant, &c.sub) == nil {
			conns = append(conns, c)
		}
	}
	rows.Close()
	total := 0
	for _, c := range conns {
		// One replica syncs an account at a time. An advisory lock belongs to its connection, so the lock and the unlock
		// run on one held connection, never two from the pool.
		conn, err := s.db.Acquire(ctx)
		if err != nil {
			continue
		}
		var got bool
		if conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('outlook:'||$1||':'||$2))`, c.tenant, c.sub).Scan(&got) != nil || !got {
			conn.Release()
			continue
		}
		n, err := s.syncOutlook(ctx, c.tenant, c.sub)
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('outlook:'||$1||':'||$2))`, c.tenant, c.sub)
		conn.Release()
		if err != nil {
			slog.Warn("outlook sync failed", "tenant", c.tenant, "user", c.sub, "err", err)
		}
		total += n
	}
	return total
}
