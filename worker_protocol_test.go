package qjsbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func protocolWorker(t *testing.T, body string) *Runner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nIFS= read -r request\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return New(path)
}

func TestWorkerTerminalErrorSurvivesResolverReplyBrokenPipe(t *testing.T) {
	for _, test := range []struct {
		code string
		exit int
	}{{"SCRIPT_TIMEOUT", 124}, {"SCRIPT_LIMIT", 126}, {"SCRIPT_ERROR", 0}} {
		t.Run(test.code, func(t *testing.T) {
			// Closing the child's read end before emitting its read request makes
			// the parent's resolver reply fail with EPIPE on every execution.
			// Its terminal error is already available on the separate stdout pipe.
			body := fmt.Sprintf("exec 0<&-\nprintf '%%s\\n' '{\"read\":\"wait\"}' '{\"error\":{\"code\":\"%s\",\"message\":\"native terminal reason\"}}'\nexit %d\n", test.code, test.exit)
			runner := protocolWorker(t, body)
			calls := 0
			_, err := runner.Execute(context.Background(), Request{Kind: "RULE", Code: "return factors.wait;", AllowedReferences: []string{"wait"}}, func(context.Context, string) (Value, error) {
				calls++
				return Value{Type: "BOOLEAN", Value: json.RawMessage(`true`)}, nil
			})
			var actual *Error
			if !errors.As(err, &actual) || actual.Code != test.code || actual.Message != "native terminal reason" || calls != 1 {
				t.Fatalf("lost terminal reason after the broken pipe: error=%v calls=%d", err, calls)
			}
			if len(runner.workers) != 0 {
				t.Fatal("worker slot was not released after terminal failure")
			}
		})
	}
}

func TestWorkerEOFUsesNativeExitReason(t *testing.T) {
	for _, test := range []struct {
		exit int
		code string
	}{{124, "SCRIPT_TIMEOUT"}, {125, "SCRIPT_UNAVAILABLE"}, {126, "SCRIPT_LIMIT"}, {1, "SCRIPT_UNAVAILABLE"}, {0, "SCRIPT_PROTOCOL"}} {
		t.Run(fmt.Sprint(test.exit), func(t *testing.T) {
			runner := protocolWorker(t, fmt.Sprintf("exit %d\n", test.exit))
			_, err := runner.Execute(context.Background(), Request{Kind: "RULE", Code: "return true;"}, nil)
			var actual *Error
			if !errors.As(err, &actual) || actual.Code != test.code {
				t.Fatalf("exit %d was misclassified: %v", test.exit, err)
			}
		})
	}
}

func TestWorkerCannotResumeAfterResolverReplyPipeClosed(t *testing.T) {
	for _, terminal := range []string{`{"read":"wait"}`, `{"value":true,"valueType":"BOOLEAN"}`} {
		runner := protocolWorker(t, "exec 0<&-\nprintf '%s\\n' '{\"read\":\"wait\"}' '"+terminal+"'\n")
		calls := 0
		_, err := runner.Execute(context.Background(), Request{Kind: "RULE", Code: "return factors.wait;", AllowedReferences: []string{"wait"}}, func(context.Context, string) (Value, error) {
			calls++
			return Value{Type: "BOOLEAN", Value: json.RawMessage(`true`)}, nil
		})
		var actual *Error
		if !errors.As(err, &actual) || actual.Code != "SCRIPT_PROTOCOL" || calls != 1 {
			t.Fatalf("closed factor pipe was treated as an active protocol: %v, calls=%d", err, calls)
		}
	}
}
