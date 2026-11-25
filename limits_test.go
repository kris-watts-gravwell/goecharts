// Copyright (c) 2026, Gravwell, Inc.
// All rights reserved.
//
// Use of this source code is governed by a BSD 2-Clause license that can be
// found in the LICENSE file.

package goecharts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMaxInputBytesRequest(t *testing.T) {
	ctx := context.Background()
	r, err := New(WithMaxInputBytes(int64(len(barOption)) / 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RenderSVG(ctx, []byte(barOption), nil); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("option over the limit: got %v, want ErrInputTooLarge", err)
	}

	// The datasets count too: a limit that fits the option alone does not
	// fit the option with its data.
	r, err = New(WithMaxInputBytes(int64(len(barOption)) + 40))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RenderSVG(ctx, []byte(barOption), salesData()); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("datasets over the limit: got %v, want ErrInputTooLarge", err)
	}

	// No limit, and the default, both accept it.
	for _, opt := range []Option{WithMaxInputBytes(0), WithMaxInputBytes(DefaultMaxInputBytes)} {
		r, err = New(opt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.RenderSVG(ctx, []byte(barOption), salesData()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMaxInputBytesLoads(t *testing.T) {
	ctx := context.Background()
	body := []byte(`[` + strings.Repeat("1,", 500) + `1]`)
	loader := LoaderFunc(func(ctx context.Context, url string) ([]byte, error) { return body, nil })
	const once = `var d; $.getJSON('/a', function (v) { d = v; });
option = {title: {text: 'n ' + d.length}};`
	// Loads add up over a render, and a script that swallows the error
	// still fails the render.
	const twice = `var n = 0;
try { $.ajax({url: '/a', async: false}); n++; } catch (e) {}
try { fetch('/b'); n++; } catch (e) {}
try { $.get('/c'); n++; } catch (e) {}
option = {title: {text: 'n ' + n}};`

	limit := int64(len(twice) + len(body)*2)
	r, err := New(WithLoader(loader), WithImageLoader(loader), WithMaxInputBytes(limit))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(ctx, Request{Script: []byte(once)}); err != nil {
		t.Fatalf("one load within the limit: %v", err)
	}
	if _, err := r.Render(ctx, Request{Script: []byte(twice)}); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("three loads over the limit: got %v, want ErrInputTooLarge", err)
	}
	// The budget is per render: the next one starts afresh.
	if _, err := r.Render(ctx, Request{Script: []byte(once)}); err != nil {
		t.Fatalf("render after a failed one: %v", err)
	}

	// Images named by a plain option are loads as well.
	const option = `{"xAxis":{"type":"category","data":["a"]},"yAxis":{},
		"series":[{"type":"bar","data":[1],
		"itemStyle":{"color":{"image":"https://img.test/p.png","repeat":"repeat"}}}]}`
	r, err = New(WithImageLoader(loader), WithMaxInputBytes(int64(len(option))+100))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RenderSVG(ctx, []byte(option), nil); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("image over the limit: got %v, want ErrInputTooLarge", err)
	}
	if _, err := r.RenderSVG(ctx, []byte(barOption), nil); err != nil {
		t.Fatalf("pooled runtime after an image over the limit: %v", err)
	}
}

func TestMaxOutputBytes(t *testing.T) {
	ctx := context.Background()
	r := sharedRenderer(t)
	svg, err := r.RenderSVG(ctx, []byte(barOption), salesData())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		limit int64
		ok    bool
	}{
		// Not len(svg) exactly: ids inside the SVG depend on how many
		// charts the runtime has drawn, so its size varies by a few bytes.
		{int64(len(svg)) * 2, true},
		{int64(len(svg)) / 2, false},
		{0, true},
		{-1, true},
	} {
		r, err := New(WithMaxOutputBytes(c.limit))
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.RenderSVG(ctx, []byte(barOption), salesData())
		if c.ok && err != nil {
			t.Errorf("limit %d: %v", c.limit, err)
		}
		if !c.ok && !errors.Is(err, ErrOutputTooLarge) {
			t.Errorf("limit %d: got %v, want ErrOutputTooLarge", c.limit, err)
		}
	}
}

// A Loader learns how much it may return from its context.
func TestLoadLimit(t *testing.T) {
	const script = `$.get('/a'); $.get('/b'); option = {series: []};`
	var seen []int64
	loader := LoaderFunc(func(ctx context.Context, url string) ([]byte, error) {
		n, ok := LoadLimit(ctx)
		if !ok {
			t.Error("no load limit in the context")
		}
		seen = append(seen, n)
		return []byte("0123456789"), nil
	})
	const limit = 10_000
	r, err := New(WithLoader(loader), WithMaxInputBytes(limit))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(context.Background(), Request{Script: []byte(script)}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] >= limit-int64(len(script)) || seen[1] != seen[0]-10 {
		t.Errorf("load limits %v", seen)
	}
	if _, ok := LoadLimit(context.Background()); ok {
		t.Error("LoadLimit reports a limit outside a load")
	}
}

// A caller with no deadline of its own is not held forever waiting for a
// render slot.
func TestSlotWaitTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	loader := LoaderFunc(func(ctx context.Context, url string) ([]byte, error) {
		close(started)
		<-release
		return []byte("1"), nil
	})
	r, err := New(WithLoader(loader), WithMaxConcurrency(1), WithTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = r.Render(context.Background(), Request{Script: []byte(`$.get('/a'); option = {};`)}) }()
	<-started
	start := time.Now()
	_, err = r.RenderSVG(context.Background(), []byte(barOption), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("waited %v for a slot", d)
	}
}
