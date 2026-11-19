package sanitize

import "testing"

func TestHTML(t *testing.T) {
	cases := map[string]string{
		`<p>Hello <strong>world</strong></p>`:                   `<p>Hello <strong>world</strong></p>`,
		`<p onclick="evil()">x</p>`:                             `<p>x</p>`,
		`<script>alert(1)</script><p>ok</p>`:                    `<p>ok</p>`,
		`<a href="javascript:alert(1)">x</a>`:                   `<a>x</a>`,
		`<a href="https://e.com" target="_blank">x</a>`:         `<a href="https://e.com" target="_blank" rel="noopener noreferrer">x</a>`,
		`<a href="/rel" target="_top" rel="opener">x</a>`:       `<a href="/rel">x</a>`,
		`<img src="data:image/png;base64,xx" onerror="x">`:      `<img>`,
		`<img src="/uploads/a.png" alt="A">`:                    `<img src="/uploads/a.png" alt="A">`,
		`<div><p>keep text</p></div>`:                           `<p>keep text</p>`,
		`<p>unclosed <em>em`:                                    `<p>unclosed <em>em</em></p>`,
		`<style>p{}</style><iframe src="x">inner</iframe>after`: `after`,
		`<p>&lt;b&gt; stays text</p>`:                           `<p>&lt;b&gt; stays text</p>`,
		`<svg><script>alert(1)</script></svg><p>z</p>`:          `<p>z</p>`,
		`<p title="&quot; onmouseover=&quot;x">t</p>`:           `<p>t</p>`,
		`<ol start="3"><li>three</li></ol>`:                     `<ol start="3"><li>three</li></ol>`,
	}
	for in, want := range cases {
		if got := HTML(in); got != want {
			t.Errorf("HTML(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestIsEmpty(t *testing.T) {
	for in, want := range map[string]bool{
		"":                     true,
		"<p></p>":              true,
		"<p> &nbsp; </p>":      true, // only whitespace
		"<p>x</p>":             false,
		`<p><img src="a"></p>`: false,
	} {
		if got := IsEmpty(in); got != want {
			t.Errorf("IsEmpty(%q) = %v, want %v", in, got, want)
		}
	}
}
