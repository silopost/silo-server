package mailview

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/image/draw"
	"golang.org/x/image/webp"

	"github.com/silopost/silo-server/internal/blob"
)

// ErrThumbnailUnsupported: the attachment is not an image Silo can make a
// thumbnail of (plan 5.1; PDF needs a renderer that Silo, built without cgo,
// does not have), or the picture cannot be decoded or is too large.
var ErrThumbnailUnsupported = errors.New("mailview: no thumbnail for this attachment")

// ThumbnailSizes are the sizes of GET /attachments/{id}/thumbnail: the
// longer side in pixels. Smaller pictures are not enlarged.
var ThumbnailSizes = map[string]int{"s": 64, "m": 256}

// MaxThumbnailPixels bounds the picture decoded for a thumbnail (20 MP).
const MaxThumbnailPixels = 20_000_000

var thumbnailTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// ThumbnailSupported tells whether an attachment of this content type has a
// thumbnail (png, jpeg, gif, webp).
func ThumbnailSupported(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = contentType
	}
	return thumbnailTypes[strings.ToLower(strings.TrimSpace(mt))]
}

// Thumbnail is a stored thumbnail.
type Thumbnail struct {
	ContentType   string
	SHA           string
	Width, Height int
	Body          io.ReadSeekCloser
}

// ThumbnailsGenerated is the number of thumbnails this View has decoded and
// encoded (the others came from the cache).
func (v *View) ThumbnailsGenerated() int64 { return v.thumbsGenerated.Load() }

// Thumbnail returns the thumbnail of an attachment (or upload) of the user in
// size s or m. It is made on the first request and kept in the blob store
// (table thumbnails, one per source blob and size); the attachment is
// downloaded first when it is not in the mirror (as OpenAttachment).
func (v *View) Thumbnail(ctx context.Context, userID, attachmentID int64, size string) (*Thumbnail, error) {
	side, ok := ThumbnailSizes[size]
	if !ok {
		return nil, fmt.Errorf("%w: size must be s or m", ErrInvalid)
	}
	var ct string
	err := v.pool.QueryRow(ctx, `SELECT content_type FROM attachments WHERE id = $1 AND user_id = $2`, attachmentID, userID).Scan(&ct)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !ThumbnailSupported(ct) {
		return nil, ErrThumbnailUnsupported
	}
	if v.blobs == nil {
		return nil, ErrContentNotAvailable
	}
	att, err := v.OpenAttachment(ctx, userID, attachmentID)
	if err != nil {
		return nil, err
	}
	defer att.Body.Close()
	src, _ := hex.DecodeString(att.SHA)

	t := &Thumbnail{}
	var thumbSHA []byte
	err = v.pool.QueryRow(ctx, `SELECT thumb_sha256, content_type, width, height FROM thumbnails WHERE source_sha256 = $1 AND size = $2`,
		src, size).Scan(&thumbSHA, &t.ContentType, &t.Width, &t.Height)
	switch {
	case err == nil:
		t.SHA = hex.EncodeToString(thumbSHA)
		f, err := v.blobs.Open(ctx, t.SHA)
		if err == nil {
			t.Body = f
			return t, nil
		}
		if !errors.Is(err, blob.ErrNotFound) {
			return nil, err
		}
		// The bytes are gone: make it again under the same row.
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}

	data, outCT, w, h, err := makeThumbnail(att.Body, side)
	if err != nil {
		return nil, err
	}
	v.thumbsGenerated.Add(1)
	sum, n, err := v.blobs.Put(ctx, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	sha, _ := hex.DecodeString(sum)
	err = pgx.BeginFunc(ctx, v.pool, func(tx pgx.Tx) error {
		var old []byte
		err := tx.QueryRow(ctx, `SELECT thumb_sha256 FROM thumbnails WHERE source_sha256 = $1 AND size = $2 FOR UPDATE`, src, size).Scan(&old)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.Exec(ctx, `
INSERT INTO blobs (sha256, size_bytes, kind, ref_count) VALUES ($1, $2, 'local', 1)
ON CONFLICT (sha256) DO UPDATE SET ref_count = blobs.ref_count + 1`, sha, n); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
INSERT INTO thumbnails (source_sha256, size, thumb_sha256, content_type, width, height) VALUES ($1, $2, $3, $4, $5, $6)`,
				src, size, sha, outCT, w, h)
			return err
		case err != nil:
			return err
		}
		// The row exists (bytes lost, or made meanwhile): it keeps its
		// reference; the bytes were stored again under the same hash.
		return nil
	})
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" { // made by a concurrent request
			err = nil
		} else {
			return nil, fmt.Errorf("mailview: record thumbnail: %w", err)
		}
	}
	return &Thumbnail{ContentType: outCT, SHA: sum, Width: w, Height: h, Body: nopSeekCloser{bytes.NewReader(data)}}, nil
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// makeThumbnail decodes a png, jpeg, gif (first frame) or webp picture of at
// most MaxThumbnailPixels and scales it so that its longer side is at most
// side: a JPEG when it is opaque, else a PNG (transparency kept).
func makeThumbnail(r io.ReadSeeker, side int) (data []byte, contentType string, w, h int, err error) {
	cfg, format, err := image.DecodeConfig(r)
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("%w: %v", ErrThumbnailUnsupported, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxThumbnailPixels {
		return nil, "", 0, 0, fmt.Errorf("%w: %dx%d pixels", ErrThumbnailUnsupported, cfg.Width, cfg.Height)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, "", 0, 0, err
	}
	var src image.Image
	switch format {
	case "png":
		src, err = png.Decode(r)
	case "jpeg":
		src, err = jpeg.Decode(r)
	case "gif":
		src, err = gif.Decode(r)
	case "webp":
		src, err = webp.Decode(r)
	default:
		err = fmt.Errorf("format %s", format)
	}
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("%w: %v", ErrThumbnailUnsupported, err)
	}
	b := src.Bounds()
	w, h = b.Dx(), b.Dy()
	if w > side || h > side {
		if w >= h {
			w, h = side, max(1, h*side/w)
		} else {
			w, h = max(1, w*side/h), side
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	var buf bytes.Buffer
	if dst.Opaque() {
		err, contentType = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}), "image/jpeg"
	} else {
		err, contentType = png.Encode(&buf, dst), "image/png"
	}
	if err != nil {
		return nil, "", 0, 0, err
	}
	return buf.Bytes(), contentType, w, h, nil
}
