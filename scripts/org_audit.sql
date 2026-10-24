-- Author: Patrick Munis at newwave
-- ============================================================
-- The audit trail of outside organizations (orgaudit.go)
--
-- audit_log.org names the TPA ("tpa:<id>") or health plan ("plan:<id>") an
-- entry is about, so each organization reads its own trail on its dashboard.
-- The organization is also inside the hashed payload ("org", or the entry's
-- tpa_id / plan_id); this column is only the index.
-- ============================================================

ALTER TABLE public.audit_log ADD COLUMN IF NOT EXISTS org text;
CREATE INDEX IF NOT EXISTS audit_log_org ON public.audit_log (tenant, org, id) WHERE org IS NOT NULL;

-- Entries written before the column: their payload already names the organization.
UPDATE public.audit_log
SET org = CASE
    WHEN payload ~ '"tpa_id":"[0-9a-f-]{36}"' THEN 'tpa:' || substring(payload from '"tpa_id":"([0-9a-f-]{36})"')
    ELSE 'plan:' || substring(payload from '"plan_id":"([0-9a-f-]{36})"') END
WHERE org IS NULL AND payload LIKE '{%'
  AND (payload ~ '"tpa_id":"[0-9a-f-]{36}"' OR payload ~ '"plan_id":"[0-9a-f-]{36}"');
