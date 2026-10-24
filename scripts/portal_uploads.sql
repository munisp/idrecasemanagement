-- Author: Patrick Munis at newwave
-- ============================================================
-- Resumable uploads from the portal (portal_uploads.go)
--
-- A big file is sent in chunks; each chunk is virus-scanned and vault-sealed
-- before it is stored (<tenant>/uploads/<id>/<n>.enc). A finished upload is
-- used once by an upload endpoint (status USED, chunks removed); an unused
-- one expires after 24 hours (EXPIRED, chunks removed).
-- ============================================================
CREATE TABLE IF NOT EXISTS public.portal_uploads (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant      text NOT NULL,
    owner       text NOT NULL,                -- the person who started it
    filename    text NOT NULL,
    size_bytes  bigint NOT NULL,
    received    bigint NOT NULL DEFAULT 0,
    parts       int NOT NULL DEFAULT 0,
    status      text NOT NULL DEFAULT 'OPEN', -- OPEN | COMPLETE | USED | ABORTED | EXPIRED
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL DEFAULT now() + interval '24 hours'
);
CREATE INDEX IF NOT EXISTS portal_uploads_owner ON public.portal_uploads (tenant, owner, status);
CREATE INDEX IF NOT EXISTS portal_uploads_expiry ON public.portal_uploads (expires_at) WHERE status IN ('OPEN','COMPLETE');
