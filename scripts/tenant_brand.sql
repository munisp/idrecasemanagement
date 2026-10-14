-- ============================================================
-- The portal's brand name, per tenant (program.go setTenantBrand)
--
-- Shown in the portal's sidebar and page titles for that tenant and on the
-- public pages for it. NULL = the platform default, "Capitol Bridge".
-- ============================================================
ALTER TABLE public.tenant_labels ADD COLUMN IF NOT EXISTS brand_name text;
