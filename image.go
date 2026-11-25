// Copyright (c) 2026, Gravwell, Inc.
// All rights reserved.
//
// Use of this source code is governed by a BSD 2-Clause license that can be
// found in the LICENSE file.

package goecharts

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"net/url"
	"strings"

	// Formats that image.DecodeConfig can read the size of.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/dop251/goja"
)

// hostImageSize(src) resolves an image source and returns its pixel size as
// {width, height}. A data: URL is decoded in place; anything else goes
// through the image loader (WithImageLoader). Plain options reach this too,
// so the script Loader stands in for a missing image loader only while a
// script renders, when it can reach that Loader anyway. Only the header is
// decoded, so the cost is the fetch.
func (rt *jsRuntime) hostImageSize(call goja.FunctionCall) goja.Value {
	src := call.Argument(0).String()
	var data []byte
	if strings.HasPrefix(src, "data:") {
		comma := strings.IndexByte(src, ',')
		if comma < 0 {
			panic(rt.vm.NewGoError(fmt.Errorf("malformed data URL")))
		}
		meta, payload := src[5:comma], src[comma+1:]
		var err error
		if strings.HasSuffix(meta, ";base64") {
			data, err = base64.StdEncoding.DecodeString(payload)
		} else {
			// The payload should be percent-encoded, but hand-written SVG
			// often carries a bare '%' (width:100%); read that as it is.
			if text, uerr := url.PathUnescape(payload); uerr == nil {
				data = []byte(text)
			} else {
				data = []byte(payload)
			}
		}
		if err != nil {
			panic(rt.vm.NewGoError(fmt.Errorf("decoding data URL: %w", err)))
		}
	} else {
		loader := rt.r.opts.imageLoader
		if loader == nil && rt.script {
			loader = rt.r.opts.loader
		}
		if loader == nil {
			panic(rt.vm.NewGoError(fmt.Errorf("image loading is disabled (no image loader configured): %s", src)))
		}
		var err error
		if data, err = rt.fetch(loader, src); err != nil {
			panic(rt.vm.NewGoError(err))
		}
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		if w, h, ok := svgSize(data); ok {
			return rt.sizeObject(w, h)
		}
		panic(rt.vm.NewGoError(fmt.Errorf("decoding image %s: %w", src, err)))
	}
	return rt.sizeObject(cfg.Width, cfg.Height)
}

func (rt *jsRuntime) sizeObject(w, h int) goja.Value {
	o := rt.vm.NewObject()
	_ = o.Set("width", w)
	_ = o.Set("height", h)
	return o
}

// svgSize reads the width and height attributes of an SVG document's root
// element, falling back to its viewBox.
func svgSize(data []byte) (w, h int, ok bool) {
	root, err := parseXML(string(data))
	if err != nil {
		return 0, 0, false
	}
	for _, c := range root.children {
		if c.typ != "element" || !strings.EqualFold(c.name, "svg") {
			continue
		}
		attr := func(name string) string {
			for _, a := range c.attrs {
				if a[0] == name {
					return a[1]
				}
			}
			return ""
		}
		var fw, fh float64
		if _, err := fmt.Sscanf(attr("width"), "%g", &fw); err == nil {
			if _, err := fmt.Sscanf(attr("height"), "%g", &fh); err == nil && fw > 0 && fh > 0 {
				return int(fw + 0.5), int(fh + 0.5), true
			}
		}
		var x, y float64
		if n, _ := fmt.Sscanf(strings.ReplaceAll(attr("viewBox"), ",", " "), "%g %g %g %g", &x, &y, &fw, &fh); n == 4 && fw > 0 && fh > 0 {
			return int(fw + 0.5), int(fh + 0.5), true
		}
		return 0, 0, false
	}
	return 0, 0, false
}
