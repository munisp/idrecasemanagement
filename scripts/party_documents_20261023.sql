-- party_documents: the documents a letter carried to an organization on the platform (a TPA or a health plan),
-- so it finds them under the dispute in its portal, not only in its mailbox (case-api party_documents.go).
CREATE TABLE IF NOT EXISTS public.party_documents (
  tenant    text        NOT NULL,
  case_id   text        NOT NULL,
  org       text        NOT NULL,            -- "tpa:<id>" | "plan:<id>"
  side      text        NOT NULL,            -- PROVIDER | HEALTH_PLAN
  doc_id    uuid        NOT NULL,
  template  text        NOT NULL DEFAULT '', -- the letter that carried it
  subject   text,
  qa_id     text,
  sent_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, org, doc_id)
);
CREATE INDEX IF NOT EXISTS party_documents_case ON public.party_documents (tenant, case_id);

-- party_mail: each letter itself, as the organization received it (subject, text, when), shown with its documents.
CREATE TABLE IF NOT EXISTS public.party_mail (
  tenant    text        NOT NULL,
  case_id   text        NOT NULL,
  org       text        NOT NULL,
  side      text        NOT NULL,
  qa_id     text        NOT NULL,
  template  text        NOT NULL DEFAULT '',
  subject   text,
  body      text,
  sent_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, org, qa_id)
);
CREATE INDEX IF NOT EXISTS party_mail_case ON public.party_mail (tenant, case_id);
