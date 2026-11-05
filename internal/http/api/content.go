package api

import (
	"context"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// htmlCSP is sent with the sanitised HTML of a message: the document may load
// images and inline styles, nothing else (no scripts, frames, forms, fonts,
// network requests). The web app also shows it in a sandboxed iframe.
const htmlCSP = "default-src 'none'; img-src data: https: http: cid:; style-src 'unsafe-inline'"

// attachmentCSP locks down attachment bytes served from the API origin.
const attachmentCSP = "default-src 'none'; sandbox"

type messageIDInput struct {
	ID string `path:"id"`
}

type attachmentInput struct {
	ID     string `path:"id"`
	Inline bool   `query:"inline" doc:"Show in the browser (images only) instead of downloading."`
}

func streamOf(ct string, desc string) map[string]*huma.Response {
	return map[string]*huma.Response{"200": {
		Description: desc,
		Content:     map[string]*huma.MediaType{ct: {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}},
	}}
}

// inlineImage types may be shown inline; everything else is downloaded.
var inlineImage = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/bmp": true, "image/avif": true,
}

func (d *deps) registerContent(a huma.API) {
	errs := []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusServiceUnavailable}

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "getMessageHtml", Method: http.MethodGet, Path: "/messages/{id}/html", Tags: []string{"messages"},
		Summary: "Sanitised HTML of a message",
		Description: "The text/html part with scripts, event handlers, active URLs, forms, frames and @import removed; " +
			"cid: images point to /attachments/{id}?inline=1; remote images are left in. A message that has only " +
			"text is returned as a preformatted block. When the body is not in the mirror yet (bodyState headers or " +
			"deferred) it is downloaded from the mail server first, waiting up to 10 s; then 503 content_not_available " +
			"with Retry-After. The response is immutable and cacheable. It carries a Content-Security-Policy.",
		Responses: streamOf("text/html", "HTML document fragment"), Errors: errs,
	}), func(ctx context.Context, in *messageIDInput) (*huma.StreamResponse, error) {
		id, ok := parseID("m_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		h, err := d.view.HTML(ctx, sessionFrom(ctx).UserID, id, func(att int64) string {
			return Prefix + "/attachments/" + formatID("a_", att) + "?inline=1"
		})
		if err != nil {
			return nil, viewError(err)
		}
		return &huma.StreamResponse{Body: func(c huma.Context) {
			c.SetHeader("Content-Type", "text/html; charset=utf-8")
			c.SetHeader("Content-Security-Policy", htmlCSP)
			c.SetHeader("Cache-Control", "private, max-age=31536000, immutable")
			c.SetHeader("ETag", h.ETag)
			c.SetHeader("X-Content-Type-Options", "nosniff")
			c.SetHeader("Referrer-Policy", "no-referrer")
			c.SetStatus(http.StatusOK)
			_, _ = c.BodyWriter().Write(h.Body)
		}}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "getMessageRaw", Method: http.MethodGet, Path: "/messages/{id}/raw", Tags: []string{"messages"},
		Summary:     "Original message (.eml)",
		Description: "The message exactly as the mail server delivered it (message/rfc822), downloaded first when it is not in the mirror yet.",
		Responses:   streamOf("message/rfc822", "RFC 822 message"), Errors: errs,
	}), func(ctx context.Context, in *messageIDInput) (*huma.StreamResponse, error) {
		id, ok := parseID("m_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		raw, err := d.view.OpenRaw(ctx, sessionFrom(ctx).UserID, id)
		if err != nil {
			return nil, viewError(err)
		}
		return &huma.StreamResponse{Body: func(c huma.Context) {
			defer raw.Body.Close()
			c.SetHeader("Content-Type", "message/rfc822")
			c.SetHeader("Content-Disposition", dispositionOf("attachment", emlName(raw.Subject)))
			c.SetHeader("Cache-Control", "private, max-age=31536000, immutable")
			c.SetHeader("ETag", `"`+raw.SHA+`"`)
			c.SetHeader("X-Content-Type-Options", "nosniff")
			c.SetHeader("Content-Security-Policy", attachmentCSP)
			c.SetStatus(http.StatusOK)
			_, _ = io.Copy(c.BodyWriter(), raw.Body)
		}}, nil
	})

	huma.Register(a, d.secured(huma.Operation{
		OperationID: "getAttachment", Method: http.MethodGet, Path: "/attachments/{id}", Tags: []string{"messages"},
		Summary: "Attachment bytes",
		Description: "A download; with inline=1 images are shown in the browser. An attachment that was not downloaded " +
			"yet is fetched from the mail server together with its message (same wait and 503 as /messages/{id}/html).",
		Responses: streamOf("application/octet-stream", "The attachment, with its own content type"), Errors: errs,
	}), func(ctx context.Context, in *attachmentInput) (*huma.StreamResponse, error) {
		id, ok := parseID("a_", in.ID)
		if !ok {
			return nil, errNotFound()
		}
		att, err := d.view.OpenAttachment(ctx, sessionFrom(ctx).UserID, id)
		if err != nil {
			return nil, viewError(err)
		}
		ct, _, perr := mime.ParseMediaType(att.ContentType)
		if perr != nil {
			ct = "application/octet-stream"
		}
		disp := "attachment"
		if in.Inline && inlineImage[ct] {
			disp = "inline"
		}
		return &huma.StreamResponse{Body: func(c huma.Context) {
			defer att.Body.Close()
			c.SetHeader("Content-Type", ct)
			c.SetHeader("Content-Disposition", dispositionOf(disp, att.Filename))
			c.SetHeader("Cache-Control", "private, max-age=31536000, immutable")
			c.SetHeader("ETag", `"`+att.SHA+`"`)
			c.SetHeader("X-Content-Type-Options", "nosniff")
			c.SetHeader("Content-Security-Policy", attachmentCSP)
			c.SetStatus(http.StatusOK)
			_, _ = io.Copy(c.BodyWriter(), att.Body)
		}}, nil
	})
}

// dispositionOf formats a Content-Disposition header; a name that is not
// ASCII goes in the RFC 5987 filename* form.
func dispositionOf(kind, name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if name == "" {
		return kind
	}
	if s := mime.FormatMediaType(kind, map[string]string{"filename": name}); s != "" {
		return s
	}
	return kind
}

func emlName(subject string) string {
	s := strings.TrimSpace(subject)
	if s == "" {
		s = "message"
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s + ".eml"
}
