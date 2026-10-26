-- Personal mailboxes: every staff member gets an address on the inbound domain (Resend receiving), may connect their
-- Outlook account (Microsoft Graph), and every message -- received, sent, or synchronised from Outlook -- is kept
-- here with its history. A person reads only their own mail: every read is filtered by user_sub.
-- Idempotent; run once per database (idre, idre_prod).

CREATE TABLE IF NOT EXISTS public.user_mailboxes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant      text NOT NULL,
    user_sub    text NOT NULL,                 -- Keycloak user id (the JWT sub)
    username    text NOT NULL,
    address     text NOT NULL,                 -- lower case, e.g. jane.doe@mail.upi.dev
    status      text NOT NULL DEFAULT 'ACTIVE', -- ACTIVE | DISABLED
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS user_mailboxes_address ON public.user_mailboxes (address);
CREATE UNIQUE INDEX IF NOT EXISTS user_mailboxes_user ON public.user_mailboxes (tenant, user_sub);

-- One row per message, whatever carried it.
CREATE TABLE IF NOT EXISTS public.mail_messages (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant              text NOT NULL,
    user_sub            text NOT NULL,                 -- whose mail it is
    mailbox_id          uuid REFERENCES public.user_mailboxes(id),
    source              text NOT NULL,                 -- RESEND (the platform address) | OUTLOOK
    direction           text NOT NULL,                 -- IN | OUT
    folder              text NOT NULL,                 -- inbox | sent
    provider_message_id text NOT NULL DEFAULT '',      -- Resend email id, or the Graph message id
    internet_message_id text NOT NULL DEFAULT '',      -- the RFC 5322 Message-ID
    conversation_id     text NOT NULL DEFAULT '',
    from_addr           text NOT NULL DEFAULT '',
    from_name           text NOT NULL DEFAULT '',
    to_addrs            jsonb NOT NULL DEFAULT '[]',
    cc_addrs            jsonb NOT NULL DEFAULT '[]',
    subject             text NOT NULL DEFAULT '',
    body_text           text NOT NULL DEFAULT '',
    body_html           text NOT NULL DEFAULT '',
    has_attachments     boolean NOT NULL DEFAULT false,
    status              text NOT NULL,                 -- RECEIVED | SENT | DELIVERED | BOUNCED | COMPLAINED | FAILED | SYNCED
    status_detail       text NOT NULL DEFAULT '',
    auth_results        jsonb NOT NULL DEFAULT '{}',   -- spf / dkim / dmarc for received mail
    is_read             boolean NOT NULL DEFAULT false,
    occurred_at         timestamptz NOT NULL,          -- received or sent
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
-- The same provider message is stored once per person (a retried webhook or a re-run sync files nothing twice).
CREATE UNIQUE INDEX IF NOT EXISTS mail_messages_provider ON public.mail_messages (user_sub, source, provider_message_id) WHERE provider_message_id <> '';
CREATE INDEX IF NOT EXISTS mail_messages_user ON public.mail_messages (tenant, user_sub, folder, occurred_at DESC);

CREATE TABLE IF NOT EXISTS public.mail_attachments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id   uuid NOT NULL REFERENCES public.mail_messages(id) ON DELETE CASCADE,
    filename     text NOT NULL,
    content_type text NOT NULL DEFAULT 'application/octet-stream',
    size_bytes   bigint NOT NULL DEFAULT 0,
    object_key   text NOT NULL DEFAULT '',             -- sealed in the documents bucket; '' when not stored
    status       text NOT NULL DEFAULT 'STORED',       -- STORED | TOO_LARGE | FAILED
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS mail_attachments_message ON public.mail_attachments (message_id);

-- What happened to each message, in order: received, sent, delivered, bounced, read, synced...
CREATE TABLE IF NOT EXISTS public.mail_message_events (
    id          bigserial PRIMARY KEY,
    message_id  uuid NOT NULL REFERENCES public.mail_messages(id) ON DELETE CASCADE,
    event       text NOT NULL,
    detail      text NOT NULL DEFAULT '',
    actor       text NOT NULL DEFAULT '',
    at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS mail_message_events_message ON public.mail_message_events (message_id, at);

-- A person's Outlook account. The tokens are sealed by the vault, never stored in clear.
CREATE TABLE IF NOT EXISTS public.outlook_connections (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant                text NOT NULL,
    user_sub              text NOT NULL,
    ms_user_id            text NOT NULL DEFAULT '',
    email                 text NOT NULL DEFAULT '',
    display_name          text NOT NULL DEFAULT '',
    access_token_sealed   bytea,
    refresh_token_sealed  bytea,
    token_expires_at      timestamptz,
    status                text NOT NULL DEFAULT 'ACTIVE',   -- ACTIVE | ERROR | DISCONNECTED
    last_error            text NOT NULL DEFAULT '',
    inbox_synced_through  timestamptz,                       -- newest received time imported
    sent_synced_through   timestamptz,                       -- newest sent time imported
    last_sync_at          timestamptz,
    connected_at          timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS outlook_connections_user ON public.outlook_connections (tenant, user_sub);

-- Inbound mail is filed under the dispute it names (mailfiling.go): the person's copy knows which dispute and which
-- mail-log row; the mail log keeps the Message-ID (so one mail reaching several people, or a retry, is filed once,
-- and a reply threads to its dispute) and why it was or wasn't matched.
ALTER TABLE public.mail_messages ADD COLUMN IF NOT EXISTS case_id text NOT NULL DEFAULT '';
ALTER TABLE public.mail_messages ADD COLUMN IF NOT EXISTS correspondence_id bigint;
CREATE INDEX IF NOT EXISTS mail_messages_msgid ON public.mail_messages (tenant, internet_message_id) WHERE internet_message_id <> '';
ALTER TABLE public.correspondence_log ADD COLUMN IF NOT EXISTS internet_message_id text NOT NULL DEFAULT '';
ALTER TABLE public.correspondence_log ADD COLUMN IF NOT EXISTS match_reason text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS correspondence_log_msgid ON public.correspondence_log (tenant, internet_message_id) WHERE internet_message_id <> '';

-- The program's company mailbox (company_outlook.go) is an Outlook connection owned by 'company', not a person. It may
-- read a shared mailbox through the signed-in admin's access (mailbox), and records which admin connected it.
ALTER TABLE public.outlook_connections ADD COLUMN IF NOT EXISTS mailbox text NOT NULL DEFAULT '';
ALTER TABLE public.outlook_connections ADD COLUMN IF NOT EXISTS connected_by text NOT NULL DEFAULT '';

-- The program's own address (system_mailbox.go) is a user_mailboxes row owned by 'system', with the name its mail is
-- sent under. How the program's mail goes out: from that address (platform) or from its company Outlook (outlook).
ALTER TABLE public.user_mailboxes ADD COLUMN IF NOT EXISTS display_name text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS public.program_mail_settings (
    tenant      text PRIMARY KEY,
    send_via    text NOT NULL DEFAULT 'platform',   -- platform | outlook
    updated_by  text NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- The Message-ID a sent mail finally carried: the provider (Resend, through Amazon SES) replaces ours, and replies quote
-- its one. Recorded from the delivery reports so a reply is filed under the case its mail was about.
ALTER TABLE public.correspondence_log ADD COLUMN IF NOT EXISTS provider_internet_id text NOT NULL DEFAULT '';
ALTER TABLE public.mail_messages ADD COLUMN IF NOT EXISTS provider_internet_id text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS correspondence_log_provider_msgid ON public.correspondence_log (tenant, provider_internet_id) WHERE provider_internet_id <> '';
CREATE INDEX IF NOT EXISTS mail_messages_provider_msgid ON public.mail_messages (tenant, provider_internet_id) WHERE provider_internet_id <> '';
