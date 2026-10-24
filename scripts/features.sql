-- Author: Patrick Munis at newwave
-- Per-tenant feature flags (features.go). Kept in their own table, not in
-- program_rules.config: whether a tenant has a program_rules row at all is
-- what marks it as running its own program (loadProgram, the deadline sweep,
-- the workflow worker), so writing a flag there for a federal tenant would
-- silently switch it into custom-program mode. Same reasoning as
-- tenant_labels. A flag that is absent counts as ON.
CREATE TABLE IF NOT EXISTS public.tenant_features (
    tenant      text PRIMARY KEY,
    features    jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_by  text NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now()
);
