-- 000007_filters: filters get a name, an on/off switch and updated_at (plan
-- 3.11, API GET/POST/PUT/DELETE /filters). actions keeps the API shape
-- ({"archive":true,"addLabelIds":[7],...}); criteria is the search syntax.
ALTER TABLE filters
  ADD COLUMN name       text        NOT NULL DEFAULT '',
  ADD COLUMN enabled    bool        NOT NULL DEFAULT true,
  ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX filters_user ON filters (user_id);
