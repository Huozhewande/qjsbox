package qjsbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerAdmissionSaturationAndRecovery(t *testing.T) {
	r := testRunner(t)
	const expectedLimit = 8
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, expectedLimit)
	release := make(chan struct{})
	results := make(chan error, expectedLimit)
	var active, peak atomic.Int32
	resolver := func(ctx context.Context, _ string) (Value, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
		}
		entered <- struct{}{}
		select {
		case <-release:
			return Value{Type: "BOOLEAN", Value: json.RawMessage(`true`)}, nil
		case <-ctx.Done():
			return Value{}, ctx.Err()
		}
	}
	for i := 0; i < expectedLimit; i++ {
		go func() {
			_, e := r.Execute(ctx, Request{Kind: "RULE", Code: `return factors.wait;`, AllowedReferences: []string{"wait"}}, resolver)
			results <- e
		}()
	}
	for i := 0; i < expectedLimit; i++ {
		select {
		case <-entered:
		case <-time.After(700 * time.Millisecond):
			cancel()
			t.Fatal("workers did not enter resolver within budget")
		}
	}
	start := time.Now()
	_, err := r.Execute(context.Background(), Request{Kind: "FACTOR", Code: `return 1;`, ReturnType: "NUMBER"}, nil)
	var engineErr *Error
	if !errors.As(err, &engineErr) || engineErr.Code != "SCRIPT_BUSY" {
		cancel()
		t.Fatalf("ninth worker was not rejected: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		cancel()
		t.Fatal("saturation queued instead of rejecting")
	}
	if _, e := r.Analyze(context.Background(), "RULE", `return true;`); !errors.As(e, &engineErr) || engineErr.Code != "SCRIPT_BUSY" {
		cancel()
		t.Fatalf("analysis bypassed shared worker admission: %v", e)
	}
	close(release)
	for i := 0; i < expectedLimit; i++ {
		if e := <-results; e != nil {
			t.Fatalf("admitted worker failed: %v", e)
		}
	}
	if peak.Load() != expectedLimit {
		t.Fatalf("observed peak %d", peak.Load())
	}
	t.Logf("peak admitted workers=%d, overflow rejected without queue", peak.Load())
	if _, e := r.Execute(context.Background(), Request{Kind: "FACTOR", Code: `return 2;`, ReturnType: "NUMBER"}, nil); e != nil {
		t.Fatalf("slots not reusable: %v", e)
	}
}

func TestWorkerAdmissionCancelledSlotsAreReusable(t *testing.T) {
	r := testRunner(t)
	for round := 0; round < 2; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{}, 8)
		results := make(chan error, 8)
		for i := 0; i < 8; i++ {
			go func() {
				_, e := r.Execute(ctx, Request{Kind: "RULE", Code: `return factors.wait;`, AllowedReferences: []string{"wait"}}, func(ctx context.Context, _ string) (Value, error) {
					entered <- struct{}{}
					<-ctx.Done()
					return Value{}, ctx.Err()
				})
				results <- e
			}()
		}
		for i := 0; i < 8; i++ {
			select {
			case <-entered:
			case <-time.After(700 * time.Millisecond):
				cancel()
				t.Fatal("admission never reached resolver")
			}
		}
		cancel()
		for i := 0; i < 8; i++ {
			if e := <-results; e == nil {
				t.Fatal("cancelled worker succeeded")
			}
		}
	}
	if _, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: `return true;`}, nil); e != nil {
		t.Fatalf("cancelled slots leaked: %v", e)
	}
}

func TestNestedWorkerAdmissionDoesNotWaitWhenSaturated(t *testing.T) {
	r := testRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 8)
	proceed := make(chan struct{})
	results := make(chan error, 8)
	var rejected atomic.Int32
	for i := 0; i < 8; i++ {
		go func() {
			_, e := r.Execute(ctx, Request{Kind: "RULE", Code: `return factors.nested;`, AllowedReferences: []string{"nested"}}, func(ctx context.Context, _ string) (Value, error) {
				entered <- struct{}{}
				select {
				case <-proceed:
				case <-ctx.Done():
					return Value{}, ctx.Err()
				}
				_, e := r.Execute(ctx, Request{Kind: "FACTOR", Code: `return true;`, ReturnType: "BOOLEAN"}, nil)
				var busy *Error
				if errors.As(e, &busy) && busy.Code == "SCRIPT_BUSY" {
					rejected.Add(1)
				}
				return Value{}, e
			})
			results <- e
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case <-entered:
		case <-time.After(700 * time.Millisecond):
			cancel()
			t.Fatal("parent workers did not start")
		}
	}
	start := time.Now()
	close(proceed)
	for i := 0; i < 8; i++ {
		select {
		case <-results:
		case <-time.After(time.Second):
			t.Fatal("nested admission deadlocked")
		}
	}
	if rejected.Load() == 0 {
		t.Fatal("nested saturation did not reject")
	}
	t.Logf("nested saturation rejected %d requests; all parents returned in %s", rejected.Load(), time.Since(start))
}

func TestWorkerAdmissionFailurePathsReleaseSlots(t *testing.T) {
	healthy := testRunner(t).binaryPath
	r := New(filepath.Join(t.TempDir(), "missing-worker"))
	for i := 0; i < MaxConcurrentWorkers+2; i++ {
		_, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: `return true;`}, nil)
		var failure *Error
		if !errors.As(e, &failure) || failure.Code != "SCRIPT_UNAVAILABLE" {
			t.Fatalf("startup slot leaked: %v", e)
		}
	}
	fixture := filepath.Join(t.TempDir(), "invalid-protocol")
	if e := os.WriteFile(fixture, []byte("#!/bin/sh\nIFS= read -r line\nprintf 'invalid\\n'\n"), 0700); e != nil {
		t.Fatal(e)
	}
	r.binaryPath = fixture
	for i := 0; i < MaxConcurrentWorkers+2; i++ {
		_, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: `return true;`}, nil)
		var failure *Error
		if !errors.As(e, &failure) || failure.Code != "SCRIPT_PROTOCOL" {
			t.Fatalf("protocol slot leaked: %v", e)
		}
	}
	r.binaryPath = healthy
	if _, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: `return true;`}, nil); e != nil {
		t.Fatalf("failure path slots were not reusable: %v", e)
	}
}

func TestCachedReadsEmitOrderedReuseWithoutRecomputation(t *testing.T) {
	r := testRunner(t)
	for _, unavailable := range []bool{false, true} {
		events := []string{}
		code := `return factors.foo && factors.foo;`
		if unavailable {
			code = `try{factors.foo}catch(e){} try{factors.foo}catch(e){} return false;`
		}
		_, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: code, AllowedReferences: []string{"foo"}, OnReuse: func(ref string) { events = append(events, "REUSED:"+ref) }}, func(context.Context, string) (Value, error) {
			events = append(events, "EXECUTED:foo")
			if unavailable {
				return Value{}, errors.New("missing")
			}
			return Value{Type: "BOOLEAN", Value: json.RawMessage(`true`)}, nil
		})
		if (e != nil) != unavailable {
			t.Fatalf("unexpected result: %v", e)
		}
		if len(events) != 2 || events[0] != "EXECUTED:foo" || events[1] != "REUSED:foo" {
			t.Fatalf("wrong cached read sequence: %v", events)
		}
	}
}

func TestRepeatedReadsHaveIndependentOperationBudget(t *testing.T) {
	r := testRunner(t)
	calls, reuses := 0, 0
	_, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: `for(let i=0;i<513;i++) factors.foo; return true;`, AllowedReferences: []string{"foo"}, OnReuse: func(string) { reuses++ }}, func(context.Context, string) (Value, error) {
		calls++
		return Value{Type: "BOOLEAN", Value: json.RawMessage(`true`)}, nil
	})
	var limit *Error
	if !errors.As(e, &limit) || limit.Code != "SCRIPT_LIMIT" {
		t.Fatalf("repeated IPC reads not bounded: %v", e)
	}
	if calls != 1 || reuses != 511 {
		t.Fatalf("unexpected bounded events: executed=%d reused=%d", calls, reuses)
	}
	t.Logf("read operations delivered=%d, next read rejected before trace append", calls+reuses)
	if _, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: `return true;`}, nil); e != nil {
		t.Fatalf("read-budget failure poisoned next request: %v", e)
	}
}
