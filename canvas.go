// Copyright (c) 2026, Gravwell, Inc.
// All rights reserved.
//
// Use of this source code is governed by a BSD 2-Clause license that can be
// found in the LICENSE file.

package goecharts

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"

	"github.com/dop251/goja"
)

// ErrCanvasTooLarge reports that a render asked for more canvas pixels than
// the limit set with WithMaxCanvasPixels.
var ErrCanvasTooLarge = errors.New("echarts: canvas exceeds the pixel limit")

// pixelCanvas is the pixel store behind a canvas element. It implements the
// part of the 2D canvas that produces pixels ECharts reads back or embeds:
// filled circles and rectangles with an optional blurred shadow, drawing one
// canvas onto another, and reading, writing and exporting the pixels. That
// is what the heatmap layer on a geo coordinate system needs. Text, general
// paths, transforms, gradients and strokes are not drawn.
//
// Pixels are 8-bit RGBA with straight (not premultiplied) alpha, the layout
// of ImageData.
type pixelCanvas struct {
	w, h int
	// pix is nil until something is drawn after the canvas was sized; an
	// unallocated canvas reads as transparent.
	pix []uint8

	// maxScratch limits the pixels of the working mask one paint may use;
	// zero means no limit.
	maxScratch int64
	// cancelled reports whether the render has ended, so that long drawing
	// operations stop early; nil means never.
	cancelled func() bool
}

// errCancelled reports that a drawing operation stopped because the render
// ended.
var errCancelled = errors.New("canvas operation cancelled")

func (c *pixelCanvas) stopped() bool { return c.cancelled != nil && c.cancelled() }

// rgba is a colour with every channel in 0..1 and straight alpha.
type rgba struct{ r, g, b, a float64 }

// shape is one closed subpath: a circle when r > 0, else a rectangle.
type shape struct {
	x, y, r float64 // circle centre and radius
	w, h    float64 // rectangle size, with x, y its top left corner
}

func (c *pixelCanvas) clear(x0, y0, x1, y1 int) {
	x0, y0, x1, y1 = max(x0, 0), max(y0, 0), min(x1, c.w), min(y1, c.h)
	if c.pix == nil || x0 >= x1 {
		return
	}
	for y := y0; y < y1; y++ {
		clear(c.pix[(y*c.w+x0)*4 : (y*c.w+x1)*4])
	}
}

// blend composites colour col with coverage cov over the pixel at i.
func (c *pixelCanvas) blend(i int, col rgba, cov float64) {
	sa := col.a * cov
	if sa <= 0 {
		return
	}
	p := c.pix[i : i+4 : i+4]
	da := float64(p[3]) / 255
	oa := sa + da*(1-sa)
	if oa <= 0 {
		return
	}
	mix := func(s float64, d uint8) uint8 {
		return uint8(math.Round((s*255*sa + float64(d)*da*(1-sa)) / oa))
	}
	p[0], p[1], p[2] = mix(col.r, p[0]), mix(col.g, p[1]), mix(col.b, p[2])
	p[3] = uint8(math.Round(oa * 255))
}

// coverage returns how much of the pixel at (px, py) the shape covers,
// antialiased at the edge.
func (s shape) coverage(px, py int) float64 {
	if s.r > 0 {
		d := math.Hypot(float64(px)+0.5-s.x, float64(py)+0.5-s.y)
		return clamp01(s.r + 0.5 - d)
	}
	overlap := func(lo, size float64, p int) float64 {
		return clamp01(math.Min(lo+size, float64(p+1)) - math.Max(lo, float64(p)))
	}
	return overlap(s.x, s.w, px) * overlap(s.y, s.h, py)
}

// bounds returns the shape's bounding box.
func (s shape) bounds() (x0, y0, x1, y1 float64) {
	if s.r > 0 {
		return s.x - s.r, s.y - s.r, s.x + s.r, s.y + s.r
	}
	return s.x, s.y, s.x + s.w, s.y + s.h
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// fill paints the shapes with fill, after their shadow if there is one.
// alpha is the context's globalAlpha.
func (c *pixelCanvas) fill(shapes []shape, fill, shadow rgba, blur, offX, offY, alpha float64) error {
	if len(shapes) == 0 || c.w == 0 || c.h == 0 {
		return nil
	}
	if shadow.a > 0 && (blur > 0 || offX != 0 || offY != 0) {
		moved := make([]shape, len(shapes))
		for i, s := range shapes {
			s.x, s.y = s.x+offX, s.y+offY
			moved[i] = s
		}
		// The blur of a canvas shadow is a Gaussian whose standard
		// deviation is half of shadowBlur.
		if err := c.paint(moved, shadow, alpha, max(blur, 0)/2); err != nil {
			return err
		}
	}
	return c.paint(shapes, fill, alpha, 0)
}

// paint composites the union of shapes in colour col, blurred by a Gaussian
// of standard deviation sigma when that is positive.
func (c *pixelCanvas) paint(shapes []shape, col rgba, alpha, sigma float64) error {
	if col.a*alpha <= 0 {
		return nil
	}
	// The kernel reaches three standard deviations. No canvas pixel is
	// further than the canvas size from the region it paints, so a longer
	// kernel only adds work; gaussianBlur normalizes a cut-off kernel as if
	// it were whole, so the result stays a wide, faint shadow.
	reach := int(math.Min(math.Ceil(3*sigma), float64(max(c.w, c.h))))
	bx0, by0, bx1, by1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, s := range shapes {
		x0, y0, x1, y1 := s.bounds()
		bx0, by0, bx1, by1 = math.Min(bx0, x0), math.Min(by0, y0), math.Max(bx1, x1), math.Max(by1, y1)
	}
	// The region of the canvas the paint can touch, and the region of the
	// mask that can contribute to it (reach further out, for the blur).
	cx0 := max(int(math.Floor(bx0))-1-reach, 0)
	cy0 := max(int(math.Floor(by0))-1-reach, 0)
	cx1 := min(int(math.Ceil(bx1))+1+reach, c.w)
	cy1 := min(int(math.Ceil(by1))+1+reach, c.h)
	if cx0 >= cx1 || cy0 >= cy1 {
		return nil
	}
	mx0, my0 := cx0-reach, cy0-reach
	mw, mh := cx1-cx0+2*reach, cy1-cy0+2*reach
	if n := int64(mw) * int64(mh); c.maxScratch > 0 && n > c.maxScratch {
		return fmt.Errorf("%w: a shadow blur needs %d pixels, limit %d", ErrCanvasTooLarge, n, c.maxScratch)
	}
	mask := make([]float32, mw*mh)
	for _, s := range shapes {
		x0, y0, x1, y1 := s.bounds()
		for y := max(int(math.Floor(y0))-1, my0); y < min(int(math.Ceil(y1))+1, my0+mh); y++ {
			if c.stopped() {
				return errCancelled
			}
			for x := max(int(math.Floor(x0))-1, mx0); x < min(int(math.Ceil(x1))+1, mx0+mw); x++ {
				i := (y-my0)*mw + x - mx0
				mask[i] = max(mask[i], float32(s.coverage(x, y)))
			}
		}
	}
	if reach > 0 {
		if !gaussianBlur(mask, mw, mh, sigma, reach, c.stopped) {
			return errCancelled
		}
	}
	col.a *= alpha
	for y := cy0; y < cy1; y++ {
		for x := cx0; x < cx1; x++ {
			if cov := mask[(y-my0)*mw+x-mx0]; cov > 0 {
				c.blend((y*c.w+x)*4, col, float64(cov))
			}
		}
	}
	return nil
}

// gaussianBlur blurs a w by h mask in place with a separable kernel. It
// checks stopped once per row and returns false if that ends it early.
func gaussianBlur(mask []float32, w, h int, sigma float64, reach int, stopped func() bool) bool {
	kernel := make([]float32, 2*reach+1)
	var sum float64
	for i := range kernel {
		d := float64(i - reach)
		v := math.Exp(-d * d / (2 * sigma * sigma))
		kernel[i] = float32(v)
		sum += v
	}
	if float64(reach) < 3*sigma {
		// The kernel was cut short; weight it as the whole Gaussian.
		sum = sigma * math.Sqrt(2*math.Pi)
	}
	for i := range kernel {
		kernel[i] /= float32(sum)
	}
	tmp := make([]float32, len(mask))
	for y := range h {
		if stopped() {
			return false
		}
		row := mask[y*w : (y+1)*w]
		for x := range w {
			var acc float32
			for k, kv := range kernel {
				if sx := x + k - reach; sx >= 0 && sx < w {
					acc += row[sx] * kv
				}
			}
			tmp[y*w+x] = acc
		}
	}
	for y := range h {
		if stopped() {
			return false
		}
		for x := range w {
			var acc float32
			for k, kv := range kernel {
				if sy := y + k - reach; sy >= 0 && sy < h {
					acc += tmp[sy*w+x] * kv
				}
			}
			mask[y*w+x] = acc
		}
	}
	return true
}

// draw composites src onto c with its top left corner at (dx, dy), scaled
// to dw by dh by nearest-neighbour sampling when that differs from its own
// size. Positions are rounded to whole pixels.
func (c *pixelCanvas) draw(src *pixelCanvas, dx, dy, dw, dh, alpha float64) error {
	if src.pix == nil || src.w == 0 || src.h == 0 || dw <= 0 || dh <= 0 || alpha <= 0 {
		return nil
	}
	ox, oy := int(math.Round(dx)), int(math.Round(dy))
	w, h := int(math.Round(dw)), int(math.Round(dh))
	if src == c {
		src = &pixelCanvas{w: c.w, h: c.h, pix: bytes.Clone(c.pix)}
	}
	for y := max(oy, 0); y < min(oy+h, c.h); y++ {
		if c.stopped() {
			return errCancelled
		}
		sy := (y - oy) * src.h / h
		for x := max(ox, 0); x < min(ox+w, c.w); x++ {
			sx := (x - ox) * src.w / w
			p := src.pix[(sy*src.w+sx)*4:]
			if p[3] == 0 {
				continue
			}
			col := rgba{float64(p[0]) / 255, float64(p[1]) / 255, float64(p[2]) / 255, float64(p[3]) / 255}
			c.blend((y*c.w+x)*4, col, alpha)
		}
	}
	return nil
}

// region copies the w by h rectangle at (x, y) out of the canvas; pixels
// outside the canvas are transparent.
func (c *pixelCanvas) region(x, y, w, h int) []uint8 {
	out := make([]uint8, w*h*4)
	if c.pix == nil {
		return out
	}
	for row := max(y, 0); row < min(y+h, c.h); row++ {
		x0, x1 := max(x, 0), min(x+w, c.w)
		if x0 < x1 {
			copy(out[((row-y)*w+x0-x)*4:], c.pix[(row*c.w+x0)*4:(row*c.w+x1)*4])
		}
	}
	return out
}

// put writes a w by h block of pixels at (x, y), replacing what is there.
func (c *pixelCanvas) put(data []uint8, w, h, x, y int) {
	for row := max(y, 0); row < min(y+h, c.h); row++ {
		x0, x1 := max(x, 0), min(x+w, c.w)
		if x0 < x1 {
			copy(c.pix[(row*c.w+x0)*4:(row*c.w+x1)*4], data[((row-y)*w+x0-x)*4:])
		}
	}
}

// dataURL encodes the canvas as a PNG data URL.
func (c *pixelCanvas) dataURL() (string, error) {
	img := &image.NRGBA{Pix: c.pix, Stride: c.w * 4, Rect: image.Rect(0, 0, c.w, c.h)}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// hostCanvas() creates the pixel store of one canvas and returns its
// operations as an object for the canvas shim in post.js, which keeps the
// drawing state (path, colours, globalAlpha) and passes it with each call.
func (rt *jsRuntime) hostCanvas(goja.FunctionCall) goja.Value {
	vm := rt.vm
	c := &pixelCanvas{
		maxScratch: rt.r.opts.maxCanvasPixels,
		// A canvas can outlive the render that made it, in a cache inside
		// ECharts, so read the context of whichever render is drawing.
		cancelled: func() bool { return rt.ctx != nil && rt.ctx.Err() != nil },
	}
	o := vm.NewObject()
	num := func(call goja.FunctionCall, i int) float64 {
		v := call.Argument(i).ToFloat()
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0
		}
		return v
	}
	// Coordinates far outside any canvas would overflow int arithmetic.
	coord := func(call goja.FunctionCall, i int) float64 {
		return math.Max(-1e9, math.Min(1e9, num(call, i)))
	}
	px := func(call goja.FunctionCall, i int) int { return int(math.Round(coord(call, i))) }
	colour := func(call goja.FunctionCall, i int) rgba {
		return rgba{num(call, i) / 255, num(call, i+1) / 255, num(call, i+2) / 255, clamp01(num(call, i+3))}
	}
	set := func(name string, fn func(call goja.FunctionCall) goja.Value) { _ = o.Set(name, fn) }
	// fail throws err into the script. A size limit is also recorded so
	// that the render fails with it even if the script catches the throw.
	fail := func(err error) {
		if errors.Is(err, ErrCanvasTooLarge) && rt.limitErr == nil {
			rt.limitErr = err
		}
		if errors.Is(err, errCancelled) && rt.ctx != nil && rt.ctx.Err() != nil {
			err = rt.ctx.Err()
		}
		panic(vm.NewGoError(err))
	}
	// alloc allocates the pixels of a canvas that was sized since it was
	// last drawn on. Allocation waits for a draw so that the usual
	// "width = w; height = h" pair is charged once, at its final size.
	alloc := func() {
		if c.pix != nil || c.w == 0 || c.h == 0 {
			return
		}
		if err := rt.chargeCanvas(int64(c.w) * int64(c.h)); err != nil {
			fail(err)
		}
		c.pix = make([]uint8, c.w*c.h*4)
	}

	// handle lets draw() find the pixel store of another canvas.
	_ = o.Set("handle", vm.ToValue(c))

	// resize(w, h) sets the size and clears the canvas, as assigning a
	// canvas's width or height does.
	set("resize", func(call goja.FunctionCall) goja.Value {
		c.w, c.h, c.pix = max(px(call, 0), 0), max(px(call, 1), 0), nil
		return goja.Undefined()
	})
	set("clear", func(call goja.FunctionCall) goja.Value {
		x, y := px(call, 0), px(call, 1)
		c.clear(x, y, x+px(call, 2), y+px(call, 3))
		return goja.Undefined()
	})
	// fill(shapes, r, g, b, a, sr, sg, sb, sa, blur, offX, offY, alpha):
	// shapes is a flat array of [kind, x, y, p, q] records, where kind 0 is
	// a circle of radius p and kind 1 a rectangle of size p by q.
	set("fill", func(call goja.FunctionCall) goja.Value {
		var flat []float64
		if err := vm.ExportTo(call.Argument(0), &flat); err != nil {
			panic(vm.NewGoError(err))
		}
		shapes := make([]shape, 0, len(flat)/5)
		for i := 0; i+5 <= len(flat); i += 5 {
			v := func(j int) float64 {
				f := flat[i+j]
				if math.IsNaN(f) {
					return 0
				}
				return math.Max(-1e9, math.Min(1e9, f))
			}
			s := shape{x: v(1), y: v(2)}
			if flat[i] == 0 {
				s.r = v(3)
			} else {
				s.w, s.h = v(3), v(4)
			}
			shapes = append(shapes, s)
		}
		alloc()
		if err := c.fill(shapes, colour(call, 1), colour(call, 5), coord(call, 9), coord(call, 10), coord(call, 11), clamp01(num(call, 12))); err != nil {
			fail(err)
		}
		return goja.Undefined()
	})
	// draw(handle, dx, dy, dw, dh, alpha)
	set("draw", func(call goja.FunctionCall) goja.Value {
		src, ok := call.Argument(0).Export().(*pixelCanvas)
		if !ok {
			return goja.Undefined()
		}
		alloc()
		if err := c.draw(src, coord(call, 1), coord(call, 2), coord(call, 3), coord(call, 4), clamp01(num(call, 5))); err != nil {
			fail(err)
		}
		return goja.Undefined()
	})
	// read(x, y, w, h) returns the pixels as an ArrayBuffer. The copy is
	// not counted against the render's canvas pixels, since reading a
	// canvas back whole is the normal use, but no single read may be larger
	// than the limit.
	set("read", func(call goja.FunctionCall) goja.Value {
		w, h := max(px(call, 2), 0), max(px(call, 3), 0)
		if limit := rt.r.opts.maxCanvasPixels; limit > 0 && int64(w)*int64(h) > limit {
			fail(fmt.Errorf("%w: reading %d pixels, limit %d", ErrCanvasTooLarge, int64(w)*int64(h), limit))
		}
		return vm.ToValue(vm.NewArrayBuffer(c.region(px(call, 0), px(call, 1), w, h)))
	})
	// write(buffer, w, h, x, y)
	set("write", func(call goja.FunctionCall) goja.Value {
		buf, ok := call.Argument(0).Export().(goja.ArrayBuffer)
		w, h := max(px(call, 1), 0), max(px(call, 2), 0)
		if !ok || len(buf.Bytes()) < w*h*4 {
			panic(vm.NewTypeError("putImageData: not image data"))
		}
		alloc()
		c.put(buf.Bytes(), w, h, px(call, 3), px(call, 4))
		return goja.Undefined()
	})
	set("dataURL", func(goja.FunctionCall) goja.Value {
		alloc()
		url, err := c.dataURL()
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("encoding canvas: %w", err)))
		}
		return vm.ToValue(url)
	})
	return o
}

// chargeCanvas counts n canvas pixels against the render's limit.
func (rt *jsRuntime) chargeCanvas(n int64) error {
	limit := rt.r.opts.maxCanvasPixels
	if limit <= 0 {
		return nil
	}
	if n > limit-rt.canvasPixels {
		err := fmt.Errorf("%w: %d more pixels pass the limit of %d", ErrCanvasTooLarge, n, limit)
		if rt.limitErr == nil {
			rt.limitErr = err
		}
		return err
	}
	rt.canvasPixels += n
	return nil
}
