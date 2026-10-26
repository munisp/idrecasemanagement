package main

import (
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMailboxLocalPart(t *testing.T) {
	for in, want := range map[string]string{
		"manual.case_manager": "manual.case_manager",
		"Jane Doe":            "jane.doe",
		"  JOHN@SMITH ":       "john.smith",
		"a..b":                "a.b",
		"...x...":             "x",
		"tenantadmin.fl":      "tenantadmin.fl",
		"ü":                   "",
	} {
		if got := mailboxLocalPart(in); got != want {
			t.Errorf("mailboxLocalPart(%q) = %q, want %q", in, got, want)
		}
	}
	if l := mailboxLocalPart(strings.Repeat("a", 90)); len(l) != 60 {
		t.Errorf("local part capped at 60, got %d", len(l))
	}
}

func TestRecipientsOnDomain(t *testing.T) {
	got := recipientsOnDomain("mail.upi.dev",
		[]string{"Jane <Jane.Doe@mail.upi.dev>", "someone@elsewhere.com"},
		[]string{"jane.doe@mail.upi.dev", "x@sub.mail.upi.dev"},
		[]string{"bob@mail.upi.dev"})
	if strings.Join(got, ",") != "jane.doe@mail.upi.dev,bob@mail.upi.dev" {
		t.Errorf("only the domain's own addresses, each once, lower case: %v", got)
	}
}

func TestSplitAddress(t *testing.T) {
	n, a := splitAddress(`"Jane Doe" <Jane@Example.com>`)
	if n != "Jane Doe" || a != "jane@example.com" {
		t.Errorf("got %q %q", n, a)
	}
	if _, a := splitAddress("bare@x.org"); a != "bare@x.org" {
		t.Errorf("bare address: %q", a)
	}
}

func TestHTMLToText(t *testing.T) {
	got := htmlToText(`<p>Hello&nbsp;<b>there</b></p><script>alert(1)</script><div>Line two &amp; more</div>`)
	if got != "Hello there\nLine two & more" {
		t.Errorf("got %q", got)
	}
}

func TestOutlookState(t *testing.T) {
	s := &server{cfg: Config{OutlookClientSecret: "secret"}}
	now := time.Now()
	st := s.outlookSignState(oauthState{tenant: "fl", sub: "sub-1", by: "sub-1"}, now.Add(time.Minute))
	if got, ok := s.outlookVerifyState(st, now); !ok || got.tenant != "fl" || got.sub != "sub-1" || got.mailbox != "" {
		t.Fatalf("a fresh state verifies: %+v", got)
	}
	if _, ok := s.outlookVerifyState(st, now.Add(2*time.Minute)); ok {
		t.Error("an expired state is refused")
	}
	other := &server{cfg: Config{OutlookClientSecret: "other"}}
	if _, ok := other.outlookVerifyState(st, now); ok {
		t.Error("a state signed with another secret is refused")
	}
	parts := strings.SplitN(st, ".", 2)
	forged := s.outlookSignState(oauthState{tenant: "fl", sub: companySub, by: "sub-1"}, now.Add(time.Minute))
	if _, ok := s.outlookVerifyState(strings.SplitN(forged, ".", 2)[0]+"."+parts[1], now); ok {
		t.Error("a payload with another's signature is refused")
	}
	co := s.outlookSignState(oauthState{tenant: "fl", sub: companySub, mailbox: "idre@upi.dev", by: "admin-1"}, now.Add(time.Minute))
	if got, ok := s.outlookVerifyState(co, now); !ok || got.sub != companySub || got.mailbox != "idre@upi.dev" || got.by != "admin-1" {
		t.Errorf("the company state carries the mailbox and who connected it: %+v", got)
	}
}

func TestOutlookScopes(t *testing.T) {
	if strings.Contains(outlookScopesFor(""), "Shared") {
		t.Error("an account's own mail asks for no shared-mailbox access (personal Microsoft accounts refuse it)")
	}
	if !strings.Contains(outlookScopesFor("idre@upi.dev"), "Mail.ReadWrite.Shared") {
		t.Error("a named shared mailbox needs the shared scopes")
	}
}

func TestOutlookFolderURL(t *testing.T) {
	since := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	u, _ := url.Parse(outlookFolderURL(graphAPI+"/me", "sentitems", since))
	q := u.Query()
	if !strings.HasSuffix(u.Path, "/me/mailFolders/sentitems/messages") || q.Get("$filter") != "sentDateTime gt 2026-10-01T08:30:00Z" || q.Get("$orderby") != "sentDateTime asc" {
		t.Errorf("sent items: %s", u)
	}
	u, _ = url.Parse(outlookFolderURL(graphAPI+"/users/idre@upi.dev", "inbox", since))
	if !strings.HasPrefix(u.Query().Get("$filter"), "receivedDateTime gt ") || !strings.HasSuffix(u.Path, "/users/idre@upi.dev/mailFolders/inbox/messages") {
		t.Errorf("a shared mailbox's inbox: %s", u)
	}
}

func TestResendKey(t *testing.T) {
	if k := (&server{cfg: Config{ResendAPIKey: "re_a", SMTPHost: "smtp.resend.com", SMTPPass: "re_b"}}).resendKey(); k != "re_a" {
		t.Errorf("RESEND_API_KEY first: %q", k)
	}
	if k := (&server{cfg: Config{SMTPHost: "smtp.resend.com", SMTPPass: "re_b"}}).resendKey(); k != "re_b" {
		t.Errorf("Resend's SMTP password is an API key: %q", k)
	}
	if k := (&server{cfg: Config{SMTPHost: "smtp.example.com", SMTPPass: "pw"}}).resendKey(); k != "" {
		t.Errorf("another SMTP host's password is not a Resend key: %q", k)
	}
}

func TestIsStaff(t *testing.T) {
	if !isStaff(principal{Subject: "s", Username: "u", Roles: []string{"CASE_MANAGER"}}) {
		t.Error("a Reviewer is staff")
	}
	for _, roles := range [][]string{{"TPA"}, {"HEALTH_PLAN"}, {"PARTY"}} {
		if isStaff(principal{Subject: "s", Username: "u", Roles: roles}) {
			t.Errorf("%v is not staff", roles)
		}
	}
	if isStaff(principal{Subject: "s", Roles: []string{"PM"}}) {
		t.Error("the worker's service principal (no username) is not staff")
	}
}

func TestCaseNumbersIn(t *testing.T) {
	got := caseNumbersIn("Re: fl26-019334 — see also FL26-019334 and TX26-00042, not FL2026-1 or XFL26-123456")
	if strings.Join(got, ",") != "FL26-019334,TX26-00042" {
		t.Errorf("distinct, upper case, whole words only: %v", got)
	}
	if len(caseNumbersIn("no dispute here, call 555-123456")) != 0 {
		t.Error("a phone number is not a dispute number")
	}
}

func TestMessageIDsIn(t *testing.T) {
	got := messageIDsIn("<a.1@upi.dev>  <b.2@x.org>\r\n\t<c@y>  junk")
	if strings.Join(got, " ") != "<a.1@upi.dev> <b.2@x.org> <c@y>" {
		t.Errorf("got %v", got)
	}
}

func TestTestAddressesAreNotSent(t *testing.T) {
	t.Setenv("EMAIL_TEST_DOMAINS", ".test")
	s := &server{cfg: Config{SMTPHost: "smtp.invalid", SMTPPort: 587}}
	if err := s.smtpDeliver("idre@upi.dev", []string{"a@idre.test"}, []string{"b@sopcheck.test"}, []byte("x")); err != nil {
		t.Errorf("mail only to test addresses is dropped before any connection: %v", err)
	}
	if err := s.smtpDeliver("idre@upi.dev", []string{"a@idre.test", "real@example.com"}, nil, []byte("x")); err == nil {
		t.Error("a real recipient is still sent to (and this host cannot be reached)")
	}
}

func TestOutlookDeclineResult(t *testing.T) {
	if outlookDeclineResult("access_denied", "AADSTS90094: The grant requires admin permission.") != "needs-admin" {
		t.Error("an organization that requires approval is told apart from a cancel")
	}
	if outlookDeclineResult("consent_required", "") != "needs-admin" {
		t.Error("consent_required needs an admin")
	}
	if outlookDeclineResult("access_denied", "AADSTS65004: User declined to consent to access the app.") != "declined" {
		t.Error("the person cancelling is a decline")
	}
}

func TestOutlookAdminConsentURL(t *testing.T) {
	if (&server{}).outlookAdminConsentURL() != "" {
		t.Error("no link while Outlook is off")
	}
	s := &server{cfg: Config{OutlookClientID: "app-1", OutlookClientSecret: "x", OutlookRedirectURL: "https://idre.example/api/oauth/outlook/callback"}}
	u, _ := url.Parse(s.outlookAdminConsentURL())
	if u.Host != "login.microsoftonline.com" || u.Path != "/organizations/adminconsent" || u.Query().Get("client_id") != "app-1" ||
		u.Query().Get("redirect_uri") != "https://idre.example/api/oauth/outlook/callback" {
		t.Errorf("approval link: %s", u)
	}
}

func TestSystemFromDefaultsToSMTPFrom(t *testing.T) {
	systemFromCache.Delete("zz")
	s := &server{cfg: Config{SMTPFrom: "IDRE <idre@upi.dev>"}}
	h, env := s.systemFrom("zz")
	if h != "IDRE <idre@upi.dev>" || env != "idre@upi.dev" {
		t.Errorf("no program address: the platform default, as header and bare envelope: %q %q", h, env)
	}
	if s.systemSendVia("zz") != "platform" {
		t.Error("the program's mail goes through the platform unless Outlook was chosen")
	}
}

func TestReplyText(t *testing.T) {
	body := "Yes, please proceed with the full review.\n\nThanks,\nPat\n\nOn Mon, Oct 26, 2026 at 9:00 AM Florida Claims Disputes <cases@mail.upi.dev> wrote:\n> We need your permission to proceed. Do you wish to proceed?\n> Reply yes or no."
	if got := replyText(body); got != "Yes, please proceed with the full review.\n\nThanks,\nPat" {
		t.Errorf("only what the sender wrote: %q", got)
	}
	if got := replyText("We agree.\n-----Original Message-----\nFrom: x\nDo you agree?"); got != "We agree." {
		t.Errorf("an Outlook reply's original is cut: %q", got)
	}
}

func TestReadDecision(t *testing.T) {
	for _, c := range []struct {
		kind, text, want string
		clear            bool
	}{
		{"PERMISSION", "Yes, please proceed with the full review.", "PROCEED", true},
		{"PERMISSION", "We give you permission to go ahead.", "PROCEED", true},
		{"PERMISSION", "Approved.", "PROCEED", true},
		{"PERMISSION", "We do not wish to proceed.", "DECLINE", true},
		{"PERMISSION", "Please don't proceed, we decline.", "DECLINE", true},
		{"PERMISSION", "We no longer want to continue.", "DECLINE", true},
		{"PERMISSION", "Should we proceed?", "", false},
		{"PERMISSION", "We approve, but we decline the estimate.", "", false},
		{"PERMISSION", "Can you confirm the deadline", "", false},
		{"PERMISSION", "Thanks, received.", "", false},
		{"EXTENSION", "That works for us, we agree to the extra time.", "AGREE", true},
		{"EXTENSION", "We do not agree to more time.", "DECLINE", true},
		{"EXTENSION", "We object.", "DECLINE", true},
	} {
		got, clear := readDecision(c.kind, c.text)
		if got != c.want || clear != c.clear {
			t.Errorf("%s %q = %q, %v; want %q, %v", c.kind, c.text, got, clear, c.want, c.clear)
		}
	}
}

func TestMailAuth(t *testing.T) {
	h := authFromHeader("spf=pass (sender IP is 1.2.3.4) smtp.mailfrom=tpa.example; dkim=pass (signature was verified) header.d=tpa.example;dmarc=pass action=none")
	if !mailPassedChecks(h) {
		t.Errorf("an Outlook Authentication-Results header that passes: %v", h)
	}
	if mailPassedChecks(authFromHeader("spf=fail; dkim=none; dmarc=fail")) {
		t.Error("a failing sender is not trusted")
	}
	if !mailPassedChecks(authResults([]byte(`{"spf":"pass","dkim":"pass","dmarc":"none"}`))) {
		t.Error("SPF and DKIM passing is enough")
	}
	if mailPassedChecks(authResults([]byte(`{}`))) {
		t.Error("no checks recorded: not trusted")
	}
}

func TestWriteMixedParses(t *testing.T) {
	var b strings.Builder
	writeMailHeaders(&b, "Pat <pat@mail-dev.upi.dev>", []string{"a@b.test"}, nil, "Records", "<x@y>")
	b.WriteString("MIME-Version: 1.0\r\n")
	writeMixed(&b, "See attached.", []outAttachment{{Name: "eob.pdf", ContentType: "application/pdf", Content: []byte("%PDF-1.4 fake")}, {Name: "notes.txt", Content: []byte("hello")}})
	msg, err := mail.ReadMessage(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	names := []string{}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if fn := part.FileName(); fn != "" {
			raw, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
			names = append(names, fn+":"+string(raw))
		}
	}
	if strings.Join(names, ",") != "eob.pdf:%PDF-1.4 fake,notes.txt:hello" {
		t.Errorf("both files, intact: %v", names)
	}
}
