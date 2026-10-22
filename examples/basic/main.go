// Command basic evaluates a factor, a rule and a three-valued rule with qjsbox.
//
//	make worker && go run ./examples/basic -worker bin/qjsbox-worker
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"path/filepath"

	"github.com/Huozhewande/qjsbox"
)

func main() {
	worker := flag.String("worker", "bin/qjsbox-worker", "path to the built qjsbox-worker")
	flag.Parse()
	path, err := filepath.Abs(*worker)
	if err != nil {
		log.Fatal(err)
	}
	// Share one Runner across requests: its concurrency limit is per Runner.
	runner := qjsbox.New(path)
	ctx := context.Background()
	const now = 1_700_000_000_000

	// A FACTOR computes one typed value from read-only JSON input.
	factor, err := runner.Execute(ctx, qjsbox.Request{
		Kind:       "FACTOR",
		ReturnType: "NUMBER",
		Now:        now,
		Code:       `return input.orders.filter(o => o.at >= context.now - 86400000).length;`,
		Input:      json.RawMessage(`{"orders":[{"at":1699999990000},{"at":1699999000000},{"at":1690000000000}]}`),
	}, nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("factor ordersLastDay = %s\n", factor.Value)

	// A RULE returns a boolean and reads named factors lazily. Analyze reports
	// the static references, which become the allow-list for Execute.
	rule := `return factors.ordersLastDay >= 2 && factors.accountAgeDays < 7;`
	analysis, err := runner.Analyze(ctx, "RULE", rule)
	if err != nil {
		log.Fatal(err)
	}
	resolve := func(_ context.Context, ref string) (qjsbox.Value, error) {
		switch ref {
		case "ordersLastDay":
			return qjsbox.Value{Type: "NUMBER", Value: factor.Value}, nil
		case "accountAgeDays":
			return qjsbox.Value{Type: "NUMBER", Value: json.RawMessage(`3`)}, nil
		}
		return qjsbox.Value{}, errors.New("unknown factor")
	}
	result, err := runner.Execute(ctx, qjsbox.Request{Kind: "RULE", Code: rule, AllowedReferences: analysis.References}, resolve)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("rule = %s (reads %v)\n", result.Value, result.Reads)

	// Generated helper code is three-valued: an unavailable fact never silently
	// becomes false. Here the country lookup fails but the score alone decides.
	threeValued := `return __rule.any([
  () => __rule.test('user.country', 'IN', ['XX', 'YY']),
  () => __rule.test('risk.score', 'GTE', 80)
]);`
	analysis, err = runner.Analyze(ctx, "RULE", threeValued)
	if err != nil {
		log.Fatal(err)
	}
	result, err = runner.Execute(ctx, qjsbox.Request{Kind: "RULE", Code: threeValued, AllowedReferences: analysis.References}, func(_ context.Context, ref string) (qjsbox.Value, error) {
		if ref == "risk.score" {
			return qjsbox.Value{Type: "NUMBER", Value: json.RawMessage(`85`)}, nil
		}
		return qjsbox.Value{}, errors.New("country provider timed out")
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("three-valued rule = %s (helper tests %d)\n", result.Value, len(analysis.HelperTests))

	// Hostile code fails closed with a structured error.
	_, err = runner.Execute(ctx, qjsbox.Request{Kind: "FACTOR", ReturnType: "TEXT", Code: `while (true) {}`}, nil)
	var scriptErr *qjsbox.Error
	if errors.As(err, &scriptErr) {
		fmt.Printf("infinite loop -> %s\n", scriptErr.Code)
	}
}
