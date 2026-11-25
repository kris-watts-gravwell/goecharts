// Copyright (c) 2026, Gravwell, Inc.
// All rights reserved.
//
// Use of this source code is governed by a BSD 2-Clause license that can be
// found in the LICENSE file.

package goecharts

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

// Values from an option must not be able to add attributes or elements to
// the SVG, whatever characters they contain.
func TestOutputEscaping(t *testing.T) {
	const option = `{
	  "title": {"text": "a<b & \"c\" ]]> \u0001 \uffff", "textStyle": {
	    "fontFamily": "\"My Font\", serif",
	    "color": "red\" onload=\"alert(1)"}},
	  "graphic": [
	    {"type": "image", "style": {"image": "https://x.test/g.png?a=1&b=\"2\"", "width": 10, "height": 10}},
	    {"type": "rect", "shape": {"width": 5, "height": 5},
	     "style": {"fill": "blue\"><script>alert(2)</script><g x=\""}},
	    {"type": "rect", "shape": {"width": 5, "height": 5},
	     "style": {"fill": "green"},
	     "emphasis": {"style": {"fill": "]]></style><script>alert(3)</script>"}}}
	  ],
	  "xAxis": {"type": "category", "data": ["a"]}, "yAxis": {},
	  "series": [{"type": "bar", "data": [1], "emphasis": {"itemStyle": {"color": "]]><script>alert(4)</script>"}}}]
	}`
	r, err := New(WithFailOnError(false))
	if err != nil {
		t.Fatal(err)
	}
	svg, err := r.RenderSVG(context.Background(), []byte(option), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Parse it as a browser would and look at what is really there.
	d := xml.NewDecoder(bytes.NewReader(svg))
	var sawFont, sawHref, sawTitle bool
	var text strings.Builder
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("SVG is not well-formed XML: %v\n%s", err, svg)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			if tok.Name.Local == "script" {
				t.Errorf("option data created a <script> element")
			}
			for _, a := range tok.Attr {
				if strings.HasPrefix(strings.ToLower(a.Name.Local), "on") {
					t.Errorf("option data created the attribute %s=%q", a.Name.Local, a.Value)
				}
				if strings.Contains(a.Value, `"My Font", serif`) {
					sawFont = true
				}
				if a.Name.Local == "href" && a.Value == `https://x.test/g.png?a=1&b="2"` {
					sawHref = true
				}
			}
		case xml.CharData:
			text.Write(tok)
		}
	}
	if strings.Contains(text.String(), `a<b & "c" ]]>`) {
		sawTitle = true
	}
	if !sawFont || !sawHref || !sawTitle {
		t.Errorf("values did not survive escaping: font %v, href %v, title %v\n%s", sawFont, sawHref, sawTitle, svg)
	}
}
