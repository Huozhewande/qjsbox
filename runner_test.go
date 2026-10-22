package qjsbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testRunner(t *testing.T) *Runner {
	t.Helper()
	p := os.Getenv("QJSBOX_WORKER")
	if p == "" {
		p = "bin/qjsbox-worker"
	}
	p, _ = filepath.Abs(p)
	if _, e := os.Stat(p); e != nil {
		t.Fatalf("build the worker first (make worker): %v", e)
	}
	return New(p)
}
func TestExecution(t *testing.T) {
	r := testRunner(t)
	for _, tt := range []struct{ code, typ, want string }{
		{`return input.uid;`, "TEXT", `"u1"`},
		{`return input.user.records.filter(x=>x.createdAt >= context.now-100).length;`, "NUMBER", `1`},
		{`return context.now===1000;`, "BOOLEAN", `true`},
	} {
		t.Run(tt.typ, func(t *testing.T) {
			v, e := r.Execute(context.Background(), Request{Kind: "FACTOR", Code: tt.code, Input: json.RawMessage(`{"uid":"u1","user":{"records":[{"createdAt":950},{"createdAt":800}]}}`), Now: 1000, ReturnType: tt.typ}, nil)
			if e != nil || string(v.Value) != tt.want {
				t.Fatalf("%s %v", v.Value, e)
			}
		})
	}
}
func TestStaticReferences(t *testing.T) {
	r := testRunner(t)
	a, e := r.Analyze(context.Background(), "RULE", `// factors.fake
const text="factors.nope"; return factors.foo && factors['bar'];`)
	if e != nil || strings.Join(a.References, ",") != "bar,foo" {
		t.Fatalf("%+v %v", a, e)
	}
	for _, code := range []string{`const f=factors;return f.foo;`, `return factors[input.key];`, `return Reflect.get(factors,'foo');`, `return globalThis.factors.foo;`, `return import('os');`} {
		if _, e := r.Analyze(context.Background(), "RULE", code); e == nil {
			t.Errorf("accepted %s", code)
		}
	}
}
func TestLazyAndRequiredValues(t *testing.T) {
	r := testRunner(t)
	calls := 0
	resolver := func(ctx context.Context, ref string) (Value, error) {
		calls++
		if ref == "bad" {
			return Value{}, errors.New("missing")
		}
		return Value{Type: "BOOLEAN", Value: json.RawMessage(`true`)}, nil
	}
	for _, code := range []string{`return true || factors.bad;`, `return false && factors.bad;`, `return factors.good && factors.good;`} {
		calls = 0
		v, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: code, AllowedReferences: []string{"bad", "good"}}, resolver)
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(code, "good") && calls != 1 {
			t.Fatalf("not cached: %d %+v", calls, v)
		}
		if !strings.Contains(code, "good") && calls != 0 {
			t.Fatalf("eager calls %d", calls)
		}
	}
	for _, code := range []string{`try {factors.bad} catch(e) {} return false;`, `try {input.missing} catch(e) {} return false;`} {
		if _, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: code, AllowedReferences: []string{"bad"}}, resolver); e == nil {
			t.Fatalf("swallowed missing %s", code)
		}
	}
}
func TestThreeValues(t *testing.T) {
	r := testRunner(t)
	for _, tt := range []struct {
		code, want string
		fail       bool
	}{
		{`return __rule.all([()=>__rule.test('fact.bad','EQUALS',true),()=>__rule.test('fact.no','EQUALS',true)]);`, `false`, false},
		{`return __rule.any([()=>__rule.test('fact.bad','EQUALS',true),()=>__rule.test('fact.yes','EQUALS',true)]);`, `true`, false},
		{`return __rule.any([()=>__rule.test('fact.bad','EQUALS',true),()=>__rule.test('fact.no','EQUALS',true)]);`, ``, true},
	} {
		v, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: tt.code, AllowedReferences: []string{"fact.bad", "fact.no", "fact.yes"}}, func(ctx context.Context, ref string) (Value, error) {
			if strings.HasSuffix(ref, ".bad") {
				return Value{}, errors.New("missing")
			}
			return Value{Type: "BOOLEAN", Value: json.RawMessage(map[bool]string{true: "true", false: "false"}[strings.HasSuffix(ref, ".yes")])}, nil
		})
		if (e != nil) != tt.fail || string(v.Value) != tt.want {
			t.Fatalf("%s %v", v.Value, e)
		}
	}
}
func TestHostileScriptsAndRecovery(t *testing.T) {
	r := testRunner(t)
	for _, code := range []string{`while(true){}`, `function f(){return f()} return f();`, `return new Array(1e9).fill('x');`, `return 'x'.repeat(100000);`, `return null;`, `return undefined;`, `return {};`, `return Promise.resolve(true);`, `return NaN;`, `return ({}).constructor.constructor('return process')();`, `return fetch('https://example.com');`, `return std.loadFile('/etc/passwd');`, `return Date.now();`, `return input.x=2;`} {
		t.Run(code[:min(len(code), 25)], func(t *testing.T) {
			start := time.Now()
			if _, e := r.Execute(context.Background(), Request{Kind: "FACTOR", Code: code, ReturnType: "TEXT"}, nil); e == nil {
				t.Fatal("accepted hostile/invalid script")
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("deadline exceeded")
			}
			if _, e := r.Execute(context.Background(), Request{Kind: "FACTOR", Code: `return "healthy";`, ReturnType: "TEXT"}, nil); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestLimitsAndResolverCancellation(t *testing.T) {
	r := testRunner(t)
	if _, e := r.Analyze(context.Background(), "RULE", strings.Repeat(" ", MaxSourceBytes+1)); e == nil {
		t.Fatal("source limit absent")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	entered := make(chan struct{})
	_, e := r.Execute(ctx, Request{Kind: "RULE", Code: `return factors.wait;`, AllowedReferences: []string{"wait"}}, func(ctx context.Context, _ string) (Value, error) {
		close(entered)
		time.AfterFunc(30*time.Millisecond, cancel)
		<-ctx.Done()
		return Value{}, ctx.Err()
	})
	select {
	case <-entered:
	default:
		t.Fatal("resolver was never entered")
	}
	if e == nil || time.Since(start) > 700*time.Millisecond {
		t.Fatalf("cancel %v %s", e, time.Since(start))
	}
}

func TestParserAndDataBudgets(t *testing.T) {
	r := testRunner(t)
	start := time.Now()
	_, e := r.Analyze(context.Background(), "RULE", "return "+strings.Repeat("(", 20000)+"true"+strings.Repeat(")", 20000)+";")
	if e == nil {
		t.Fatal("pathological parser nesting accepted")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("parser escaped whole-worker budget")
	}
	deep := json.RawMessage(strings.Repeat(`{"x":`, 80) + `1` + strings.Repeat(`}`, 80))
	if _, e = r.Execute(context.Background(), Request{Kind: "FACTOR", Code: `return 1;`, ReturnType: "NUMBER", Input: deep}, nil); e == nil {
		t.Fatal("input depth accepted")
	}
	if _, e = r.Execute(context.Background(), Request{Kind: "FACTOR", Code: `return 1;`, ReturnType: "NUMBER", Input: json.RawMessage(`"` + strings.Repeat("a", MaxInputBytes) + `"`)}, nil); e == nil {
		t.Fatal("input size accepted")
	}
}

func TestErrorLocationAndHelperGuard(t *testing.T) {
	r := testRunner(t)
	_, e := r.Execute(context.Background(), Request{Kind: "FACTOR", Code: "\nthrow new Error('bad');", ReturnType: "TEXT"}, nil)
	var scriptErr *Error
	if !errors.As(e, &scriptErr) || scriptErr.Line != 2 {
		t.Fatalf("missing code location: %+v", e)
	}
	if _, e = r.Analyze(context.Background(), "RULE", `return __rule.test('fact.missing','EQUALS',true) === false;`); e == nil {
		t.Fatal("helper unavailable coercion accepted")
	}
}

func TestNativeSandboxPermissionsAndLimits(t *testing.T) {
	r := testRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data, e := exec.CommandContext(ctx, r.binaryPath, "--selftest-isolation").Output()
	if e != nil {
		t.Fatalf("native sandbox proof failed: %s %v", data, e)
	}
	var proof struct {
		FilesystemDenied              bool  `json:"filesystemDenied"`
		NetworkDenied                 bool  `json:"networkDenied"`
		ProcessCreationDenied         bool  `json:"processCreationDenied"`
		HeapLimitBytes                int64 `json:"heapLimitBytes"`
		ProcessAddressSpaceLimitBytes int64 `json:"processAddressSpaceLimitBytes"`
	}
	if e = json.Unmarshal(data, &proof); e != nil {
		t.Fatal(e)
	}
	if !proof.FilesystemDenied || !proof.NetworkDenied || !proof.ProcessCreationDenied || proof.HeapLimitBytes != 64<<20 {
		t.Fatalf("incomplete isolation proof: %s", data)
	}
	if runtime.GOOS == "linux" && proof.ProcessAddressSpaceLimitBytes != 256<<20 {
		t.Fatalf("Linux hard process memory cap absent: %s", data)
	}
	t.Logf("native sandbox proof: %s", data)
}
