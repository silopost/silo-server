package mailview

import (
	"strings"
	"testing"
)

func cidTo(cid string) string {
	if cid == "logo" {
		return "/api/v1/attachments/a_1?inline=1"
	}
	return ""
}

func TestSanitizeHTMLRemovesActiveContent(t *testing.T) {
	cases := map[string]string{
		"script":         `a<script>alert(1)</script>b`,
		"script upper":   `a<SCRIPT SRC=//x></SCRIPT>b`,
		"script split":   `<scr<script>ipt>alert(1)</scr</script>ipt>`,
		"unclosed":       `a<script>alert(1)`,
		"onerror":        `<img src=x onerror=alert(1)>`,
		"onerror quoted": `<img src="x" OnError="alert(1)">`,
		"slash handler":  `<img/src=x/onerror=alert(1)>`,
		"svg":            `<svg onload=alert(1)><script>alert(1)</script></svg>`,
		"javascript":     `<a href="javascript:alert(1)">x</a>`,
		"javascript ws":  "<a href=\" jav&#x09;ascript:alert(1)\">x</a>",
		"javascript nl":  "<a href=\"java\nscript:alert(1)\">x</a>",
		"vbscript":       `<a href="vbscript:msgbox(1)">x</a>`,
		"data html":      `<a href="data:text/html,<script>alert(1)</script>">x</a>`,
		"img js":         `<img src="javascript:alert(1)">`,
		"form":           `<form action="http://evil"><input name=a><button>go</button></form>`,
		"iframe":         `<iframe src="http://evil"></iframe>`,
		"object":         `<object data="x"><embed src="x"></object>`,
		"meta":           `<meta http-equiv="refresh" content="0;url=http://evil">`,
		"base":           `<base href="http://evil/">`,
		"link":           `<link rel="stylesheet" href="http://evil/x.css">`,
		"import":         `<style>@import url(http://evil/x.css); p{color:red}</style>`,
		"import escaped": `<style>@\69mport 'http://evil/x.css';</style>`,
		"expression":     `<p style="width:expression(alert(1))">x</p>`,
		"css js url":     `<p style="background:url(javascript:alert(1))">x</p>`,
		"css comment":    `<p style="background:url(java/**/script:alert(1))">x</p>`,
		"css entity":     `<p style="background:url(&#106;avascript:alert(1))">x</p>`,
		"binding":        `<p style="-moz-binding:url(http://evil/x.xml#y)">x</p>`,
		"comment":        `<!--[if IE]><script>alert(1)</script><![endif]-->`,
		"cdata":          `<![CDATA[<script>alert(1)</script>]]>`,
		"style break":    `<style>p{} </style><script>alert(1)</script>`,
		"textarea":       `<textarea><script>alert(1)</script></textarea>`,
		"title":          `<title></title><script>alert(1)</script>`,
		"null byte":      "<scr\x00ipt>alert(1)</script>",
		"attr break":     `<a title="x" onclick="alert(1)" href="https://ok">x</a>`,
		"unquoted":       `<a href=https://ok onmouseover=alert(1)>x</a>`,
	}
	for name, in := range cases {
		out := strings.ToLower(SanitizeHTML(in, cidTo))
		for _, bad := range []string{"<script", "onerror", "onload", "onclick", "onmouseover", "javascript", "vbscript", "<form", "<input",
			"<iframe", "<object", "<embed", "<meta", "<base", "<link", "@import", "expression", "-moz-binding", "data:text"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: output contains %q: %s", name, bad, out)
			}
		}
		// Text between tags is inert; only markup and attributes matter for "script split".
		if name != "script split" && strings.Contains(out, "alert(") {
			t.Errorf("%s: output contains alert(: %s", name, out)
		}
		switch name {
		case "import", "meta", "link", "base", "iframe", "form", "css js url", "binding":
			if strings.Contains(out, "evil") {
				t.Errorf("%s: output contains the evil host: %s", name, out)
			}
		}
	}
}

func TestSanitizeHTMLKeepsContent(t *testing.T) {
	in := `<div class="a" style="color: red; background:url(cid:logo)"><p>Hi <b>there</b> &amp; <a href="https://ok.example/x?a=1&b=2">link</a></p>` +
		`<img src="cid:logo" alt="Logo" width="10"><img src="cid:unknown"><img src="https://remote.example/p.png">` +
		`<img src="data:image/png;base64,AAAA"><a href="mailto:a@b.c">mail</a><table><tr><td colspan="2">x</td></tr></table>1 < 2</div>`
	out := SanitizeHTML(in, cidTo)
	for _, want := range []string{
		`<div class="a" style="color: red; background:url(&#34;/api/v1/attachments/a_1?inline=1&#34;)">`, "<b>there</b>", "&amp;",
		`href="https://ok.example/x?a=1&amp;b=2"`, `target="_blank" rel="noopener noreferrer"`,
		`<img src="/api/v1/attachments/a_1?inline=1" alt="Logo" width="10">`, `src="https://remote.example/p.png"`,
		`src="data:image/png;base64,AAAA"`, `href="mailto:a@b.c"`, `<td colspan="2">x</td>`, "1 &lt; 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cid:unknown") || strings.Contains(out, `<img src="cid`) {
		t.Errorf("unresolved cid kept: %s", out)
	}
}

func TestSanitizeCSS(t *testing.T) {
	got := sanitizeCSS("p{color:red;background:url('https://x.example/a.png')} @import 'x'; q{width:expression(1)}", nil)
	for _, bad := range []string{"@import", "expression"} {
		if strings.Contains(got, bad) {
			t.Errorf("css still has %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, `url("https://x.example/a.png")`) || !strings.Contains(got, "color:red") {
		t.Errorf("css lost content: %s", got)
	}
}
