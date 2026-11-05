package mail

import (
	"strings"

	"github.com/emersion/go-imap/v2"
)

// AttachmentInfo describes one attachment part. For parts known only from
// BODYSTRUCTURE, Size is the encoded size and no blob exists yet.
type AttachmentInfo struct {
	PartPath    string // IMAP part number, e.g. "1.2"
	Filename    string
	ContentType string
	Size        int64
	ContentID   string
	Inline      bool // inline part referenced by Content-ID (e.g. a picture in HTML)
}

// AttachmentsFromStructure lists the attachment parts of a BODYSTRUCTURE
// (nil-safe). The text/plain and text/html bodies are not attachments;
// message/rfc822 parts are attachments and are not descended into.
func AttachmentsFromStructure(bs imap.BodyStructure) []AttachmentInfo {
	if bs == nil {
		return nil
	}
	var out []AttachmentInfo
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		disp := ""
		if d := sp.Disposition(); d != nil {
			disp = strings.ToLower(d.Value)
		}
		a := AttachmentInfo{
			PartPath:    joinPath(path),
			Filename:    sp.Filename(),
			ContentType: sp.MediaType(),
			Size:        int64(sp.Size),
			ContentID:   strings.Trim(sp.ID, "<> "),
		}
		a.Inline = disp != "attachment" && a.ContentID != ""
		if isAttachmentPart(a.ContentType, disp, a.Filename) {
			out = append(out, a)
		}
		return true
	})
	return out
}

func isAttachmentPart(mediaType, disposition, filename string) bool {
	switch {
	case disposition == "attachment":
		return true
	case filename != "":
		return true
	case mediaType == "text/plain" || mediaType == "text/html":
		return false
	}
	return true
}

func joinPath(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	var b strings.Builder
	for i, n := range path {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(itoa(n))
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// hasRealAttachment is true when at least one attachment is not an inline
// picture referenced by Content-ID.
func hasRealAttachment(atts []AttachmentInfo) bool {
	for _, a := range atts {
		if !a.Inline {
			return true
		}
	}
	return false
}

// firstTextPart returns the encoding and charset of the first text/plain part
// (or text/html when there is none); used to decode a partial TEXT fetch.
func firstTextPart(bs imap.BodyStructure) (encoding, charset string, html bool, ok bool) {
	if bs == nil {
		return "", "", false, false
	}
	var plain, htm *imap.BodyStructureSinglePart
	bs.Walk(func(_ []int, part imap.BodyStructure) bool {
		if sp, isSP := part.(*imap.BodyStructureSinglePart); isSP {
			switch sp.MediaType() {
			case "text/plain":
				if plain == nil && sp.Filename() == "" {
					plain = sp
				}
			case "text/html":
				if htm == nil && sp.Filename() == "" {
					htm = sp
				}
			}
		}
		return true
	})
	if plain != nil {
		return strings.ToLower(plain.Encoding), plain.Params["charset"], false, true
	}
	if htm != nil {
		return strings.ToLower(htm.Encoding), htm.Params["charset"], true, true
	}
	return "", "", false, false
}
