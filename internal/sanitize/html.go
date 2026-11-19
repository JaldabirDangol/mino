// Package sanitize cleans rich-text HTML against a strict allowlist.
package sanitize

import (
	"bytes"
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// allowed maps permitted elements to their permitted attributes.
var allowed = map[string][]string{
	"p": nil, "br": nil, "hr": nil,
	"h1": nil, "h2": nil, "h3": nil, "h4": nil, "h5": nil, "h6": nil,
	"strong": nil, "b": nil, "em": nil, "i": nil, "u": nil, "s": nil, "del": nil, "mark": nil, "sub": nil, "sup": nil,
	"code": nil, "pre": nil, "blockquote": nil,
	"ul": nil, "ol": {"start"}, "li": nil,
	"a":     {"href", "title", "target"},
	"img":   {"src", "alt", "title", "width", "height"},
	"table": nil, "thead": nil, "tbody": nil, "tr": nil, "th": {"colspan", "rowspan"}, "td": {"colspan", "rowspan"},
	"span": nil,
}

var voidElements = map[string]bool{"br": true, "hr": true, "img": true}

// dropContent lists elements whose text content must be discarded entirely.
var dropContent = map[string]bool{"script": true, "style": true, "iframe": true, "object": true, "embed": true, "noscript": true, "template": true, "svg": true, "math": true}

// HTML returns sanitized markup. Unknown elements are removed but their text is kept;
// dangerous elements are removed with their content. URLs must be http(s), mailto, tel or relative.
func HTML(in string) string {
	z := html.NewTokenizer(strings.NewReader(in))
	var buf bytes.Buffer
	var stack []string
	skipDepth := 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if z.Err() != io.EOF {
				return ""
			}
			for i := len(stack) - 1; i >= 0; i-- {
				buf.WriteString("</" + stack[i] + ">")
			}
			return buf.String()
		case html.TextToken:
			if skipDepth == 0 {
				buf.WriteString(html.EscapeString(string(z.Text())))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			name := tok.Data
			if dropContent[name] {
				if tt == html.StartTagToken {
					skipDepth++
				}
				continue
			}
			if skipDepth > 0 {
				continue
			}
			attrs, ok := allowed[name]
			if !ok {
				continue
			}
			buf.WriteString("<" + name)
			blankTarget := false
			for _, a := range tok.Attr {
				if !contains(attrs, a.Key) {
					continue
				}
				val := a.Val
				if a.Key == "href" || a.Key == "src" {
					if !safeURL(val) {
						continue
					}
				}
				if a.Key == "target" {
					if val != "_blank" {
						continue
					}
					blankTarget = true
				}
				buf.WriteString(" " + a.Key + `="` + html.EscapeString(val) + `"`)
			}
			if blankTarget {
				buf.WriteString(` rel="noopener noreferrer"`)
			}
			buf.WriteString(">")
			if !voidElements[name] && tt == html.StartTagToken {
				stack = append(stack, name)
			}
		case html.EndTagToken:
			tok := z.Token()
			name := tok.Data
			if dropContent[name] {
				if skipDepth > 0 {
					skipDepth--
				}
				continue
			}
			if skipDepth > 0 || voidElements[name] {
				continue
			}
			// Close up to the matching open element so output is always well-formed.
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i] == name {
					for j := len(stack) - 1; j >= i; j-- {
						buf.WriteString("</" + stack[j] + ">")
					}
					stack = stack[:i]
					break
				}
			}
		}
	}
}

// IsEmpty reports whether sanitized HTML contains no visible text or media.
func IsEmpty(s string) bool {
	if strings.Contains(s, "<img") || strings.Contains(s, "<hr") {
		return false
	}
	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return true
		case html.TextToken:
			if strings.TrimSpace(html.UnescapeString(string(z.Text()))) != "" {
				return false
			}
		}
	}
}

func safeURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "", "http", "https", "mailto", "tel":
		// Reject scheme-less values that browsers would treat as protocol-relative to another host
		// only when they are malformed; "//cdn.example.com/x.png" is acceptable.
		return !strings.ContainsAny(raw, "\x00\n\r\t")
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
