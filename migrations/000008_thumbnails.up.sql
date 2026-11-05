-- Thumbnails of image attachments (plan 5.1: GET /attachments/{id}/thumbnail),
-- generated on the first request and kept in the blob store. One row per
-- source blob and size, so equal attachments share their thumbnails. The
-- thumbnail blob holds one reference for the row; blob gc deletes the rows of
-- a source blob that is no longer referenced and releases the thumbnails.
CREATE TABLE thumbnails (
  source_sha256 bytea  NOT NULL REFERENCES blobs(sha256),
  size          text   NOT NULL CHECK (size IN ('s','m')),
  thumb_sha256  bytea  NOT NULL REFERENCES blobs(sha256),
  content_type  text   NOT NULL,
  width         int    NOT NULL,
  height        int    NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (source_sha256, size)
);
CREATE INDEX thumbnails_thumb ON thumbnails (thumb_sha256);
