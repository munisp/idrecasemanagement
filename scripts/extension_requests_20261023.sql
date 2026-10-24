-- A request for more time (document request reason EXTENSION, case-api deadline_extensions.go requestExtension)
-- carries the deadline and the days asked for, applied when the side agrees.
ALTER TABLE public.document_requests ADD COLUMN IF NOT EXISTS meta jsonb;
