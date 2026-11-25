// Copyright (c) 2026, Gravwell, Inc.
// All rights reserved.
//
// Use of this source code is governed by a BSD 2-Clause license that can be
// found in the LICENSE file.

package goecharts

import (
	"context"
	"time"
)

// Loader resolves URLs that a script (Request.Script) loads through `$.get`,
// `$.getJSON`, `$.ajax` or `fetch`. Loads are synchronous from the script's
// point of view and happen on the rendering goroutine; a Loader must be safe
// for concurrent use because several renders may run at once.
//
// A load counts against WithMaxInputBytes only once Load has returned it,
// so the limit alone does not stop a Loader from reading a huge response
// into memory. A Loader that fetches from anywhere it does not trust should
// stop reading once it passes LoadLimit(ctx) bytes and return an error.
type Loader interface {
	Load(ctx context.Context, url string) ([]byte, error)
}

type loadLimitKey struct{}

// LoadLimit returns how many bytes the load made with ctx may return before
// its render exceeds the input limit set with WithMaxInputBytes. ok is
// false when the render has no limit, or ctx is not one passed to a
// Loader.
func LoadLimit(ctx context.Context) (n int64, ok bool) {
	n, ok = ctx.Value(loadLimitKey{}).(int64)
	return n, ok
}

// LoaderFunc adapts a function to the Loader interface.
type LoaderFunc func(ctx context.Context, url string) ([]byte, error)

// Load calls f.
func (f LoaderFunc) Load(ctx context.Context, url string) ([]byte, error) { return f(ctx, url) }

// LogFunc receives console output emitted by the ECharts runtime, such as
// warnings about unknown series types or deprecated options. level is the
// console method used: "error", "warn", "info", "log" or "debug".
type LogFunc func(level string, msg string)

type options struct {
	poolSize   int
	maxRenders int
	maxInput   int64
	maxOutput  int64

	maxCanvasPixels int64
	timeout         time.Duration
	log             LogFunc
	measurer        TextMeasurer
	loader          Loader
	imageLoader     Loader

	failOnError bool
}

// Option configures a Renderer.
type Option func(*options)

// WithPoolSize sets the maximum number of idle JavaScript runtimes kept for
// reuse. Each runtime holds a loaded copy of ECharts and serves one render at
// a time; when the pool is empty a new runtime is created on demand and
// discarded again if the pool is full. The default is runtime.NumCPU().
//
// The pool bounds idle runtimes only; WithMaxConcurrency bounds the number
// that exist at once.
func WithPoolSize(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.poolSize = n
		}
	}
}

// WithMaxConcurrency limits the number of renders in progress, and so the
// number of JavaScript runtimes alive at once. Further calls to Render wait
// for a slot until their context ends or, if that is sooner, for as long as
// the render timeout (WithTimeout). The default is runtime.GOMAXPROCS(0);
// more than that only adds memory, because a render is CPU-bound. Zero or a
// negative value removes the limit.
//
// This bounds the number of runtimes, not their size. The JavaScript engine
// has no memory limit: a render can allocate until the process runs out of
// memory, and the timeout bounds its CPU time only. A plain option is
// limited by its own size and the data it is given, but an untrusted script
// can allocate without bound, so run those in a process whose memory is
// limited from outside (a container or cgroup limit, for example).
func WithMaxConcurrency(n int) Option {
	return func(o *options) { o.maxRenders = n }
}

// Default size limits of a render; see WithMaxInputBytes and
// WithMaxOutputBytes. They are far above what a chart needs and exist to
// stop runaway input, not to shape normal use.
const (
	DefaultMaxInputBytes   = 256 << 20
	DefaultMaxOutputBytes  = 256 << 20
	DefaultMaxCanvasPixels = 64 << 20
)

// WithMaxCanvasPixels limits the canvas pixels one render may allocate,
// counting every canvas it draws on at the size it has then; each pixel
// takes four bytes. No single block of pixels read back, and no working
// buffer a shadow blur needs, may be larger than the limit either. Canvases are used by the heatmap on a geo
// coordinate system and by scripts. A render that exceeds the limit fails
// with an error that wraps ErrCanvasTooLarge. The default is
// DefaultMaxCanvasPixels; zero or a negative value removes the limit.
func WithMaxCanvasPixels(n int64) Option {
	return func(o *options) { o.maxCanvasPixels = n }
}

// WithMaxInputBytes limits how much data one render may take in. The total
// counts the request (Option or Script, Theme, and the datasets as they are
// encoded for ECharts) and everything the render fetches through the
// loaders set with WithLoader and WithImageLoader. A render that exceeds it
// fails with an error that wraps ErrInputTooLarge. The default is
// DefaultMaxInputBytes; zero or a negative value removes the limit.
func WithMaxInputBytes(n int64) Option {
	return func(o *options) { o.maxInput = n }
}

// WithMaxOutputBytes limits the size of the SVG document a render may
// return. A larger document fails the render with an error that wraps
// ErrOutputTooLarge. The document has been built inside the runtime by the
// time it is measured, so this bounds what a caller receives and stores
// rather than the memory the render used. The default is
// DefaultMaxOutputBytes; zero or a negative value removes the limit.
func WithMaxOutputBytes(n int64) Option {
	return func(o *options) { o.maxOutput = n }
}

// WithTimeout bounds the wall-clock time of a single render, independent of
// the caller's context. The default is 30 seconds; a negative value disables
// the built-in limit. Time spent waiting for a render slot (see
// WithMaxConcurrency) is not counted against the render; the wait is
// bounded separately by the same duration, so a call to Render returns
// within twice the timeout.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithLoader enables scripts to load data by URL through l. Without a
// loader every load fails, because fetching arbitrary URLs named in an
// untrusted script is a request-forgery risk.
//
// Only scripts reach this loader. Images are a separate opt-in, because a
// plain JSON option can name them too; see WithImageLoader. Without an
// image loader, the images a script uses are loaded through l, since the
// script could load them through it anyway; those named in a plain option
// are not.
func WithLoader(l Loader) Option {
	return func(o *options) { o.loader = l }
}

// WithImageLoader enables fetching the images a chart refers to by URL, to
// learn their pixel size (the SVG keeps the URL; the image is not embedded).
// Without it only data: URLs are read and other images have no known size.
//
// Unlike WithLoader this applies to plain JSON options as well as scripts:
// an `image://` symbol, a pattern fill or a graphic image element in
// Request.Option makes l load that URL. With untrusted options that is a
// request-forgery risk even though the response is never returned, so l
// should restrict where it fetches from.
func WithImageLoader(l Loader) Option {
	return func(o *options) { o.imageLoader = l }
}

// WithLogger forwards ECharts console output to fn. Without a logger all
// output is discarded.
func WithLogger(fn LogFunc) Option {
	return func(o *options) { o.log = fn }
}

// WithTextMeasurer replaces the text width function used for layout. See
// DefaultTextMeasurer.
func WithTextMeasurer(m TextMeasurer) Option {
	return func(o *options) {
		if m != nil {
			o.measurer = m
		}
	}
}

// WithFailOnError controls what happens when ECharts logs at error level
// while processing an option (for example a series that refers to a
// component that does not exist). ECharts itself only logs such problems and
// draws what it can, often an empty chart. By default this package turns
// them into a render failure; pass false to get the lenient behavior and
// receive the messages through WithLogger instead.
func WithFailOnError(fail bool) Option {
	return func(o *options) { o.failOnError = fail }
}
