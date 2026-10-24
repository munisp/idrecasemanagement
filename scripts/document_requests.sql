-- Author: Patrick Munis at newwave
-- ============================================================
-- Document requests (document_requests.go)
--
-- A reviewer asks one side of a dispute for documents -- for verification,
-- an eligibility re-check, a missing or unreadable document, or with an RFI
-- letter. When that side is an organization on the platform (a TPA, a health
-- plan) the request appears in its portal and it uploads there; the email
-- only tells it to. A side with no portal account gets the secure upload
-- link by email, as before.
-- ============================================================

CREATE TABLE IF NOT EXISTS public.document_requests (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant        text NOT NULL,
    case_id       text NOT NULL,
    side          text NOT NULL,               -- PROVIDER | HEALTH_PLAN (who is asked)
    org           text,                        -- tpa:<id> | plan:<id>; NULL = no portal account (email link)
    reason        text NOT NULL,               -- VERIFICATION | ELIGIBILITY_RECHECK | MISSING_DOCUMENT | CLEARER_COPY | RFI | OTHER
    items         jsonb NOT NULL DEFAULT '[]', -- what is asked for, in words
    message       text,
    due_date      date,
    source        text NOT NULL DEFAULT 'reviewer', -- reviewer | rfi_letter
    status        text NOT NULL DEFAULT 'OPEN',     -- OPEN | SUBMITTED | COMPLETE | CANCELLED
    requested_by  text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    submitted_at  timestamptz,
    submitted_by  text,
    response_note text,
    closed_at     timestamptz,
    closed_by     text
);
CREATE INDEX IF NOT EXISTS document_requests_org ON public.document_requests (tenant, org, status);
CREATE INDEX IF NOT EXISTS document_requests_case ON public.document_requests (tenant, case_id);

-- The files uploaded for a request (they are case documents too).
CREATE TABLE IF NOT EXISTS public.document_request_files (
    request_id  uuid NOT NULL REFERENCES public.document_requests(id) ON DELETE CASCADE,
    tenant      text NOT NULL,
    doc_id      uuid NOT NULL,
    filename    text NOT NULL,
    uploaded_by text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id, doc_id)
);
