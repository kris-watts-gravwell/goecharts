// Copyright (c) 2026, Gravwell, Inc.
// All rights reserved.
//
// Use of this source code is governed by a BSD 2-Clause license that can be
// found in the LICENSE file.

package goecharts

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newPixelCanvas(w, h int) *pixelCanvas {
	return &pixelCanvas{w: w, h: h, pix: make([]uint8, w*h*4)}
}

func (c *pixelCanvas) at(x, y int) [4]uint8 {
	return [4]uint8(c.pix[(y*c.w+x)*4:])
}

// The heatmap brush: a circle drawn off-canvas whose blurred shadow lands in
// the middle. The result is a soft dot, densest at the centre, symmetric,
// and fading to nothing at the edge.
func TestCanvasShadowBrush(t *testing.T) {
	const r, blur = 20.0, 30.0
	e := r + blur
	c := newPixelCanvas(int(2*e), int(2*e))
	black := rgba{0, 0, 0, 1}
	c.fill([]shape{{x: -e, y: e, r: r}}, black, black, blur, 2*e, 0, 1)

	mid := int(e)
	// A disc of radius r blurred by a Gaussian of deviation s has the value
	// 1 - exp(-r*r / (2*s*s)) at its centre; s is half of shadowBlur.
	centre := c.at(mid, mid)[3]
	want := 255 * (1 - math.Exp(-r*r/(2*(blur/2)*(blur/2))))
	if math.Abs(float64(centre)-want) > 3 {
		t.Errorf("alpha at the centre is %d, want %.0f", centre, want)
	}
	if edge := c.at(0, mid)[3]; edge > 8 {
		t.Errorf("alpha at the edge is %d, want nearly transparent", edge)
	}
	prev := centre
	for d := 1; d < mid; d++ {
		a := c.at(mid+d, mid)[3]
		if a > prev {
			t.Fatalf("alpha rises from %d to %d at distance %d", prev, a, d)
		}
		prev = a
		for _, o := range [][4]uint8{c.at(mid-d-1, mid), c.at(mid, mid+d), c.at(mid, mid-d-1)} {
			if diff := int(o[3]) - int(a); diff < -3 || diff > 3 {
				t.Fatalf("not symmetric at distance %d: %d and %d", d, a, o[3])
			}
		}
	}
	// The circle itself is off the canvas, so nothing but its shadow is
	// drawn: no pixel is fully opaque.
	for i := 3; i < len(c.pix); i += 4 {
		if c.pix[i] > centre {
			t.Fatalf("pixel %d has alpha %d, above the centre's %d", i/4, c.pix[i], centre)
		}
	}
}

func TestCanvasFillAndClear(t *testing.T) {
	c := newPixelCanvas(10, 10)
	red := rgba{1, 0, 0, 1}
	c.fill([]shape{{x: 2, y: 2, w: 4, h: 4}}, red, rgba{}, 0, 0, 0, 0.5)
	if got := c.at(3, 3); got != [4]uint8{255, 0, 0, 128} {
		t.Errorf("filled pixel %v", got)
	}
	if got := c.at(7, 7); got != [4]uint8{} {
		t.Errorf("pixel outside the rectangle %v", got)
	}
	// A rectangle on a half pixel covers half of the pixels along its edge.
	c.fill([]shape{{x: 0, y: 8.5, w: 10, h: 1.5}}, red, rgba{}, 0, 0, 0, 1)
	if got := c.at(0, 8)[3]; got != 128 {
		t.Errorf("half-covered pixel alpha %d", got)
	}
	c.clear(0, 0, 4, 4)
	if got := c.at(3, 3); got != [4]uint8{} {
		t.Errorf("cleared pixel %v", got)
	}
	if got := c.at(4, 4)[3]; got == 0 {
		t.Error("clear went past its rectangle")
	}
	// Shapes and regions partly or wholly off the canvas are clipped.
	c.fill([]shape{{x: -50, y: -50, r: 10}, {x: 9, y: 9, w: 100, h: 100}}, red, red, 4, 1000, 0, 1)
	c.clear(-5, -5, 100, 100)
	for i, v := range c.pix {
		if v != 0 {
			t.Fatalf("byte %d left after clearing everything", i)
		}
	}
}

func TestCanvasDrawAndPixels(t *testing.T) {
	src := newPixelCanvas(4, 4)
	blue := rgba{0, 0, 1, 1}
	src.fill([]shape{{x: 0, y: 0, w: 4, h: 4}}, blue, rgba{}, 0, 0, 0, 1)
	dst := newPixelCanvas(10, 10)

	// Stamps add up, as the heatmap relies on, and are clipped at the edge.
	dst.draw(src, 2.4, 2.4, 4, 4, 0.25)
	once := dst.at(3, 3)[3]
	dst.draw(src, 2.4, 2.4, 4, 4, 0.25)
	twice := dst.at(3, 3)[3]
	if once != 64 || twice <= once {
		t.Errorf("alpha after one stamp %d, after two %d", once, twice)
	}
	if dst.at(1, 1)[3] != 0 || dst.at(6, 6)[3] != 0 {
		t.Error("stamp is not where it was placed")
	}
	dst.draw(src, -2, 8, 4, 4, 1)
	if got := dst.at(0, 9); got != [4]uint8{0, 0, 255, 255} {
		t.Errorf("clipped stamp %v", got)
	}
	// Scaled, and onto itself.
	dst.draw(src, 6, 0, 2, 2, 1)
	dst.draw(dst, 0, 0, 10, 10, 1)
	if got := dst.at(7, 1); got != [4]uint8{0, 0, 255, 255} {
		t.Errorf("scaled stamp %v", got)
	}

	// Reading past the edge gives transparent pixels; writing replaces.
	got := dst.region(8, 8, 4, 4)
	if len(got) != 64 || got[3] != dst.at(8, 8)[3] || got[15] != 0 {
		t.Errorf("region across the edge: %v", got)
	}
	block := bytes.Repeat([]uint8{1, 2, 3, 4}, 4)
	dst.put(block, 2, 2, 9, 9)
	if got := dst.at(9, 9); got != [4]uint8{1, 2, 3, 4} {
		t.Errorf("written pixel %v", got)
	}
}

const heatmapScript = `
echarts.registerMap('box', {type: 'FeatureCollection', features: [{
  type: 'Feature', properties: {name: 'box'},
  geometry: {type: 'Polygon', coordinates: [[[0, 0], [100, 0], [100, 50], [0, 50], [0, 0]]]}
}]});
option = {
  geo: {map: 'box', left: 0, top: 0, right: 0, bottom: 0, aspectScale: 1},
  visualMap: {show: false, min: 0, max: 10, inRange: {color: ['blue', 'red']}},
  series: [{type: 'heatmap', coordinateSystem: 'geo', pointSize: 12, blurSize: 16,
    data: [[25, 25, 10], [25, 25, 10], [25, 25, 10], [75, 25, 2]]}]
};`

// heatmapImage renders the geo heatmap and returns the picture it embeds.
func heatmapImage(t *testing.T, r *Renderer) *image.NRGBA {
	t.Helper()
	svg, err := r.Render(context.Background(), Request{Script: []byte(heatmapScript), Width: 400, Height: 200})
	if err != nil {
		t.Fatal(err)
	}
	checkWellFormed(t, svg)
	m := regexp.MustCompile(`<image href="data:image/png;base64,([^"]+)" width="400" height="200"`).FindSubmatch(svg)
	if m == nil {
		t.Fatalf("no embedded heatmap image: %.600s", svg)
	}
	raw, err := base64.StdEncoding.DecodeString(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	n, ok := img.(*image.NRGBA)
	if !ok || n.Rect.Dx() != 400 || n.Rect.Dy() != 200 {
		t.Fatalf("heatmap image is %T %v", img, img.Bounds())
	}
	return n
}

// A heatmap on a geo coordinate system is drawn into a canvas, recoloured
// by pixel and embedded in the SVG as a picture.
func TestGeoHeatmap(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	img := heatmapImage(t, r)
	// The map fills the 400 x 200 chart, so (25, 25) is at pixel (100, 100)
	// and (75, 25) at (300, 100).
	hot, cool, empty := img.NRGBAAt(100, 100), img.NRGBAAt(300, 100), img.NRGBAAt(200, 20)
	if hot.A < 128 || hot.R < 2*hot.B {
		t.Errorf("the dense point is %v, want strong and red", hot)
	}
	if cool.A == 0 || cool.A >= hot.A || cool.B < 2*cool.R {
		t.Errorf("the sparse point is %v, want faint and blue (dense is %v)", cool, hot)
	}
	if empty.A != 0 {
		t.Errorf("a place with no data is %v, want transparent", empty)
	}

	// The same chart comes out the same from a pooled runtime.
	pooled, err := New(WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pooled.RenderSVG(context.Background(), []byte(barOption), salesData()); err != nil {
		t.Fatal(err)
	}
	if again := heatmapImage(t, pooled); !bytes.Equal(again.Pix, img.Pix) {
		t.Error("the heatmap differs between renders")
	}
}

func TestMaxCanvasPixels(t *testing.T) {
	ctx := context.Background()
	// The heatmap above draws on a 400 x 200 canvas.
	r, err := New(WithMaxCanvasPixels(50_000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(ctx, Request{Script: []byte(heatmapScript), Width: 400, Height: 200}); !errors.Is(err, ErrCanvasTooLarge) {
		t.Fatalf("heatmap over the limit: got %v, want ErrCanvasTooLarge", err)
	}
	// A script cannot dodge the limit by catching the error, and a canvas
	// that is only measured with, or read from, costs nothing.
	const script = `
	  var c = document.createElement('canvas');
	  c.width = 100000; c.height = 100000;
	  var w = c.getContext('2d').measureText('abc').width;
	  c.getContext('2d').getImageData(0, 0, 1, 1);
	  var caught = 'no';
	  if (GRAB) { try { c.getContext('2d').fillRect(0, 0, 1, 1); } catch (e) { caught = 'yes'; } }
	  option = {title: {text: 'w ' + (w > 0) + ' caught ' + caught}};`
	svg, err := r.Render(ctx, Request{Script: []byte(strings.Replace(script, "GRAB", "false", 1))})
	if err != nil || !strings.Contains(string(svg), "w true caught no") {
		t.Fatalf("measuring on a huge canvas: %v %s", err, titleOf(svg))
	}
	if _, err := r.Render(ctx, Request{Script: []byte(strings.Replace(script, "GRAB", "true", 1))}); !errors.Is(err, ErrCanvasTooLarge) {
		t.Fatalf("huge canvas: got %v, want ErrCanvasTooLarge", err)
	}
	// The count starts again with each render.
	small, err := New(WithMaxCanvasPixels(400*200*4), WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		heatmapImage(t, small)
	}
}

// What a script draws on a canvas reaches the SVG when the canvas is used
// as a picture.
func TestScriptCanvasPixels(t *testing.T) {
	r := shimRenderer(t)
	s := renderScript(t, r, `
	  const canvas = document.createElement('canvas');
	  canvas.width = 8; canvas.height = 4;
	  const ctx = canvas.getContext('2d');
	  ctx.fillStyle = 'rgb(0, 128, 255)';
	  ctx.fillRect(0, 0, 4, 4);
	  const d = ctx.getImageData(0, 0, 8, 4);
	  d.data[4 * 7 + 1] = 200; d.data[4 * 7 + 3] = 255;
	  ctx.putImageData(d, 0, 0);
	  const back = ctx.getImageData(7, 0, 1, 1).data;
	  option = { title: { text: 'px ' + d.data[2] + ' ' + back[1] },
	    graphic: [{ type: 'image', style: { image: canvas, width: 8, height: 4 } }] };`)
	if !strings.Contains(s, "px 255 200") {
		t.Errorf("SVG title: %s", titleOf([]byte(s)))
	}
	if !strings.Contains(s, `href="data:image/png;base64,`) {
		t.Errorf("the canvas was not embedded as a PNG: %.400s", s)
	}
}

// Clearing a rectangle that lies partly or wholly off the canvas, or has a
// negative size, clears what it covers and nothing else.
func TestCanvasClearOutside(t *testing.T) {
	c := newPixelCanvas(10, 10)
	c.fill([]shape{{w: 10, h: 10}}, rgba{0, 0, 0, 1}, rgba{}, 0, 0, 0, 1)
	c.clear(20, 0, 25, 5)
	c.clear(-10, 0, -5, 5)
	c.clear(5, 0, 2, 5)
	if c.at(9, 0)[3] != 255 || c.at(0, 0)[3] != 255 {
		t.Fatal("clearing off the canvas cleared pixels on it")
	}
	r := shimRenderer(t)
	s := renderScript(t, r, `
	  const c = document.createElement('canvas');
	  c.width = 10; c.height = 10;
	  const ctx = c.getContext('2d');
	  ctx.fillRect(0, 0, 10, 10);
	  ctx.clearRect(20, 0, 5, 5);
	  ctx.clearRect(5, 0, -3, 5);
	  const d = ctx.getImageData(0, 0, 10, 1).data;
	  option = { title: { text: 'px ' + d[4 * 1 + 3] + ' ' + d[4 * 2 + 3] + ' ' + d[4 * 5 + 3] } };`)
	if !strings.Contains(s, "px 255 0 255") {
		t.Errorf("SVG title: %s", titleOf([]byte(s)))
	}
}

// A shadow blur far wider than the canvas costs no more than one as wide
// as the canvas, and still paints a faint shadow.
func TestCanvasHugeBlur(t *testing.T) {
	c := newPixelCanvas(10, 10)
	black := rgba{0, 0, 0, 1}
	start := time.Now()
	if err := c.fill([]shape{{x: 5, y: 5, r: 3}}, rgba{}, black, 1e6, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("blur took %v", d)
	}
	if a := c.at(5, 5)[3]; a > 2 {
		t.Errorf("a blur this wide spreads the shadow to almost nothing, got alpha %d", a)
	}

	// The working buffer counts against the pixel limit.
	c = newPixelCanvas(100, 100)
	c.maxScratch = 100 * 100
	if err := c.fill([]shape{{x: 50, y: 50, r: 3}}, rgba{}, black, 200, 0, 0, 1); !errors.Is(err, ErrCanvasTooLarge) {
		t.Errorf("blur over the limit: got %v, want ErrCanvasTooLarge", err)
	}

	// A render that ends stops the work.
	c = newPixelCanvas(100, 100)
	c.cancelled = func() bool { return true }
	if err := c.fill([]shape{{x: 50, y: 50, r: 3}}, rgba{}, black, 20, 0, 0, 1); !errors.Is(err, errCancelled) {
		t.Errorf("cancelled blur: got %v", err)
	}
	if err := c.fill([]shape{{w: 100, h: 100}}, black, rgba{}, 0, 0, 0, 1); !errors.Is(err, errCancelled) {
		t.Errorf("cancelled fill: got %v", err)
	}
}

// The render timeout stops drawing that happens in Go, in a single call.
func TestCanvasTimeout(t *testing.T) {
	r, err := New(WithTimeout(200 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	const script = `
	  const c = document.createElement('canvas');
	  c.width = 4000; c.height = 4000;
	  const ctx = c.getContext('2d');
	  ctx.beginPath();
	  for (let i = 0; i < 2000; i++) { ctx.rect(0, 0, 4000, 4000); }
	  ctx.fill();
	  option = { series: [] };`
	start := time.Now()
	_, err = r.Render(context.Background(), Request{Script: []byte(script)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("render took %v after a 200ms timeout", d)
	}
}

// Sizing a canvas one dimension at a time, then reading it back whole,
// costs the canvas once (after the drawing it had at its default size).
func TestCanvasChargedOnce(t *testing.T) {
	r, err := New(WithMaxCanvasPixels(300*150 + 2000*1000))
	if err != nil {
		t.Fatal(err)
	}
	const script = `
	  const c = document.createElement('canvas');
	  const ctx = c.getContext('2d');
	  ctx.fillRect(0, 0, 1, 1);
	  c.width = 2000; c.height = 1000;
	  ctx.fillRect(0, 0, 1, 1);
	  const d = ctx.getImageData(0, 0, 2000, 1000);
	  ctx.putImageData(d, 0, 0);
	  option = { title: { text: 'n ' + d.data.length } };`
	svg, err := r.Render(context.Background(), Request{Script: []byte(script)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(titleOf(svg), "n 8000000") {
		t.Errorf("SVG title: %s", titleOf(svg))
	}
}
