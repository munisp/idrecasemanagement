-- ============================================================
-- A TPA's logins scoped to one of its clients, and a Finance role (tpa_client_scope.go)
--
-- tpa_users.client_id: NULL = the login covers the whole TPA (every login before
-- this change, unchanged); set = it sees and acts on that one client only.
-- team_role gains FINANCE: payment operations only.
-- Additive and safe to repeat. Apply BEFORE the case-api that reads the column.
-- ============================================================

ALTER TABLE public.tpa_users ADD COLUMN IF NOT EXISTS client_id uuid REFERENCES public.tpa_clients(id);
CREATE INDEX IF NOT EXISTS tpa_users_client ON public.tpa_users (tenant, client_id) WHERE client_id IS NOT NULL;

ALTER TABLE public.tpa_users DROP CONSTRAINT IF EXISTS tpa_users_team_role_check;
ALTER TABLE public.tpa_users ADD CONSTRAINT tpa_users_team_role_check
  CHECK (team_role = ANY (ARRAY['ADMIN'::text, 'STAFF'::text, 'FINANCE'::text]));

-- A login's client must be a client of its own TPA, whatever writes the row.
CREATE OR REPLACE FUNCTION public.tpa_users_client_in_tpa() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.client_id IS NOT NULL AND NOT EXISTS (
       SELECT 1 FROM public.tpa_clients c WHERE c.id = NEW.client_id AND c.tenant = NEW.tenant AND c.tpa_id = NEW.tpa_id) THEN
    RAISE EXCEPTION 'tpa_users.client_id % is not a client of TPA %', NEW.client_id, NEW.tpa_id;
  END IF;
  RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS tpa_users_client_in_tpa ON public.tpa_users;
CREATE TRIGGER tpa_users_client_in_tpa BEFORE INSERT OR UPDATE OF client_id, tpa_id ON public.tpa_users
  FOR EACH ROW EXECUTE FUNCTION public.tpa_users_client_in_tpa();
