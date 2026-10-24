-- Case status transitions: structured capture for the transition-matrix
-- model. Until now a case's status history existed only inside free-text
-- activity bodies and audit payloads -- fine for reading, unfit for
-- computing dwell times and path shares. This table is written by the API
-- at every status write site (manual program update, bulk action,
-- auto-advance, intake) so the matrix reads one clean fact table.
--
-- Two axes share the table because the platform runs two parallel status
-- vocabularies:
--   lifecycle -- tenant_X.cases.status, the canonical federal NSA lifecycle
--                (INITIATED .. OPEN_NEGOTIATION .. DETERMINED .. CLOSED_%)
--   program   -- tenant_X.cases.internal_status, the program-specific
--                working board (FL AHCA CDR spellings, canonicalised
--                against program_rules)
-- They are never mixed in one chain: every row names its axis and the
-- matrix is computed per axis.

CREATE TABLE IF NOT EXISTS public.case_transitions (
    id          bigserial PRIMARY KEY,
    tenant      text NOT NULL,
    case_id     text NOT NULL,
    axis        text NOT NULL,            -- lifecycle | program
    from_status text,                     -- NULL on the opening transition
    to_status   text NOT NULL,
    actor_sub   text,                     -- NULL for machine actors
    source      text NOT NULL,            -- status_update | bulk | auto_advance | intake | initiation | backfill
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS case_transitions_tenant_case
    ON public.case_transitions (tenant, case_id, axis, created_at, id);

CREATE INDEX IF NOT EXISTS case_transitions_tenant_axis
    ON public.case_transitions (tenant, axis, created_at);

-- Backfill from the audit trail (runs once: skipped when the table already
-- has rows). CASE_STATUS_CHANGED payloads carry exactly one of two keys --
-- "status" from the bulk path (lifecycle axis), "internal_status" from the
-- manual program update (program axis) -- and the from-side of each hop is
-- the previous hop's to-side within the same case and axis.
WITH src AS (
    SELECT tenant, case_id,
           CASE WHEN payload::jsonb ? 'status' THEN 'lifecycle' ELSE 'program' END AS axis,
           coalesce(payload::jsonb->>'status', payload::jsonb->>'internal_status') AS to_status,
           nullif(payload::jsonb->>'by', '') AS actor_sub,
           created_at, id
    FROM public.audit_log
    WHERE action = 'CASE_STATUS_CHANGED'
      AND payload LIKE '{%'
      AND (payload::jsonb ? 'status' OR payload::jsonb ? 'internal_status')
      AND coalesce(payload::jsonb->>'status', payload::jsonb->>'internal_status') IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM public.case_transitions)
), chained AS (
    SELECT tenant, case_id, axis,
           lag(to_status) OVER w AS from_status,
           to_status, actor_sub, created_at
    FROM src
    WINDOW w AS (PARTITION BY tenant, case_id, axis ORDER BY created_at, id)
)
INSERT INTO public.case_transitions
    (tenant, case_id, axis, from_status, to_status, actor_sub, source, created_at)
SELECT tenant, case_id, axis, from_status, to_status, actor_sub, 'backfill', created_at
FROM chained
WHERE from_status IS NULL OR from_status <> to_status;
