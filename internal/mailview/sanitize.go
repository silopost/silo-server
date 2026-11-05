package mailview

import (
	"html"
	"regexp"
	"strings"
)

// SanitizeHTML turns the HTML of a message into HTML that is safe to show in
// a sandboxed iframe. It is a whitelist rewriter: the input is tokenised and
// only known elements and attributes are written out again, re-escaped, so
// parser differences between this code and a browser cannot smuggle anything
// through. Scripts, event handlers, javascript: and other active URLs, forms,
// frames, objects, embeds, meta/base/link and @import rules are removed;
// remote images stay (an image proxy is a later decision). resolveCID maps a
// cid: reference to a URL ("" drops the attribute).
//
// The response also carries a Content-Security-Policy that forbids scripts,
// so this is a second line of defence, not the only one.
func SanitizeHTML(src string, resolveCID func(cid string) string) string {
	src = strings.ReplaceAll(src, "\x00", "")
	var out strings.Builder
	out.Grow(len(src))
	i := 0
	for i < len(src) {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			out.WriteString(src[i:])
			break
		}
		out.WriteString(src[i : i+lt])
		i += lt
		rest := src[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				return out.String()
			}
			i += 4 + end + 3
		case strings.HasPrefix(rest, "<![CDATA["):
			end := strings.Index(rest, "]]>")
			if end < 0 {
				return out.String()
			}
			i += end + 3
		case strings.HasPrefix(rest, "<!") || strings.HasPrefix(rest, "<?"):
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				return out.String()
			}
			i += end + 1
		case strings.HasPrefix(rest, "</") && len(rest) > 2 && isAlpha(rest[2]):
			j := 2
			for j < len(rest) && isNameChar(rest[j]) {
				j++
			}
			name := strings.ToLower(rest[2:j])
			end := strings.IndexByte(rest[j:], '>')
			if end < 0 {
				return out.String()
			}
			i += j + end + 1
			if allowedTags[name] && !voidTags[name] {
				out.WriteString("</" + name + ">")
			}
		case len(rest) > 1 && isAlpha(rest[1]):
			tag, n, ok := parseStartTag(rest)
			if !ok { // unterminated: nothing after it can be trusted
				return out.String()
			}
			i += n
			switch {
			case rawTextTags[tag.name]:
				// Content of script, iframe, textarea, ... is dropped with the tag.
				i += skipRawText(src[i:], tag.name)
			case tag.name == "style":
				body := src[i:]
				n := skipRawText(body, "style")
				css := body
				if end := indexFold(body, "</style"); end >= 0 {
					css = body[:end]
				}
				i += n
				out.WriteString("<style>" + sanitizeCSS(css, resolveCID) + "</style>")
			case allowedTags[tag.name]:
				writeTag(&out, tag, resolveCID)
			}
		default:
			out.WriteString("&lt;")
			i++
		}
	}
	return out.String()
}

type attr struct{ name, value string }

type startTag struct {
	name  string
	attrs []attr
}

func isAlpha(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

func isNameChar(b byte) bool {
	return isAlpha(b) || b >= '0' && b <= '9' || b == '-' || b == ':' || b == '_'
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' }

// parseStartTag reads "<name attr=value ...>" at the start of s and returns
// the tag and the number of bytes consumed. It follows the HTML tokenizer for
// quoting, so a '>' inside a quoted value does not end the tag.
func parseStartTag(s string) (startTag, int, bool) {
	j := 1
	for j < len(s) && !isSpace(s[j]) && s[j] != '/' && s[j] != '>' {
		j++
	}
	t := startTag{name: strings.ToLower(s[1:j])}
	for {
		for j < len(s) && (isSpace(s[j]) || s[j] == '/') {
			j++
		}
		if j >= len(s) {
			return t, 0, false
		}
		if s[j] == '>' {
			return t, j + 1, true
		}
		k := j
		for k < len(s) && !isSpace(s[k]) && s[k] != '=' && s[k] != '>' && s[k] != '/' {
			k++
		}
		if k == j { // stray '=' etc.
			k = j + 1
		}
		a := attr{name: strings.ToLower(s[j:k])}
		j = k
		for j < len(s) && isSpace(s[j]) {
			j++
		}
		if j < len(s) && s[j] == '=' {
			j++
			for j < len(s) && isSpace(s[j]) {
				j++
			}
			switch {
			case j >= len(s):
				return t, 0, false
			case s[j] == '"' || s[j] == '\'':
				q := s[j]
				end := strings.IndexByte(s[j+1:], q)
				if end < 0 {
					return t, 0, false
				}
				a.value = s[j+1 : j+1+end]
				j += end + 2
			default:
				k := j
				for k < len(s) && !isSpace(s[k]) && s[k] != '>' {
					k++
				}
				a.value = s[j:k]
				j = k
			}
		}
		t.attrs = append(t.attrs, a)
	}
}

// skipRawText returns the length of the raw text of an element up to and
// including its end tag (everything when there is none).
func skipRawText(s, name string) int {
	end := indexFold(s, "</"+name)
	if end < 0 {
		return len(s)
	}
	gt := strings.IndexByte(s[end:], '>')
	if gt < 0 {
		return len(s)
	}
	return end + gt + 1
}

func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), sub)
}

var allowedTags = toSet(
	"a", "abbr", "acronym", "address", "article", "aside", "b", "bdi", "bdo", "big", "blockquote", "br", "caption",
	"center", "cite", "code", "col", "colgroup", "dd", "del", "details", "dfn", "div", "dl", "dt", "em", "figcaption",
	"figure", "font", "footer", "h1", "h2", "h3", "h4", "h5", "h6", "header", "hr", "i", "img", "ins", "kbd", "li",
	"main", "mark", "nav", "ol", "p", "pre", "q", "s", "samp", "section", "small", "span", "strike", "strong", "sub",
	"summary", "sup", "table", "tbody", "td", "tfoot", "th", "thead", "tr", "tt", "u", "ul", "var", "wbr",
)

var voidTags = toSet("br", "hr", "img", "col", "wbr")

// rawTextTags are dropped together with their content.
var rawTextTags = toSet("script", "iframe", "frameset", "object", "applet", "textarea", "title",
	"noscript", "noembed", "noframes", "xmp", "plaintext", "template", "svg", "math")

func toSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// allowedAttrs are the attributes kept on every element; href, src and style
// get special handling.
var allowedAttrs = toSet("class", "title", "dir", "lang", "align", "valign", "bgcolor", "color", "width", "height",
	"border", "cellpadding", "cellspacing", "colspan", "rowspan", "face", "size", "nowrap", "alt", "start", "type",
	"abbr", "scope", "span", "cite", "datetime", "open", "hspace", "vspace", "clear", "noshade", "role")

func writeTag(out *strings.Builder, t startTag, resolveCID func(string) string) {
	out.WriteByte('<')
	out.WriteString(t.name)
	seen := map[string]bool{}
	for _, a := range t.attrs {
		if seen[a.name] || strings.HasPrefix(a.name, "on") {
			continue
		}
		val := html.UnescapeString(a.value)
		switch {
		case a.name == "style":
			val = sanitizeCSS(val, resolveCID)
			if strings.TrimSpace(val) == "" {
				continue
			}
		case a.name == "href" && t.name == "a":
			u, ok := safeURL(val, false, nil)
			if !ok {
				continue
			}
			val = u
		case a.name == "src" && t.name == "img":
			u, ok := safeURL(val, true, resolveCID)
			if !ok {
				continue
			}
			val = u
		case allowedAttrs[a.name]:
		default:
			continue
		}
		seen[a.name] = true
		out.WriteString(" " + a.name + `="` + html.EscapeString(val) + `"`)
	}
	if t.name == "a" {
		out.WriteString(` target="_blank" rel="noopener noreferrer"`)
	}
	out.WriteByte('>')
}

// safeURL checks a URL attribute. Links may go to http, https, mailto, tel and
// in-page anchors; images to http, https, cid: (resolved) and raster data
// URLs. Whitespace and control characters, which browsers ignore inside a
// scheme, are removed before the check.
func safeURL(raw string, image bool, resolveCID func(string) string) (string, bool) {
	u := strings.TrimSpace(raw)
	probe := strings.Map(func(r rune) rune {
		if r <= 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToLower(u))
	switch {
	case probe == "":
		return "", false
	case strings.HasPrefix(probe, "http://"), strings.HasPrefix(probe, "https://"):
		return u, true
	case image && strings.HasPrefix(probe, "cid:"):
		if resolveCID == nil {
			return "", false
		}
		r := resolveCID(strings.Trim(strings.TrimSpace(u)[4:], "<> "))
		return r, r != ""
	case image && rasterData.MatchString(probe):
		return strings.Map(func(r rune) rune {
			if r <= 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, u), true
	case !image && (strings.HasPrefix(probe, "mailto:") || strings.HasPrefix(probe, "tel:") || strings.HasPrefix(probe, "#")):
		return u, true
	}
	return "", false
}

var rasterData = regexp.MustCompile(`^data:image/(png|gif|jpe?g|webp|bmp|avif);base64,[a-z0-9+/=]*$`)

var (
	reCSSComment = regexp.MustCompile(`(?s)/\*.*?(\*/|$)`)
	reImport     = regexp.MustCompile(`(?i)@import[^;{}]*;?`)
	reBadCSS     = regexp.MustCompile(`(?i)[^;{}]*(expression\s*\(|behaviou?r\s*:|-moz-binding\s*:|javascript\s*:|vbscript\s*:)[^;}]*`)
	reURLFunc    = regexp.MustCompile(`(?i)url\s*\(\s*("[^"]*"|'[^']*'|[^)]*)\)`)
)

// sanitizeCSS removes the constructs of a stylesheet or style attribute that
// load code or foreign resources other than images: @import, expression(),
// behavior, -moz-binding, javascript: and url() to anything but http(s),
// cid: and raster data URLs. CSS escapes are dropped, since they can spell
// any of those.
func sanitizeCSS(css string, resolveCID func(string) string) string {
	css = strings.ReplaceAll(css, "\x00", "")
	css = reCSSComment.ReplaceAllString(css, "")
	css = strings.ReplaceAll(css, `\`, "")
	css = html.UnescapeString(css)
	css = strings.NewReplacer("<", "", ">", "").Replace(css)
	css = reImport.ReplaceAllString(css, "")
	css = reBadCSS.ReplaceAllString(css, "")
	css = reURLFunc.ReplaceAllStringFunc(css, func(m string) string {
		arg := reURLFunc.FindStringSubmatch(m)[1]
		arg = strings.TrimSpace(strings.Trim(strings.TrimSpace(arg), `"'`))
		u, ok := safeURL(arg, true, resolveCID)
		if !ok || strings.ContainsAny(u, `"'()<>`+"\n\r") {
			return "none"
		}
		return `url("` + u + `")`
	})
	return css
}
