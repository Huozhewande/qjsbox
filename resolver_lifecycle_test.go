package qjsbox

import (
	"context"
	"testing"
	"time"
)

func TestExecuteWaitsForCancelledResolverCleanup(t *testing.T) {
	runner := testRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	started := time.Now()
	_, err := runner.Execute(ctx, Request{Kind: "RULE", Code: "return factors.wait;", AllowedReferences: []string{"wait"}}, func(ctx context.Context, _ string) (Value, error) {
		time.AfterFunc(10*time.Millisecond, cancel)
		<-ctx.Done()
		// Model/native resolver cleanup must finish before its caller snapshots
		// session state, even when cancellation wins the worker deadline race.
		time.Sleep(30 * time.Millisecond)
		close(finished)
		return Value{}, ctx.Err()
	})
	if err == nil || time.Since(started) > 700*time.Millisecond {
		t.Fatalf("cancellation did not remain bounded: %v (%v)", err, time.Since(started))
	}
	select {
	case <-finished:
	default:
		t.Fatal("Execute returned while the resolver still owned mutable caller state")
	}
	if _, err := runner.Execute(context.Background(), Request{Kind: "FACTOR", Code: "return true;", ReturnType: "BOOLEAN"}, nil); err != nil {
		t.Fatalf("subsequent worker failed: %v", err)
	}
}
