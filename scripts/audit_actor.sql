-- ============================================================
-- Everyone's own audit trail (orgaudit.go myAuditLog: My activity)
--
-- audit_log.actor is the person an entry records (its payload's "by"), so each
-- user reads only their own entries. The person is also in the hashed
-- payload; this column is only the index. NULL for machine actors.
-- ============================================================
ALTER TABLE public.audit_log ADD COLUMN IF NOT EXISTS actor text;
CREATE INDEX IF NOT EXISTS audit_log_actor ON public.audit_log (tenant, actor, id) WHERE actor IS NOT NULL;

UPDATE public.audit_log
SET actor = substring(payload from '"by":"([^"]+)"')
WHERE actor IS NULL AND payload LIKE '{%' AND payload ~ '"by":"[^"]+"'
  AND substring(payload from '"by":"([^"]+)"') !~ '^(system|stripe)';
