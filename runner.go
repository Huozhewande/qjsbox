// Package qjsbox runs untrusted JavaScript rules and factors in a single-use,
// OS-sandboxed QuickJS worker process supervised from Go.
package qjsbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

const MaxSourceBytes = 64 << 10
const MaxInputBytes = 1 << 20
const MaxOutputBytes = 64 << 10
const Timeout = time.Second

// MaxConcurrentWorkers is a per-shared-Runner hard admission limit. Admission
// never queues: nested rule/factor calls also fail promptly when all slots are in
// use, so a rule holding a slot cannot deadlock waiting for a child worker.
const MaxConcurrentWorkers = 8

// MaxReadOperations bounds all IPC reads, including cache hits and trace events.
const MaxReadOperations = 512

type Value struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
	// Nil means no option catalog restriction; an empty list means no options
	// are currently available in the captured visual-condition catalog.
	AvailableOptions *[]string `json:"availableOptions,omitempty"`
}

// ResolveFunc is a trusted, cooperative host callback. It must pass the supplied
// context to nested work and finish promptly when cancelled. It runs synchronously
// so Execute never returns while the resolver can still mutate caller-owned state.
type ResolveFunc func(context.Context, string) (Value, error)
type Request struct {
	Kind              string          `json:"kind"`
	Code              string          `json:"code"`
	Input             json.RawMessage `json:"input"`
	Now               int64           `json:"now"`
	ReturnType        string          `json:"returnType"`
	AllowedReferences []string        `json:"allowedReferences"`
	// OnReuse is a trusted synchronous trace sink, called on cache hits before
	// the worker resumes. It must only record the event in memory; no I/O,
	// waiting, or nested evaluation is permitted in this observer.
	OnReuse func(string) `json:"-"`
}
type Analysis struct {
	References  []string     `json:"references"`
	HelperTests []HelperTest `json:"helperTests,omitempty"`
}
type Result struct {
	Value                json.RawMessage `json:"value"`
	ValueType            string          `json:"valueType"`
	Reads                []string        `json:"reads"`
	DurationMilliseconds int64           `json:"durationMilliseconds"`
}
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

type Runner struct {
	binaryPath string
	workers    chan struct{}
}

func New(binaryPath string) *Runner {
	return &Runner{binaryPath: binaryPath, workers: make(chan struct{}, MaxConcurrentWorkers)}
}

type wireResult struct {
	Result
	References  []string     `json:"references"`
	HelperTests []HelperTest `json:"helperTests,omitempty"`
	Read        *string      `json:"read"`
	Error       *Error       `json:"error"`
}

func (r *Runner) Analyze(ctx context.Context, kind, code string) (Analysis, error) {
	v, err := r.run(ctx, Request{Kind: kind, Code: code, Input: json.RawMessage(`{}`)}, nil, true)
	return Analysis{References: v.References, HelperTests: v.HelperTests}, err
}
func (r *Runner) Execute(ctx context.Context, req Request, resolve ResolveFunc) (Result, error) {
	start := time.Now()
	v, err := r.run(ctx, req, resolve, false)
	v.DurationMilliseconds = time.Since(start).Milliseconds()
	return v.Result, err
}
func (r *Runner) run(ctx context.Context, req Request, resolve ResolveFunc, analyze bool) (out wireResult, err error) {
	if len(req.Code) == 0 || len(req.Code) > MaxSourceBytes || len(req.Input) > MaxInputBytes || len(req.AllowedReferences) > 512 {
		return out, &Error{Code: "SCRIPT_LIMIT", Message: "source, input or references exceeds limit"}
	}
	for _, ref := range req.AllowedReferences {
		if len(ref) == 0 || len(ref) > 256 {
			return out, &Error{Code: "SCRIPT_LIMIT", Message: "invalid reference size"}
		}
	}
	if req.Kind != "FACTOR" && req.Kind != "RULE" {
		return out, &Error{Code: "SCRIPT_KIND", Message: "expected FACTOR or RULE"}
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage(`{}`)
	}
	if !json.Valid(req.Input) {
		return out, &Error{Code: "SCRIPT_INPUT", Message: "invalid input JSON"}
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	if ctx.Err() != nil {
		return out, deadlineError(ctx.Err())
	}
	select {
	case r.workers <- struct{}{}:
		// The process cleanup defer is registered later and therefore runs first:
		// a slot is returned only after the worker has exited and been reaped.
		defer func() { <-r.workers }()
	default:
		return out, &Error{Code: "SCRIPT_BUSY", Message: "script worker capacity is exhausted"}
	}
	cmd := exec.CommandContext(ctx, r.binaryPath)
	cmd.Env = []string{"LANG=C"}
	cmd.Dir = "/"
	cmd.WaitDelay = 100 * time.Millisecond
	stdin, e := cmd.StdinPipe()
	if e != nil {
		return out, e
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		return out, e
	}
	if e = cmd.Start(); e != nil {
		return out, &Error{Code: "SCRIPT_UNAVAILABLE", Message: "sandbox worker could not start"}
	}
	defer func() {
		stdin.Close()
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	payload := struct {
		Request
		Analyze bool `json:"analyze"`
	}{req, analyze}
	if e = json.NewEncoder(stdin).Encode(payload); e != nil {
		return out, &Error{Code: "SCRIPT_UNAVAILABLE", Message: "worker request failed"}
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), MaxOutputBytes+4096)
	allowed := map[string]bool{}
	for _, ref := range req.AllowedReferences {
		allowed[ref] = true
	}
	reads := []string{}
	readOperations := 0
	replyFailed := false
	cache := map[string]struct {
		Value
		Error string `json:"error,omitempty"`
	}{}
	for scanner.Scan() {
		var msg wireResult
		if e = json.Unmarshal(scanner.Bytes(), &msg); e != nil {
			return out, &Error{Code: "SCRIPT_PROTOCOL", Message: "invalid worker response"}
		}
		if msg.Read == nil {
			out = msg
			out.Reads = reads
			stdin.Close()
			e = cmd.Wait()
			if ctx.Err() != nil {
				return out, deadlineError(ctx.Err())
			}
			if msg.Error != nil {
				return out, msg.Error
			}
			if replyFailed {
				return out, &Error{Code: "SCRIPT_PROTOCOL", Message: "worker returned success after its factor response pipe closed"}
			}
			if e != nil {
				return out, &Error{Code: "SCRIPT_UNAVAILABLE", Message: "sandbox worker exited unsuccessfully"}
			}
			if !analyze && len(out.Value) > MaxOutputBytes {
				return out, &Error{Code: "SCRIPT_LIMIT", Message: "output exceeds limit"}
			}
			return out, nil
		}
		ref := *msg.Read
		if replyFailed {
			return out, &Error{Code: "SCRIPT_PROTOCOL", Message: "worker requested a factor after its response pipe closed"}
		}
		if analyze || !allowed[ref] {
			return out, &Error{Code: "SCRIPT_REFERENCE", Message: "worker requested undeclared reference"}
		}
		readOperations++
		if readOperations > MaxReadOperations {
			return out, &Error{Code: "SCRIPT_LIMIT", Message: "factor read operation budget exceeded"}
		}
		response, ok := cache[ref]
		if ok && req.OnReuse != nil {
			req.OnReuse(ref)
		}
		if !ok {
			reads = append(reads, ref)
			if resolve == nil {
				response.Error = "required factor unavailable"
			} else {
				// CommandContext cancels the native process independently. Keep the
				// cooperative resolver in this call stack until its bounded cleanup
				// finishes, instead of abandoning mutations in another goroutine.
				value, resolveErr := resolve(ctx, ref)
				if ctx.Err() != nil {
					return out, deadlineError(ctx.Err())
				}
				response.Value = value
				if resolveErr != nil {
					response.Error = "required factor unavailable"
				}
			}
			if len(response.Value.Value) > MaxOutputBytes {
				response.Value = Value{}
				response.Error = "factor value exceeds limit"
			}
			if response.AvailableOptions != nil {
				size := 0
				for _, option := range *response.AvailableOptions {
					size += len(option)
					if size > MaxOutputBytes {
						break
					}
				}
				if size > MaxOutputBytes || len(*response.AvailableOptions) > MaxOutputBytes {
					response.Value = Value{}
					response.Error = "factor option catalog exceeds limit"
				}
			}
			if encoded, encodeErr := json.Marshal(response); encodeErr != nil || len(encoded) > MaxOutputBytes {
				response.Value = Value{}
				response.Error = "factor response exceeds limit or is invalid"
			}
			cache[ref] = response
		}
		if e = json.NewEncoder(stdin).Encode(response); e != nil {
			// The native watchdog can emit its terminal error and exit while a
			// cooperative resolver is finishing. A failed reply write does not
			// consume stdout: drain the terminal frame before classifying failure.
			replyFailed = true
			stdin.Close()
		}
	}
	if ctx.Err() != nil {
		return out, deadlineError(ctx.Err())
	}
	if scanErr := scanner.Err(); scanErr != nil {
		if errors.Is(scanErr, bufio.ErrTooLong) {
			return out, &Error{Code: "SCRIPT_LIMIT", Message: "worker response frame exceeds limit"}
		}
		return out, &Error{Code: "SCRIPT_PROTOCOL", Message: "worker response could not be read"}
	}
	stdin.Close()
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return out, deadlineError(ctx.Err())
	}
	return out, workerExitError(waitErr)
}

func workerExitError(err error) error {
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		// These exit codes are the native worker's explicit fallback contract
		// when its terminal JSON frame could not be delivered.
		switch exited.ExitCode() {
		case 124:
			return &Error{Code: "SCRIPT_TIMEOUT", Message: "native worker deadline exceeded"}
		case 125:
			return &Error{Code: "SCRIPT_UNAVAILABLE", Message: "native worker initialization failed"}
		case 126:
			return &Error{Code: "SCRIPT_LIMIT", Message: "native worker process memory limit exceeded"}
		}
	}
	if err != nil {
		return &Error{Code: "SCRIPT_UNAVAILABLE", Message: "sandbox worker exited without a terminal response"}
	}
	return &Error{Code: "SCRIPT_PROTOCOL", Message: "worker closed its output without a terminal response"}
}
func deadlineError(err error) error {
	code := "SCRIPT_TIMEOUT"
	if errors.Is(err, context.Canceled) {
		code = "SCRIPT_CANCELLED"
	}
	return &Error{Code: code, Message: fmt.Sprint(err)}
}
