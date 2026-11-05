UPDATE blobs b SET ref_count = GREATEST(b.ref_count - c.n, 0)
FROM (SELECT thumb_sha256 AS sha, count(*)::int AS n FROM thumbnails GROUP BY thumb_sha256) c
WHERE b.sha256 = c.sha;
DROP TABLE IF EXISTS thumbnails;
