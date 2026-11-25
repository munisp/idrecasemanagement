package main

// Hand-written entries for the API reference (apidocs.go), keyed by
// "METHOD <route pattern without /v1/tenants/{tenant}>". Field names and
// shapes are taken from the handlers' own input structs and answers; when a
// handler changes, change its entry here too (apidocs_test.go checks every
// entry still names a real route).

type apiDoc struct {
	Summary, Description string
	Query                []apiParam
	Body                 *apiBody
	Status               string // success status, default "200"
	Response             any    // example JSON answer
	ResponseType         string // non-JSON answer, e.g. "application/zip"
	ResponseDescription  string
}

type apiParam struct {
	Name, Type, Description string
	Required                bool
}

type apiBody struct {
	Multipart  bool
	Properties map[string]any // JSON-schema properties
	Required   []string
	Example    any
}

func (b *apiBody) openAPI() map[string]any {
	schema := map[string]any{"type": "object", "properties": b.Properties}
	if len(b.Required) > 0 {
		schema["required"] = b.Required
	}
	media := map[string]any{"schema": schema}
	if b.Example != nil {
		media["example"] = b.Example
	}
	ct := "application/json"
	if b.Multipart {
		ct = "multipart/form-data"
	}
	return map[string]any{"required": true, "content": map[string]any{ct: media}}
}

func docStr(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func docInt(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func docFile(desc string) map[string]any {
	return map[string]any{"type": "string", "format": "binary", "description": desc}
}
func docEnum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": desc}
}

var (
	slotNames = []string{"EOB", "ITEMIZED_BILL", "PROOF_OF_COVERAGE", "OPEN_NEGOTIATION_NOTICE", "OFFER_SUBSTANTIATION", "IDRE_ATTESTATION"}

	packetSchema = map[string]any{
		"type":        "object",
		"description": "The filing packet form (optional but recommended): the claims in dispute. Problems never block the filing; they go to the reviewer.",
		"properties": map[string]any{
			"details":               map[string]any{"type": "object", "description": "Free-form packet answers, as the portal's filing form names them."},
			"disputed_amount_cents": docInt("Total disputed, in cents."),
			"claims": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"claim_number": docStr(""), "patient_first_name": docStr(""), "patient_last_name": docStr(""), "type_of_service": docStr(""),
				"date_of_service": docStr("YYYY-MM-DD"), "billed_cents": docInt(""), "paid_cents": docInt(""), "disputed_cents": docInt(""),
				"date_claim_submitted": docStr("YYYY-MM-DD"), "date_of_denial": docStr("YYYY-MM-DD"), "date_of_final_determination": docStr("YYYY-MM-DD"),
			}}},
		},
	}

	checkoutBody = &apiBody{
		Properties: map[string]any{
			"source":            docEnum("How to pay. CHECKOUT (default) returns a payment page; TPA and CLIENT charge saved methods.", "CHECKOUT", "TPA", "CLIENT"),
			"method":            docEnum("CHECKOUT only: card (default, adds the card fee) or bank.", "card", "bank"),
			"payment_method_id": docStr("TPA: which of your saved methods to charge."),
			"client_methods":    map[string]any{"type": "object", "additionalProperties": map[string]string{"type": "string"}, "description": "CLIENT: client_id → saved method id (default: its newest chargeable one)."},
			"case_ids":          map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "Pay only these disputes (default: every unpaid one in scope)."},
		},
		Example: map[string]any{"source": "CHECKOUT", "method": "card"},
	}
	checkoutAnswer = map[string]any{"checkout_url": "https://checkout.stripe.com/c/pay/cs_test_…", "disputes": 3, "amount_cents": 105000, "card_fee_cents": 3075, "payment_group_id": "6f3c…"}

	keyCreated = map[string]any{
		"id": "0b8e…", "label": "Claims system", "client_id": "api-key-claims-system-3fa91c",
		"client_secret": "shown-once-store-it-safely", "token_url": "https://keycloak-servers.newfire.app/realms/idre/protocol/openid-connect/token",
		"api_base": "/v1/tenants/fl/tpa", "team_role": "STAFF", "limited_to_client": "", "created_at": "2026-11-04T10:00:00Z",
	}
	keyList = map[string]any{"api_keys": []map[string]any{{
		"id": "0b8e…", "label": "Claims system", "client_id": "api-key-claims-system-3fa91c", "team_role": "STAFF",
		"limited_to_client": "", "created_by": "Ada Obi", "created_at": "2026-11-04T10:00:00Z", "last_used_at": "2026-11-04T10:05:12Z", "revoked_at": nil,
	}}, "token_url": "https://keycloak-servers.newfire.app/realms/idre/protocol/openid-connect/token"}

	docRequestsAnswer = map[string]any{"open": 1, "requests": []map[string]any{{"id": "9d1…", "case_id": "79f…", "case_number": "FL26-001203", "side": "PROVIDER",
		"reason_label": "Missing document", "message": "Please send the itemized bill for claim 4471.", "due_date": "2026-11-11", "overdue": false,
		"status": "OPEN", "files": []any{}}}}
	summaryAnswer = map[string]any{"case": map[string]any{"case_number": "FL26-001203", "status": "OPEN"}, "side": "INITIATING", "dates": map[string]any{}, "claims": []any{}, "fees": map[string]any{}}
)

func apiKeyDocs(area, who string) map[string]apiDoc {
	role := ""
	body := &apiBody{
		Properties: map[string]any{"label": docStr("A name you'll recognize, e.g. the system that uses it.")},
		Required:   []string{"label"},
		Example:    map[string]any{"label": "Claims system"},
	}
	if area == "/tpa" {
		role = " Keys are STAFF (default) or FINANCE, never admins, and can be limited to one client like an employee login."
		body.Properties["team_role"] = docEnum("What the key may do: STAFF (file and follow disputes) or FINANCE (payments only).", "STAFF", "FINANCE")
		body.Properties["client_id"] = docStr("Limit the key to one client (provider). Leave out for the whole TPA.")
		body.Example = map[string]any{"label": "Claims system", "team_role": "STAFF"}
	}
	created := keyCreated
	if area == "/hp" {
		created = map[string]any{}
		for k, v := range keyCreated {
			created[k] = v
		}
		created["api_base"], created["team_role"] = "/v1/tenants/fl/hp", ""
	}
	return map[string]apiDoc{
		"GET " + area + "/api-keys": {Summary: "List your API keys",
			Description: "Your " + who + "'s keys, active first, with when each was last used. Secrets are never shown again.", Response: keyList},
		"POST " + area + "/api-keys": {Summary: "Create an API key",
			Description: "Makes a key for your software. The answer holds the `client_secret` **once**: store it now. " +
				"Only signed-in people can make keys (a key can't make keys)." + role + " Up to 20 active keys per organization.",
			Body: body, Status: "201", Response: created},
		"DELETE " + area + "/api-keys/{keyId}": {Summary: "Revoke an API key",
			Description: "The key stops working at once: no new tokens, and any token already issued with it is refused.",
			Response:    map[string]any{"revoked": true}},
	}
}

var apiCurated = func() map[string]apiDoc {
	m := map[string]apiDoc{
		// ── TPA: organization ───────────────────────────────────────────
		"GET /tpa/me": {Summary: "Your TPA", Description: "Your organization and your login's standing in it. A good first call to check a key works.",
			Response: map[string]any{"tpa": map[string]any{"id": "c1a…", "name": "Sunrise Billing Partners", "contact_name": "Ada Obi", "contact_email": "ada@sunrise.example",
				"status": "ACTIVE", "invite_code": nil, "team_role": "STAFF", "client_scope": "", "client_scope_name": ""}}},
		"GET /tpa/team": {Summary: "Your team", Description: "The people with logins at your TPA (API keys are listed separately)."},

		// ── TPA: clients ────────────────────────────────────────────────
		"GET /tpa/clients": {Summary: "List your clients", Description: "The providers you file for, with each one's onboarding status. Filing needs the client's profile (an NPI that isn't blocked); some state programs also require a payment method and a document set for every dispute.",
			Response: map[string]any{"clients": []map[string]any{{"id": "5e2…", "party_name": "Coral Gables Imaging", "party_type": "PROVIDER",
				"contact_email": "billing@coralimaging.example", "status": "ACTIVE", "case_count": 4}}}},
		"POST /tpa/clients": {Summary: "Add a client", Description: "Adds a provider you file for. Next: set its profile and add a payment method; then start a document set for each dispute.",
			Body: &apiBody{Properties: map[string]any{
				"party_name":    docStr("The provider's legal name."),
				"party_type":    docEnum("PROVIDER (default) or HEALTH_PLAN.", "PROVIDER", "HEALTH_PLAN"),
				"contact_email": docStr("The provider's contact email."),
				"group_id":      docStr("Optional: the provider group it belongs to (from GET …/tpa/groups)."),
			}, Required: []string{"party_name"}, Example: map[string]any{"party_name": "Coral Gables Imaging", "party_type": "PROVIDER", "contact_email": "billing@coralimaging.example"}},
			Status: "201", Response: map[string]any{"client_id": "5e2…", "party_type": "PROVIDER", "status": "ACTIVE"}},
		"PUT /tpa/clients/{clientId}/profile": {Summary: "Set a client's profile", Description: "NPI, TIN and address. The NPI is checked against the national registry.",
			Body:     &apiBody{Properties: map[string]any{"npi": docStr("10-digit NPI."), "tin": docStr("Tax id."), "address": docStr("Practice address.")}, Example: map[string]any{"npi": "1234567893", "tin": "59-1234567", "address": "100 Main St, Miami, FL"}},
			Response: map[string]any{"client_id": "5e2…", "onboarding_status": "PROFILE_DONE", "npi_status": "VERIFIED", "npi_registry_name": "CORAL GABLES IMAGING LLC"}},
		"POST /tpa/clients/{clientId}/payments": {Summary: "Add a client's payment method", Description: "Records the client's payment method on file (not charged when you file; you pay fees yourself).",
			Body: &apiBody{Properties: map[string]any{"method": docStr("e.g. card or ach."), "label": docStr(""), "brand": docStr(""), "last4": docStr(""), "routing": docStr(""),
				"processor_ref": docStr("Your processor's reference for it."), "auth_received": map[string]any{"type": "boolean", "description": "The client authorized it."}}},
			Status: "201", Response: map[string]any{"payment_method_id": "7aa…", "verified": true, "onboarding_status": "PAYMENT_DONE"}},

		// ── TPA: document packages ──────────────────────────────────────
		"POST /tpa/clients/{clientId}/packages": {Summary: "Start a document package",
			Description: "A dispute needs six documents: " + "`EOB`, `ITEMIZED_BILL`, `PROOF_OF_COVERAGE`, `OPEN_NEGOTIATION_NOTICE`, `OFFER_SUBSTANTIATION`, `IDRE_ATTESTATION`. " +
				"Start a set for each dispute, then upload each slot (`DISCRETE`) or one PDF that is split for you (`MERGED`).",
			Body:   &apiBody{Properties: map[string]any{"mode": docEnum("DISCRETE (upload one by one, the default) or MERGED (one PDF).", "DISCRETE", "MERGED")}, Example: map[string]any{"mode": "DISCRETE"}},
			Status: "201", Response: map[string]any{"package_id": "a41…", "mode": "DISCRETE", "status": "ASSEMBLING", "slots": slotNames}},
		"POST /tpa/packages/{packageId}/docs": {Summary: "Upload one document to a package",
			Description: "Upload a file into one slot. Each document is checked automatically; poll the package for the verdict. " +
				"Extra documents go in `MORE_<KIND>` slots (e.g. `MORE_MEDICAL_RECORDS`).",
			Body:   &apiBody{Multipart: true, Properties: map[string]any{"slot": docEnum("Which document this is.", slotNames...), "file": docFile("PDF or image.")}, Required: []string{"slot", "file"}},
			Status: "202", Response: map[string]any{"package_doc_id": "e03…", "slot": "EOB", "status": "PENDING", "note": "doc-intel validates the document; poll the package for the verdict"}},
		"POST /tpa/packages/{packageId}/merged": {Summary: "Upload all six documents as one PDF",
			Body:   &apiBody{Multipart: true, Properties: map[string]any{"file": docFile("One PDF with all six documents.")}, Required: []string{"file"}},
			Status: "202", Response: map[string]any{"package_id": "a41…", "status": "VALIDATING", "note": "doc-intel is splitting the PDF into the six required slots; poll the package for per-slot verdicts"}},
		"GET /tpa/packages/{packageId}": {Summary: "Check a package", Description: "Per-slot verdicts: `ACCEPTED`, `NEEDS_REVIEW`, `REJECTED`, `MISSING` (no file), or `PENDING`/`ANALYZING` while a file is read. " +
			"You can file with the set once no file is still being read; a verdict is a note for the reviewer, never a block.",
			Response: map[string]any{"package_id": "a41…", "client_id": "5e2…", "mode": "DISCRETE", "status": "READY", "slots": []map[string]any{{"slot": "EOB", "status": "ACCEPTED"}}}},

		// ── TPA: filing ─────────────────────────────────────────────────
		"POST /tpa/intake": {Summary: "File a dispute",
			Description: "Files one dispute for one of your clients (its NPI must not be blocked). With `package_id`, every file in the set must have been read first; some state programs require a set and a verified payment method. " +
				"The answer says whether a fee is due (`payment_required`); pay it with the fees endpoints.",
			Body: &apiBody{Properties: map[string]any{
				"client_id":             docStr("Your client (provider) filing the dispute."),
				"package_id":            docStr("This dispute's document set (optional unless your state program requires it); its files must have been read."),
				"disputed_amount_cents": docInt("Amount in dispute, in cents."),
				"other_party_name":      docStr("The health plan the dispute is against."),
				"other_party_email":     docStr("Where the other side's notices go."),
				"other_party_naic":      docStr("The plan's NAIC code, if known."),
				"contact_name":          docStr("The client's contact for this dispute."),
				"provider_type":         docStr("Your answer if the documents don't say: e.g. hospital, physician_group. Leave empty if unsure."),
				"in_network":            docEnum("Your answer if the documents don't say.", "yes", "no", ""),
				"notes":                 docStr("Anything the reviewer should know (under 5 MB)."),
				"packet":                packetSchema,
			}, Required: []string{"client_id", "disputed_amount_cents", "other_party_name"},
				Example: map[string]any{"client_id": "5e2…", "package_id": "a41…", "disputed_amount_cents": 125000, "other_party_name": "Gulf Health Plan",
					"other_party_email": "disputes@gulfhealth.example", "in_network": "no",
					"packet": map[string]any{"claims": []map[string]any{{"claim_number": "4471", "date_of_service": "2026-08-02", "billed_cents": 300000, "paid_cents": 175000, "disputed_cents": 125000}}}}},
			Status: "201", Response: map[string]any{"case_id": "79f…", "case_number": "FL26-001203", "status": "AWAITING_PAYMENT", "payment_required": true,
				"initiating_party": "Coral Gables Imaging", "filed_by": "Sunrise Billing Partners"}},
		"POST /tpa/disputes/bulk": {Summary: "File many disputes at once",
			Description: "Upload a ZIP: `disputes.csv` plus one folder per `dispute_ref` holding that dispute's six documents (or one merged PDF). " +
				"Every row must be for one of your own clients (matched by `party_name`). Get a template from `GET …/tpa/disputes/bulk/sample.zip`. " +
				"Re-sending the same `batch_ref` never files twice.",
			Body:   &apiBody{Multipart: true, Properties: map[string]any{"file": docFile("The ZIP."), "batch_ref": docStr("Your own reference for this upload (makes retries safe).")}, Required: []string{"file"}},
			Status: "202", Response: map[string]any{"batch_id": "b77…", "batch_ref": "2026-11-04-a", "rows": []any{}, "waiting_for_documents": 0, "filed": 12, "refused": 1}},
		"GET /tpa/disputes/bulk/{batchId}": {Summary: "Check a bulk filing", Description: "Each row's outcome: filed (with its case number), waiting for documents, or refused and why.",
			Response: map[string]any{"batch_id": "b77…", "items": []map[string]any{{"dispute_ref": "A1", "status": "FILED", "case_number": "FL26-001204"}}}},
		"GET /tpa/disputes/bulk/sample.zip": {Summary: "Download a bulk filing template", ResponseType: "application/zip"},

		// ── TPA: following disputes ─────────────────────────────────────
		"GET /tpa/dashboard": {Summary: "Your disputes", Description: "Every dispute you filed with its stage, plus totals per client.",
			Response: map[string]any{"cases": []map[string]any{{"case_id": "79f…", "case_number": "FL26-001203", "status": "OPEN", "client_id": "5e2…",
				"client_name": "Coral Gables Imaging", "disputed_amount_cents": 125000, "filed": "2026-11-04"}}, "by_client": []any{}}},
		"GET /tpa/disputes/{caseId}/summary": {Summary: "One dispute at a glance", Description: "Its stage, key dates, the claims, and the fee.", Response: summaryAnswer},

		// ── TPA: fees ───────────────────────────────────────────────────
		"GET /tpa/fees":                  {Summary: "What you owe", Response: map[string]any{"unpaid": 3, "amount_cents": 105000, "card_fee_cents": 3075}},
		"GET /tpa/disputes/{caseId}/fee": {Summary: "One dispute's fee"},
		"POST /tpa/fees/checkout": {Summary: "Pay all unpaid fees",
			Description: "Returns a payment page (`checkout_url`) for every unpaid fee, or charges saved methods (`source`). " +
				"Payment is confirmed automatically; poll `GET …/tpa/fees` or the dispute.",
			Body: checkoutBody, Status: "201", Response: checkoutAnswer},
		"POST /tpa/disputes/{caseId}/pay":            {Summary: "Pay one dispute's fee", Body: checkoutBody, Status: "201", Response: checkoutAnswer},
		"POST /tpa/clients/{clientId}/fees/checkout": {Summary: "Pay one client's unpaid fees", Body: checkoutBody, Status: "201", Response: checkoutAnswer},
		"POST /tpa/disputes/bulk/{batchId}/checkout": {Summary: "Pay a bulk filing's fees", Body: checkoutBody, Status: "201", Response: checkoutAnswer},

		// ── TPA: requests from the office ───────────────────────────────
		"GET /tpa/document-requests": {Summary: "Documents the office asked for", Response: docRequestsAnswer},
		"POST /tpa/document-requests/{reqId}/files": {Summary: "Add a file to a document request",
			Body: &apiBody{Multipart: true, Properties: map[string]any{"file": docFile("")}, Required: []string{"file"}}, Status: "201",
			Response: map[string]any{"doc_id": "d55…", "filename": "itemized-bill.pdf"}},
		"POST /tpa/document-requests/{reqId}/submit": {Summary: "Send a document request's files",
			Description: "Sends what you added. For a permission request, answer with `decision`.",
			Body:        &apiBody{Properties: map[string]any{"note": docStr("Optional note to the office."), "decision": docEnum("Permission requests only.", "PROCEED", "DECLINE")}},
			Response:    map[string]any{"status": "SUBMITTED", "workflow_signalled": true}},
		"GET /tpa/sent-documents": {Summary: "Letters the office sent you"},

		// ── Health plan ─────────────────────────────────────────────────
		"GET /hp/me": {Summary: "Your health plan", Description: "Your plan's record. A good first call to check a key works.",
			Response: map[string]any{"health_plan": map[string]any{"id": "p9…", "name": "Gulf Health Plan", "naic": "12345", "status": "ACTIVE"}}},
		"GET /hp/responding": {Summary: "Disputes against your plan",
			Description: "Disputes another side filed against you, once the office finds them eligible.",
			Response: map[string]any{"cases": []map[string]any{{"case_id": "79f…", "case_number": "FL26-001203", "status": "OPEN", "initiating_party": "Coral Gables Imaging",
				"disputed_amount_cents": 125000, "response_status": "AWAITING", "response_due": "2026-11-20"}}}},
		"GET /hp/responding/{caseId}": {Summary: "One dispute against your plan", Description: "The dispute and the documents you may see."},
		"POST /hp/responding/{caseId}/documents": {Summary: "Upload a response document",
			Body:   &apiBody{Multipart: true, Properties: map[string]any{"file": docFile("")}, Required: []string{"file"}},
			Status: "201", Response: map[string]any{"doc_id": "d56…", "filename": "plan-response.pdf"}},
		"POST /hp/responding/{caseId}/submit": {Summary: "Submit your response", Description: "Sends your uploaded documents to the office; it counts at once.",
			Body: &apiBody{Properties: map[string]any{"note": docStr("Optional note.")}}, Response: map[string]any{"status": "SUBMITTED", "workflow_signalled": true}},
		"POST /hp/responding/{caseId}/opt-out": {Summary: "Ask to opt out of a dispute",
			Body: &apiBody{Properties: map[string]any{"reason": docStr("Why.")}, Required: []string{"reason"}}, Response: map[string]any{"status": "OPT_OUT_REQUESTED"}},
		"POST /hp/intake": {Summary: "File a dispute for one of your parties",
			Body: &apiBody{Properties: map[string]any{
				"party_id": docStr("Your party filing the dispute."), "package_id": docStr("This dispute's document set (optional unless your state program requires it); its files must have been read."),
				"disputed_amount_cents": docInt("Amount in dispute, in cents."), "other_party_name": docStr("The provider the dispute is against."),
				"other_party_email": docStr("Where the other side's notices go."), "other_party_npi": docStr("The provider's NPI."),
				"notes": docStr(""), "packet": packetSchema,
			}, Required: []string{"party_id", "disputed_amount_cents", "other_party_name"}}, Status: "201"},
		"GET /hp/fees":                   {Summary: "What you owe", Response: map[string]any{"unpaid": 1, "amount_cents": 35000, "card_fee_cents": 1025}},
		"POST /hp/fees/checkout":         {Summary: "Pay all unpaid fees", Body: checkoutBody, Status: "201", Response: checkoutAnswer},
		"POST /hp/disputes/{caseId}/pay": {Summary: "Pay one dispute's fee", Body: checkoutBody, Status: "201", Response: checkoutAnswer},
		"GET /hp/document-requests":      {Summary: "Documents the office asked for", Response: docRequestsAnswer},
		"POST /hp/document-requests/{reqId}/files": {Summary: "Add a file to a document request",
			Body: &apiBody{Multipart: true, Properties: map[string]any{"file": docFile("")}, Required: []string{"file"}}, Status: "201"},
		"POST /hp/document-requests/{reqId}/submit": {Summary: "Send a document request's files",
			Body: &apiBody{Properties: map[string]any{"note": docStr(""), "decision": docEnum("Permission requests only.", "PROCEED", "DECLINE")}}},
		"GET /hp/disputes/{caseId}/summary": {Summary: "One dispute at a glance", Response: summaryAnswer},

		// ── Shared reads ────────────────────────────────────────────────
		"GET /cases/{caseId}":                            {Summary: "A dispute you filed", Description: "The record of a dispute your organization filed."},
		"GET /cases/{caseId}/clocks":                     {Summary: "A dispute's deadlines"},
		"GET /cases/{caseId}/documents":                  {Summary: "A dispute's documents"},
		"GET /cases/{caseId}/documents/{docId}/download": {Summary: "Download a dispute document", ResponseType: "application/octet-stream"},

		// ── Staff ───────────────────────────────────────────────────────
		"GET /api-keys":               {Summary: "Every partner API key in this tenant", Description: "Tenant and platform admins.", Response: keyList},
		"DELETE /api-keys/{keyId}":    {Summary: "Revoke any partner API key", Description: "Tenant and platform admins. Takes effect at once."},
		"GET /api-docs/internal.json": {Summary: "This reference", Description: "Office staff only."},
	}
	for k, v := range apiKeyDocs("/tpa", "TPA") {
		m[k] = v
	}
	for k, v := range apiKeyDocs("/hp", "plan") {
		m[k] = v
	}
	return m
}()
