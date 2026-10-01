-- Case-management layer: relationships, batching, checklists, notifications,
-- saved views, assignment state. (Applied per tenant where noted.)

-- Assignment state on cases (per-tenant tables): run for each tenant schema:
--   ALTER TABLE tenant_<st>.cases
--     ADD COLUMN IF NOT EXISTS assigned_to text,
--     ADD COLUMN IF NOT EXISTS assigned_role text,     -- CASE_MANAGER | ARBITRATOR
--     ADD COLUMN IF NOT EXISTS batch_id uuid,
--     ADD COLUMN IF NOT EXISTS parent_case_id uuid,
--     ADD COLUMN IF NOT EXISTS duplicate_of uuid;

-- Related cases: batch groups, parent/child, duplicates.
CREATE TABLE IF NOT EXISTS public.case_relationships (
    id          bigserial PRIMARY KEY,
    tenant      text NOT NULL,
    case_id     text NOT NULL,
    related_case_id text NOT NULL,
    rel_type    text NOT NULL,      -- BATCH | PARENT_CHILD | DUPLICATE
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, case_id, related_case_id, rel_type)
);

-- Configurable stage checklists (e.g. 12-element determination checklist).
CREATE TABLE IF NOT EXISTS public.case_checklists (
    id        bigserial PRIMARY KEY,
    tenant    text NOT NULL,
    case_id   text NOT NULL,
    stage     text NOT NULL,        -- INTAKE | ELIGIBILITY | OFFERS | DETERMINATION | PAYMENT
    item      text NOT NULL,
    required  boolean NOT NULL DEFAULT true,
    done      boolean NOT NULL DEFAULT false,
    done_by   text,
    done_at   timestamptz,
    UNIQUE (tenant, case_id, stage, item)
);

-- In-app notifications (bell): assignments, escalations, milestones, mentions.
CREATE TABLE IF NOT EXISTS public.notifications (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    user_sub   text NOT NULL,       -- keycloak sub; '*' = role broadcast marker
    type       text NOT NULL,       -- ASSIGNMENT | ESCALATION | MILESTONE | MENTION | SLA_BREACH
    body       text NOT NULL,
    link       text,                -- portal hash, e.g. #/cases/<id>
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS notifications_user ON public.notifications (tenant, user_sub, read_at);

-- Per-user saved views (Salesforce "list views").
CREATE TABLE IF NOT EXISTS public.saved_views (
    id        bigserial PRIMARY KEY,
    tenant    text NOT NULL,
    user_sub  text NOT NULL,
    object    text NOT NULL,        -- CASES | ACCOUNTS | LEADS | TASKS
    name      text NOT NULL,
    filters   jsonb NOT NULL DEFAULT '{}',
    pinned    boolean NOT NULL DEFAULT false,   -- pinned views surface first in L2 nav
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, user_sub, object, name)
);

-- Escalation log (SLA breach -> supervisor trail).
CREATE TABLE IF NOT EXISTS public.escalations (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    clock      text NOT NULL,
    level      int NOT NULL DEFAULT 1,
    escalated_to text,              -- role or sub
    detail     text,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- User preferences: theme, density, last tenant, palette recents — the
-- portal keeps a device-local copy for offline/instant paint, but this table
-- is the source of truth so prefs follow the user across PWA/desktop/native.
CREATE TABLE IF NOT EXISTS public.user_prefs (
    tenant     text NOT NULL,
    user_sub   text NOT NULL,
    key        text NOT NULL,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, user_sub, key)
);
