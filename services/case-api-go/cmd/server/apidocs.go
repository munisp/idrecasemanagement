package main

//go:generate go run ../apidocgen .

// API reference (OpenAPI 3.1), built from the live router so it can't drift
// from what the server really serves.
//
//   GET /api/public/docs/tpa.json          TPAs (public)
//   GET /api/public/docs/health-plan.json  health plans (public)
//   GET /v1/tenants/{tenant}/api-docs/internal.json  every endpoint (office staff only)
//
// Each operation's text comes from, in order: the hand-written entry in
// apidocs_curated.go, the comment on the line that registers the route, a
// title made from the path. The internal reference also shows the handler's
// own doc comment (apidocs_gen.go, from `go generate`).

import (
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
)

type apiRoute struct {
	Method, Pattern, Handler string
}

// collectAPIRoutes walks the finished router once at start-up.
func collectAPIRoutes(r chi.Routes) []apiRoute {
	var out []apiRoute
	_ = chi.Walk(r, func(method, route string, h http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasSuffix(route, "/*") || method == "HEAD" || method == "OPTIONS" || method == "CONNECT" || method == "TRACE" {
			return nil
		}
		out = append(out, apiRoute{Method: method, Pattern: strings.TrimSuffix(route, "/"), Handler: handlerFuncName(h)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pattern != out[j].Pattern {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].Method < out[j].Method
	})
	return out
}

var funcSuffix = regexp.MustCompile(`(-fm|\.func\d+(\.\d+)*)+$`)

// handlerFuncName: "main.(*server).tpaMe-fm" -> "tpaMe"; closures returned by a
// method ("main.(*server).orgAPIKeysList.func1") -> "orgAPIKeysList".
func handlerFuncName(h http.Handler) string {
	if ch, ok := h.(*chi.ChainHandler); ok {
		h = ch.Endpoint
	}
	v := reflect.ValueOf(h)
	if v.Kind() != reflect.Func {
		return ""
	}
	fn := runtime.FuncForPC(v.Pointer())
	if fn == nil {
		return ""
	}
	name := funcSuffix.ReplaceAllString(fn.Name(), "")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

var chiRegexParam = regexp.MustCompile(`\{([A-Za-z0-9_]+):[^}]*\}`)
var pathParam = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

const tenantPrefix = "/v1/tenants/{tenant}"

// What an outside organization may call besides its own /tpa or /hp area
// (accesspolicy.go: outsideShellPaths + tpaOwnCasePaths), worth documenting.
var partnerSharedPaths = regexp.MustCompile(`^/(cases/\{caseId\}(|/clocks|/documents|/documents/\{docId\}/download)|notifications(/.*)?|upload-policy|uploads(/.*)?)$`)

type apiAudience struct {
	key, title, area string // area: "/tpa/" | "/hp/" | "" (internal: everything)
}

var (
	audTPA      = apiAudience{"tpa", "API for TPAs", "/tpa/"}
	audPlan     = apiAudience{"health-plan", "API for health plans", "/hp/"}
	audInternal = apiAudience{"internal", "API — internal reference", ""}
)

// GET /api/public/docs/{audience}.json
func (s *server) publicAPIDoc(w http.ResponseWriter, r *http.Request) {
	switch chi.URLParam(r, "audience") {
	case "tpa.json":
		writeJSON(w, http.StatusOK, s.buildAPISpec(r, audTPA))
	case "health-plan.json":
		writeJSON(w, http.StatusOK, s.buildAPISpec(r, audPlan))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such API document; try tpa.json or health-plan.json"})
	}
}

// GET /v1/tenants/{tenant}/api-docs/internal.json -- office staff only.
func (s *server) internalAPIDoc(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if isExternalOnly(p) || isAPIKeyPrincipal(p) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "the internal reference is for office staff"})
		return
	}
	writeJSON(w, http.StatusOK, s.buildAPISpec(r, audInternal))
}

func (s *server) buildAPISpec(r *http.Request, aud apiAudience) map[string]any {
	paths := map[string]map[string]any{}
	tagSet := map[string]bool{}
	for _, rt := range s.apiRoutes {
		path := chiRegexParam.ReplaceAllString(rt.Pattern, "{$1}")
		rel, inTenant := strings.CutPrefix(path, tenantPrefix)
		if aud.area != "" {
			if !inTenant || !(strings.HasPrefix(rel, aud.area) || partnerSharedPaths.MatchString(rel)) {
				continue
			}
			if rt.Pattern == tenantPrefix+aud.area+"claim" || rt.Pattern == tenantPrefix+aud.area+"unlink" {
				continue // linking a person's login to an organization is a portal step, not an integration one
			}
		} else if path == "/healthz" {
			continue
		}
		op := s.apiOperation(rt, path, rel, inTenant, aud)
		tagSet[op["tags"].([]string)[0]] = true
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		paths[path][strings.ToLower(rt.Method)] = op
	}
	tags := make([]map[string]any, 0, len(tagSet))
	for _, t := range apiTagOrder(tagSet) {
		tag := map[string]any{"name": t}
		if d := apiTagDescriptions[t]; d != "" {
			tag["description"] = d
		}
		tags = append(tags, tag)
	}

	base := "https://" + requestHost(r)
	security := []map[string][]string{{"bearer": {}}}
	schemes := map[string]any{
		"bearer": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "JWT",
			"description": "An access token from the sign-in service, sent as `Authorization: Bearer <token>`."},
	}
	if aud.area != "" {
		schemes["apiKey"] = map[string]any{
			"type": "oauth2", "description": "Your organization's API key (client id + secret), exchanged for a 5-minute token.",
			"flows": map[string]any{"clientCredentials": map[string]any{"tokenUrl": s.apiTokenURL(), "scopes": map[string]string{}}},
		}
		security = []map[string][]string{{"apiKey": {}}, {"bearer": {}}}
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title": aud.title, "version": "1.0",
			"description": apiIntro(aud, base, s.apiTokenURL()),
			"contact":     map[string]string{"name": "idre support", "url": base},
		},
		"servers":  []map[string]string{{"url": base, "description": "this environment"}},
		"security": security,
		"tags":     tags,
		"paths":    paths,
		"components": map[string]any{
			"securitySchemes": schemes,
			"schemas": map[string]any{
				"Error": map[string]any{"type": "object", "properties": map[string]any{"error": map[string]string{"type": "string", "description": "What went wrong, in words you can show a person."}}},
			},
			"responses": map[string]any{
				"BadRequest":   errResp("The request is missing something or a value isn't valid."),
				"Unauthorized": errResp("No token, an expired token, or a revoked API key."),
				"Forbidden":    errResp("Your organization or role can't do this."),
				"NotFound":     errResp("Not found, or not yours (the two look the same on purpose)."),
			},
		},
	}
}

func errResp(desc string) map[string]any {
	return map[string]any{"description": desc, "content": map[string]any{"application/json": map[string]any{"schema": map[string]string{"$ref": "#/components/schemas/Error"}}}}
}

func (s *server) apiOperation(rt apiRoute, path, rel string, inTenant bool, aud apiAudience) map[string]any {
	cur, hasCur := apiCurated[rt.Method+" "+strings.TrimPrefix(rt.Pattern, tenantPrefix)]
	summary := cur.Summary
	if summary == "" {
		summary = sentenceCase(genRouteNotes[rt.Handler])
	}
	if summary == "" || (aud.area == "" && len(summary) > 120) {
		summary = titleFromPath(rt.Method, rel)
	}
	desc := cur.Description
	if aud.area == "" { // staff see the engineering notes too
		if d := genHandlerDocs[rt.Handler]; d != "" {
			if desc != "" {
				desc += "\n\n"
			}
			desc += "**Implementation notes** (`" + rt.Handler + "`):\n\n" + d
		}
	}
	op := map[string]any{
		"summary":     summary,
		"operationId": operationID(rt),
		"tags":        []string{apiTag(rel, inTenant, path)},
		"responses":   map[string]any{},
	}
	if desc != "" {
		op["description"] = desc
	}
	params := []map[string]any{}
	for _, m := range pathParam.FindAllStringSubmatch(path, -1) {
		params = append(params, pathParamDoc(m[1]))
	}
	for _, q := range cur.Query {
		params = append(params, map[string]any{"name": q.Name, "in": "query", "required": q.Required, "description": q.Description, "schema": map[string]string{"type": docOr(q.Type, "string")}})
	}
	if len(params) > 0 {
		op["parameters"] = params
	}
	if cur.Body != nil {
		op["requestBody"] = cur.Body.openAPI()
	}
	ok := map[string]any{"description": docOr(cur.ResponseDescription, "Success.")}
	if cur.Response != nil {
		ok["content"] = map[string]any{"application/json": map[string]any{"example": cur.Response}}
	} else if cur.ResponseType != "" {
		ok["content"] = map[string]any{cur.ResponseType: map[string]any{"schema": map[string]string{"type": "string", "format": "binary"}}}
	}
	status := "200"
	if cur.Status != "" {
		status = cur.Status
	}
	resps := op["responses"].(map[string]any)
	resps[status] = ok
	if rt.Method != http.MethodGet {
		resps["400"] = map[string]string{"$ref": "#/components/responses/BadRequest"}
	}
	resps["401"] = map[string]string{"$ref": "#/components/responses/Unauthorized"}
	resps["403"] = map[string]string{"$ref": "#/components/responses/Forbidden"}
	if strings.Contains(path, "{") && path != tenantPrefix {
		resps["404"] = map[string]string{"$ref": "#/components/responses/NotFound"}
	}
	if !hasCur && aud.area != "" {
		op["x-documented"] = false // listed from the live router; no hand-written guide yet
	}
	return op
}

func pathParamDoc(name string) map[string]any {
	d := map[string]string{
		"tenant":    "Your state program, lower case, e.g. `fl` for Florida.",
		"caseId":    "A dispute (case) id, as returned when it was filed.",
		"clientId":  "One of your clients (provider) ids, from `GET …/tpa/clients`.",
		"partyId":   "One of your parties' ids, from `GET …/hp/onboarding`.",
		"packageId": "A document package id, from creating the package.",
		"docId":     "A document id.",
		"keyId":     "An API key id, from the list of keys.",
		"groupId":   "A payment (checkout) id.",
		"batchId":   "A bulk filing id.",
		"reqId":     "A document request id.",
	}[name]
	p := map[string]any{"name": name, "in": "path", "required": true, "schema": map[string]string{"type": "string"}}
	if d != "" {
		p["description"] = d
	}
	if name == "tenant" {
		p["example"] = "fl"
	}
	return p
}

func operationID(rt apiRoute) string {
	if rt.Handler != "" {
		return rt.Handler + "_" + strings.ToLower(rt.Method)
	}
	return strings.ToLower(rt.Method) + strings.NewReplacer("/", "_", "{", "", "}", "").Replace(rt.Pattern)
}

// apiTag groups an operation: for partners by subject, for staff by area.
func apiTag(rel string, inTenant bool, path string) string {
	if !inTenant {
		switch {
		case strings.HasPrefix(path, "/api/public/"):
			return "Public (no sign-in)"
		case strings.HasPrefix(path, "/api/webhooks/"), strings.HasPrefix(path, "/api/email/"), strings.HasPrefix(path, "/api/oauth/"):
			return "Webhooks and callbacks"
		case strings.HasPrefix(path, "/v1/admin"):
			return "Platform admin"
		case strings.HasPrefix(path, "/api/voice"):
			return "Voice"
		}
		return "Other"
	}
	segs := strings.Split(strings.Trim(rel, "/"), "/")
	first := segs[0]
	if (first == "tpa" || first == "hp") && len(segs) > 1 {
		if t := partnerTags[segs[1]]; t != "" {
			return t
		}
		return "Your organization"
	}
	if t := partnerTags[first]; t != "" && (first == "cases" || first == "notifications" || first == "uploads" || first == "upload-policy") {
		return t
	}
	if t := staffTags[first]; t != "" {
		return t
	}
	return sentenceCase(strings.ReplaceAll(first, "-", " "))
}

var partnerTags = map[string]string{
	"me": "Your organization", "team": "Your organization", "audit": "Your organization", "authorized-senders": "Your organization",
	"invite-code": "Your organization", "api-keys": "API keys", "dashboard": "Disputes", "disputes": "Disputes", "intake": "Disputes",
	"prefill": "Disputes", "responding": "Disputes against you", "clients": "Clients", "groups": "Clients", "parties": "Parties",
	"onboarding": "Onboarding packages", "packages": "Onboarding packages", "fees": "Fees and payments", "finance": "Fees and payments",
	"payment-methods": "Fees and payments", "document-requests": "Document requests", "sent-documents": "Letters and documents",
	"documents.zip": "Letters and documents", "tasks": "Tasks", "cases": "Your disputes (read)", "notifications": "Notifications",
	"uploads": "Large uploads", "upload-policy": "Large uploads",
}

var staffTags = map[string]string{
	"cases": "Cases", "qa": "QA gate", "invoices": "Billing", "billing": "Billing", "refunds": "Billing", "ledger": "Ledger",
	"admin": "Tenant admin", "reports": "Reports", "crm": "CRM", "mail": "Mail", "mailboxes": "Mail", "letters": "Letters",
	"copilot": "Assistants", "graph": "Graph", "search": "Search", "telemetry": "Telemetry", "voice": "Voice",
	"api-keys": "API keys", "api-docs": "API reference",
}

var apiTagDescriptions = map[string]string{
	"API keys":              "Keys your software uses to call this API. Made and revoked by your organization's admins in the portal or here.",
	"Your organization":     "Who you are on idre, your team, and your organization's activity log.",
	"Clients":               "The providers you file for. A client must have a profile and a payment method before you can file for it.",
	"Parties":               "The parties your plan files for.",
	"Onboarding packages":   "The six-document package each client or party needs before filing. Upload slot by slot, or one merged PDF.",
	"Disputes":              "Filing disputes, one at a time or in bulk, and following them.",
	"Disputes against you":  "Disputes another side filed against your plan, once the office finds them eligible: read, upload your response, submit or opt out.",
	"Fees and payments":     "Fees owed per dispute and paying them, one, per client, per bulk filing or all at once.",
	"Document requests":     "Documents the office asked you for, and sending them.",
	"Letters and documents": "Letters and documents the office sent you.",
	"Your disputes (read)":  "The record, deadlines and documents of disputes you filed.",
	"Notifications":         "Your organization's alerts.",
	"Large uploads":         "Resumable uploads for big files.",
}

func apiTagOrder(set map[string]bool) []string {
	order := []string{"API keys", "Your organization", "Clients", "Parties", "Onboarding packages", "Disputes", "Disputes against you",
		"Your disputes (read)", "Fees and payments", "Document requests", "Letters and documents", "Tasks", "Notifications", "Large uploads"}
	var out []string
	for _, t := range order {
		if set[t] {
			out = append(out, t)
			delete(set, t)
		}
	}
	rest := make([]string, 0, len(set))
	for t := range set {
		rest = append(rest, t)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func titleFromPath(method, rel string) string {
	var words []string
	for _, seg := range strings.Split(strings.Trim(rel, "/"), "/") {
		if seg == "" || strings.HasPrefix(seg, "{") || seg == "tpa" || seg == "hp" {
			continue
		}
		words = append(words, strings.ReplaceAll(seg, "-", " "))
	}
	verb := map[string]string{"GET": "Get", "POST": "Create or run", "PUT": "Replace", "PATCH": "Update", "DELETE": "Delete"}[method]
	return verb + " " + strings.Join(words, " › ")
}

func sentenceCase(s string) string {
	s = strings.TrimSpace(strings.TrimRight(s, "."))
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func docOr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func requestHost(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		return strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return r.Host
}

// apiIntro is the guide at the top of each reference.
func apiIntro(aud apiAudience, base, tokenURL string) string {
	if aud.area == "" {
		return "Every endpoint case-api serves, for idre office staff and engineers. Generated from the live router; " +
			"each entry shows the handler and its notes. Partners have their own references at `/developers.html`.\n\n" +
			"Calls under `/v1/tenants/{tenant}` need a sign-in token whose `groups` claim includes `/tenant/{tenant}` " +
			"(or a federal / platform admin role); roles come from `realm_access.roles`."
	}
	area := "/tpa"
	who := "TPA"
	first := "1. `GET " + base + "/v1/tenants/fl/tpa/me` — check the key works and see your organization.\n" +
		"2. `GET …/tpa/clients` — the providers you file for, and which are ready to file.\n" +
		"3. File a dispute: `POST …/tpa/intake`, or many at once with `POST …/tpa/disputes/bulk`.\n" +
		"4. Pay its fee: `POST …/tpa/fees/checkout` returns a payment page.\n" +
		"5. Follow it: `GET …/tpa/dashboard`, `GET …/tpa/disputes/{caseId}/summary`, and answer `GET …/tpa/document-requests`.\n"
	if aud.area == "/hp/" {
		area, who = "/hp", "health plan"
		first = "1. `GET " + base + "/v1/tenants/fl/hp/me` — check the key works and see your plan.\n" +
			"2. `GET …/hp/responding` — disputes filed against your plan; respond with `POST …/hp/responding/{caseId}/documents` then `…/submit`.\n" +
			"3. File your own: `POST …/hp/intake`.\n" +
			"4. Answer the office: `GET …/hp/document-requests`.\n"
	}
	return "Connect your " + who + "'s own systems to the dispute resolution portal. Everything you can do in the portal's " + who + " area you can do here, " +
		"with the same rules.\n\n" +
		"## 1. Get an API key\n\n" +
		"In the portal, open **API keys** in the menu → **New key** (" + map[string]string{"/tpa": "TPA admins only", "/hp": "any of your plan's logins"}[area] + "). " +
		"You get a **client id** and a **client secret**. The secret is shown once: store it like a password. " +
		"Keys can be limited and revoked at any time; each key's actions appear in your activity log under its name.\n\n" +
		"## 2. Get a token\n\n" +
		"Exchange the key for an access token (valid 5 minutes; get a new one when it expires):\n\n" +
		"```\ncurl -s " + tokenURL + " \\\n  -d grant_type=client_credentials \\\n  -d client_id=$IDRE_CLIENT_ID \\\n  -d client_secret=$IDRE_CLIENT_SECRET\n```\n\n" +
		"The answer's `access_token` goes in every call: `Authorization: Bearer <access_token>`.\n\n" +
		"## 3. Call the API\n\n" +
		"Every path starts with `/v1/tenants/{tenant}`, where `{tenant}` is your state program (e.g. `fl`).\n\n" + first + "\n" +
		"## Environments\n\n" +
		"| | Base URL | Token URL |\n|---|---|---|\n" +
		"| Test | https://idre.newfire.app | https://keycloak-servers.newfire.app/realms/idre/protocol/openid-connect/token |\n" +
		"| Production | https://idre-prod.newfire.app | https://keycloak-servers.newfire.app/realms/idre-prod/protocol/openid-connect/token |\n\n" +
		"Keys belong to one environment: make a test key on the test site.\n\n" +
		"## Conventions\n\n" +
		"- Requests and answers are JSON unless an endpoint says it takes a file (`multipart/form-data`).\n" +
		"- Errors are `{\"error\": \"…\"}` with a plain-language message, and the usual status codes.\n" +
		"- `404` also means \"not yours\": you never learn whether another organization's record exists.\n" +
		"- Endpoints marked `x-documented: false` are listed from the live service and work today, but have no worked example yet.\n"
}
