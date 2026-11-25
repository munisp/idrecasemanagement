-- Notes on eligibility reviews: the reviewer's working notes saved with a review, and later notes from the PM or
-- the Attorney on the same review. One row per note, never edited, so the record shows who said what and when.
-- Safe to run more than once.
CREATE TABLE IF NOT EXISTS public.eligibility_review_notes (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant      text NOT NULL,
  case_id     text NOT NULL,
  review_id   uuid NOT NULL REFERENCES public.eligibility_reviews(id),
  body        text NOT NULL CHECK (length(btrim(body)) BETWEEN 1 AND 4000),
  author      text NOT NULL,
  author_role text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS eligibility_review_notes_case_idx ON public.eligibility_review_notes (tenant, case_id, created_at);
