// Copyright (c) 2026, Gravwell, Inc.
// All rights reserved.
//
// Use of this source code is governed by a BSD 2-Clause license that can be
// found in the LICENSE file.

package goecharts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

// jsRuntime is one goja VM with ECharts loaded. It is not safe for
// concurrent use; Renderer hands each one to a single render at a time.
type jsRuntime struct {
	r  *Renderer
	vm *goja.Runtime

	renderFn  goja.Callable
	prepareFn goja.Callable
	finishFn  goja.Callable

	// ctx is the context of the render in progress, used by loads.
	ctx context.Context
	// script is set while the render in progress runs a script.
	script bool

	// inputLeft is how many more bytes the render in progress may load
	// before it exceeds the input limit; negative means no limit. limitErr
	// records the first size limit the render hit, so that the render
	// fails with it even if the script caught the error thrown at it.
	inputLeft int64
	limitErr  error
	// canvasPixels counts the canvas pixels the render has allocated.
	canvasPixels int64

	// Messages ECharts logged at error level during the current render.
	loggedErrors []string

	fontCache map[string]Font
}

func newJSRuntime(r *Renderer) (*jsRuntime, error) {
	rt := &jsRuntime{r: r, vm: goja.New(), fontCache: map[string]Font{}}
	vm := rt.vm

	host := vm.NewObject()
	_ = host.Set("measureText", rt.hostMeasureText)
	_ = host.Set("load", rt.hostLoad)
	_ = host.Set("parseXML", rt.hostParseXML)
	_ = host.Set("imageSize", rt.hostImageSize)
	_ = host.Set("canvas", rt.hostCanvas)
	_ = vm.Set("__goecharts_host", host)

	console := vm.NewObject()
	for _, level := range []string{"log", "info", "warn", "error", "debug"} {
		_ = console.Set(level, func(call goja.FunctionCall) goja.Value {
			rt.log(level, call.Arguments)
			return goja.Undefined()
		})
	}
	_ = vm.Set("console", console)

	for _, p := range []*goja.Program{r.pre, r.bundle, r.post} {
		if _, err := vm.RunProgram(p); err != nil {
			return nil, fmt.Errorf("echarts: initializing runtime: %w", wrapJSError(err))
		}
	}
	for name, dst := range map[string]*goja.Callable{
		"render": &rt.renderFn, "prepareScript": &rt.prepareFn, "finishScript": &rt.finishFn,
	} {
		v, err := vm.RunString("__goecharts." + name)
		if err != nil {
			return nil, fmt.Errorf("echarts: initializing runtime: %w", err)
		}
		fn, ok := goja.AssertFunction(v)
		if !ok {
			return nil, fmt.Errorf("echarts: initializing runtime: __goecharts.%s is not a function", name)
		}
		*dst = fn
	}
	return rt, nil
}

// reset clears per-render state so the runtime can be pooled.
func (rt *jsRuntime) reset() {
	rt.loggedErrors = nil
	rt.limitErr = nil
	rt.inputLeft = -1
	rt.canvasPixels = 0
	rt.ctx = nil
	rt.script = false
	rt.vm.ClearInterrupt()
	if len(rt.fontCache) > 4096 {
		rt.fontCache = map[string]Font{}
	}
}

func (rt *jsRuntime) render(ctx context.Context, option, data, opts string) ([]byte, error) {
	stop := rt.begin(ctx)
	defer stop()
	rt.script = false
	vm := rt.vm
	resultV, err := rt.renderFn(goja.Undefined(), vm.ToValue(option), vm.ToValue(data), vm.ToValue(opts))
	if err != nil {
		return nil, rt.wrapError(err)
	}
	return rt.result(resultV)
}

// renderScript runs a script and serializes the chart it configured. The
// script runs in one call and the chart is serialized in a second, so that
// promise reactions the script scheduled (a fetch().then chain) run in
// between, when control is back in Go.
func (rt *jsRuntime) renderScript(ctx context.Context, script, data, opts string) ([]byte, error) {
	stop := rt.begin(ctx)
	defer stop()
	rt.script = true
	vm := rt.vm
	if _, err := rt.prepareFn(goja.Undefined(), vm.ToValue(script), vm.ToValue(data), vm.ToValue(opts)); err != nil {
		// Leave no half-built chart behind for the next render.
		_, _ = rt.finishFn(goja.Undefined())
		return nil, rt.wrapError(err)
	}
	resultV, err := rt.finishFn(goja.Undefined())
	if err != nil {
		return nil, rt.wrapError(err)
	}
	return rt.result(resultV)
}

// begin starts a render: it records the context and interrupts the VM if
// the context ends while JavaScript is running. The returned function must
// be called when the render is over; it waits for the watcher to exit so
// that a late Interrupt cannot hit the next render on this runtime (reset()
// then clears any interrupt that landed after JavaScript had finished).
func (rt *jsRuntime) begin(ctx context.Context) (stop func()) {
	rt.ctx = ctx
	rt.loggedErrors = rt.loggedErrors[:0]
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-ctx.Done():
			rt.vm.Interrupt(ctx.Err())
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

// result extracts the SVG from a {svg, error} result object.
func (rt *jsRuntime) result(resultV goja.Value) ([]byte, error) {
	if rt.limitErr != nil {
		return nil, rt.limitErr
	}
	result, ok := resultV.(*goja.Object)
	if !ok {
		return nil, errors.New("echarts: render returned no result object")
	}
	if errV := result.Get("error"); errV != nil && !goja.IsNull(errV) && !goja.IsUndefined(errV) {
		return nil, &Error{Msg: errV.String()}
	}
	if rt.r.opts.failOnError && len(rt.loggedErrors) > 0 {
		return nil, &Error{Msg: strings.Join(rt.loggedErrors, "; ")}
	}
	svg := result.Get("svg")
	if svg == nil || goja.IsNull(svg) || goja.IsUndefined(svg) {
		return nil, errors.New("echarts: render produced no SVG")
	}
	out := svg.String()
	if max := rt.r.opts.maxOutput; max > 0 && int64(len(out)) > max {
		return nil, fmt.Errorf("%w: SVG is %d bytes, limit %d", ErrOutputTooLarge, len(out), max)
	}
	return []byte(out), nil
}

// wrapError converts an error from running JavaScript. A size limit hit
// during the render wins over whatever the script made of it, unless the
// render was interrupted.
func (rt *jsRuntime) wrapError(err error) error {
	if _, ok := errors.AsType[*goja.InterruptedError](err); !ok && rt.limitErr != nil {
		return rt.limitErr
	}
	return wrapJSError(err)
}

// chargeInput counts n loaded bytes against the render's input limit and
// reports whether they fit.
func (rt *jsRuntime) chargeInput(url string, n int) error {
	if rt.inputLeft < 0 {
		return nil
	}
	if int64(n) > rt.inputLeft {
		err := fmt.Errorf("%w: loading %s (%d bytes) passes the limit of %d", ErrInputTooLarge, url, n, rt.r.opts.maxInput)
		if rt.limitErr == nil {
			rt.limitErr = err
		}
		rt.inputLeft = 0
		return err
	}
	rt.inputLeft -= int64(n)
	return nil
}

func wrapJSError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*goja.InterruptedError](err); ok {
		return err
	}
	if exc, ok := errors.AsType[*goja.Exception](err); ok {
		msg := exc.Value().String()
		if obj, ok := exc.Value().(*goja.Object); ok {
			if m := obj.Get("message"); m != nil && !goja.IsUndefined(m) && m.String() != "" {
				msg = m.String()
			} else {
				// The production ECharts build strips most error messages;
				// the stack at least says where inside the bundle it failed.
				msg = exc.String()
			}
		}
		return &Error{Msg: msg}
	}
	return fmt.Errorf("echarts: %w", err)
}

// hostLoad(url) fetches url through the Renderer's Loader and returns the
// body as a string, or throws.
func (rt *jsRuntime) hostLoad(call goja.FunctionCall) goja.Value {
	url := call.Argument(0).String()
	loader := rt.r.opts.loader
	if loader == nil {
		panic(rt.vm.NewGoError(fmt.Errorf("loading is disabled (no Loader configured): %s", url)))
	}
	b, err := rt.fetch(loader, url)
	if err != nil {
		panic(rt.vm.NewGoError(err))
	}
	return rt.vm.ToValue(string(b))
}

// fetch loads url through loader and counts it against the input limit.
// The context it passes tells the loader how much it may return; see
// LoadLimit.
func (rt *jsRuntime) fetch(loader Loader, url string) ([]byte, error) {
	ctx := rt.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if rt.inputLeft >= 0 {
		ctx = context.WithValue(ctx, loadLimitKey{}, rt.inputLeft)
	}
	b, err := loader.Load(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", url, err)
	}
	if err := rt.chargeInput(url, len(b)); err != nil {
		return nil, err
	}
	return b, nil
}

func (rt *jsRuntime) hostMeasureText(call goja.FunctionCall) goja.Value {
	text := call.Argument(0).String()
	font := rt.parseFont(call.Argument(1).String())
	return rt.vm.ToValue(rt.r.opts.measurer(text, font))
}

// parseFont parses a CSS font string, caching results per runtime since the
// same few font strings recur for every label.
func (rt *jsRuntime) parseFont(fontStr string) Font {
	font, ok := rt.fontCache[fontStr]
	if !ok {
		font = ParseCSSFont(fontStr)
		if len(rt.fontCache) > 4096 {
			rt.fontCache = map[string]Font{}
		}
		rt.fontCache[fontStr] = font
	}
	return font
}

func (rt *jsRuntime) log(level string, args []goja.Value) {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = a.String()
	}
	msg := strings.Join(parts, " ")
	if level == "error" {
		rt.loggedErrors = append(rt.loggedErrors, msg)
	}
	if fn := rt.r.opts.log; fn != nil {
		fn(level, msg)
	}
}
