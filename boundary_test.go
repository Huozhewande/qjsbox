package qjsbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestTrustedGlobalBindingsCannotBeReplaced(t *testing.T) {
	r := testRunner(t)
	for _, code := range []string{
		`Reflect = {has:()=>true,get:()=>false}; return input.missing;`,
		`try { globalThis.Reflect = {has:()=>true,get:()=>false}; } catch(e) {} try { input.missing; } catch(e) {} return false;`,
		`Object.defineProperty(globalThis,'Reflect',{value:{has:()=>true,get:()=>false}}); return input.missing;`,
		`JSON = {stringify:()=>"false"}; return false;`,
		`Number = {isFinite:()=>true}; return NaN;`,
		`Array = {isArray:()=>true}; return false;`,
		`Object = {freeze:x=>x}; return false;`,
		`Math = {max:()=>0}; throw new Error('bad');`,
		`Error = function(){}; try {input.missing} catch(e){} return false;`,
		`RegExp = function(){return {exec:()=>null}}; return false;`,
		`Set = function(){return {has:()=>true}}; return false;`,
		`Function.prototype.call = ()=>false; return false;`,
	} {
		t.Run(code[:min(len(code), 30)], func(t *testing.T) {
			if v, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: code}, nil); e == nil {
				t.Fatalf("trusted binding replacement accepted: %s", v.Value)
			}
		})
	}
}

func TestImplicitWrapperArgumentsAreRejected(t *testing.T) {
	r := testRunner(t)
	for _, code := range []string{
		`return arguments[2][input.ref] === true;`,
		`if(false) factors.foo; const f=arguments[2]; return f[input.ref];`,
		`return (()=>arguments[2][input.ref])();`,
		`const args=arguments; return args[2].foo;`,
		`return Reflect.get(arguments[2],input.ref);`,
		`return ((...rest)=>arguments[2].foo)();`,
		`return function(){return arguments[2]}();`,
		`return arguments[3].test('fact.foo','EQUALS',true);`,
	} {
		t.Run(code[:min(len(code), 35)], func(t *testing.T) {
			if a, e := r.Analyze(context.Background(), "RULE", code); e == nil {
				t.Fatalf("implicit alias analyzed: %+v", a)
			}
			calls := 0
			if v, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: code, Input: json.RawMessage(`{"ref":"foo"}`), AllowedReferences: []string{"foo", "fact.foo"}}, func(context.Context, string) (Value, error) {
				calls++
				return Value{Type: "BOOLEAN", Value: json.RawMessage(`true`)}, nil
			}); e == nil {
				t.Fatalf("implicit alias executed: %s", v.Value)
			}
			if calls != 0 {
				t.Fatalf("invalid source made %d reads", calls)
			}
		})
	}
}

func TestInvalidHelperOperandsFailBeforeReads(t *testing.T) {
	r := testRunner(t)
	for _, args := range []string{
		`'IN',[1]`, `'IN',["a",null]`, `'IN',[]`, `'IN',[["a"]]`, `'IN',"a"`,
		`'ANY_MATCH',[false]`, `'ANY_MATCH',[null]`, `'UNKNOWN_OP',["a"]`, `'EXISTS',true`,
		`'EQUALS'`, `'EQUALS',null`, `'EQUALS',["a"]`, `'EQUALS',/x/`,
		`'GTE',"1"`, `'LTE',false`, `'GTE',1e400`, `'GTE'`,
	} {
		code := `return __rule.test('fact.x',` + args + `);`
		t.Run(args, func(t *testing.T) {
			if a, e := r.Analyze(context.Background(), "RULE", code); e == nil {
				t.Fatalf("invalid helper analyzed: %+v", a)
			}
			calls := 0
			if v, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: code, AllowedReferences: []string{"fact.x"}}, func(context.Context, string) (Value, error) {
				calls++
				return Value{Type: "TEXT", Value: json.RawMessage(`"1"`)}, nil
			}); e == nil {
				t.Fatalf("invalid operand became result %s", v.Value)
			}
			if calls != 0 {
				t.Fatalf("invalid helper made %d reads", calls)
			}
		})
	}
}

func TestNullAndRequiredHostValuesHaveDistinctContracts(t *testing.T) {
	r := testRunner(t)
	for _, code := range []string{`return input.a === null;`, `try {return input.a.b;} catch(e) {return false;}`} {
		v, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: code, Input: json.RawMessage(`{"a":null}`)}, nil)
		want := "true"
		if strings.Contains(code, "catch") {
			want = "false"
		}
		if e != nil || string(v.Value) != want {
			t.Fatalf("legal null handling: %s %v", v.Value, e)
		}
	}
	if _, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: `try {return input.a.b;} catch(e) {return false;}`}, nil); e == nil {
		t.Fatal("missing host input swallowed")
	}
	if _, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: `try {return factors.foo;} catch(e) {return false;}`, AllowedReferences: []string{"foo"}}, func(context.Context, string) (Value, error) {
		return Value{Type: "BOOLEAN", Value: json.RawMessage(`"false"`)}, nil
	}); e == nil {
		t.Fatal("mistyped required factor swallowed")
	}
}

func TestOptionCatalogAvailabilityIsLeafSpecific(t *testing.T) {
	r := testRunner(t)
	known := []string{"enabled"}
	for _, tt := range []struct {
		code, want string
		fails      bool
	}{
		{`return __rule.test('user.region','EQUALS','removed');`, "", true},
		{`return __rule.test('user.region','IN',['removed','enabled']);`, "", true},
		{`return __rule.any([()=>__rule.test('user.region','EQUALS','removed'),()=>__rule.test('user.region','EQUALS','enabled')]);`, "true", false},
		{`return __rule.all([()=>__rule.test('content.tags','ANY_MATCH',['removed']),()=>__rule.test('content.flags','EQUALS','other')]);`, "false", false},
		{`return __rule.test('content.flags','ANY_MATCH',['removed']);`, "false", false},
	} {
		v, e := r.Execute(context.Background(), Request{Kind: "RULE", Code: tt.code, AllowedReferences: []string{"user.region", "content.tags", "content.flags"}}, func(_ context.Context, ref string) (Value, error) {
			switch ref {
			case "user.region":
				return Value{Type: "TEXT", Value: json.RawMessage(`"enabled"`), AvailableOptions: &known}, nil
			case "content.tags":
				return Value{Type: "SET", Value: json.RawMessage(`["enabled"]`), AvailableOptions: &known}, nil
			}
			// No catalog: choices are compared normally.
			return Value{Type: "SET", Value: json.RawMessage(`["enabled"]`)}, nil
		})
		if (e != nil) != tt.fails || string(v.Value) != tt.want {
			t.Fatalf("option parity %s: %s %v", tt.code, v.Value, e)
		}
	}
}
