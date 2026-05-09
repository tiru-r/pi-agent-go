package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

const hookCallTimeout = 5 * time.Second

// jsExtension runs a JavaScript extension in-process via goja (pure Go, no Node.js).
//
// Extensions communicate with Pi through a promise-based hostcall protocol.
// JS calls pi.tool(), pi.http(), pi.exec(), pi.env(), pi.session(), pi.ui(),
// or pi.log() — each returns a Promise resolved synchronously before returning
// to the caller. Extensions may be written as async functions and use await.
//
// Execution model (per Execute call):
//
//  1. callLocked acquires the VM mutex and sets callCtx.
//  2. executeFn is called; it runs until completion or the first await.
//  3. Any pi.* calls inside execute run synchronously: capability is checked,
//     the request is dispatched, and the returned Promise is pre-resolved.
//  4. If execute is async, the event loop ticks until the root Promise settles.
//  5. callLocked releases the mutex.
type jsExtension struct {
	info     Info
	manifest Manifest

	mu      sync.Mutex    // serialises all VM access; goja is not goroutine-safe
	vm      *goja.Runtime
	callCtx context.Context // current call's context; valid only while mu is held

	// pre-resolved function references (valid for the VM's lifetime)
	executeFn    goja.Callable
	beforeToolFn goja.Callable // nil if not defined
	afterToolFn  goja.Callable // nil if not defined

	// Dispatcher handles capability, dedup, lanes, shadow, telemetry.
	dispatcher *HostcallDispatcher
}

func loadJS(ctx context.Context, path string) (*jsExtension, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Static analysis before loading.
	scan := Scan(string(src))
	if len(scan.ForbiddenPatterns) > 0 {
		return nil, fmt.Errorf("%s: forbidden patterns detected: %s", path, strings.Join(scan.ForbiddenPatterns, ", "))
	}
	if unavail := scan.UnavailableImports(); len(unavail) > 0 {
		fmt.Fprintf(os.Stderr, "[ext] %s: unavailable require() modules: %s\n",
			filepath.Base(path), strings.Join(unavail, ", "))
	}

	manifest := loadManifest(path)
	e := &jsExtension{
		manifest:   manifest,
		dispatcher: NewHostcallDispatcher(filepath.Base(path), manifest),
	}

	if err := e.init(ctx, path, string(src)); err != nil {
		// Auto-repair: try AutoSafe fixes and reload.
		repaired := Repair(string(src), RepairAutoSafe, scan)
		if len(repaired.Applied) > 0 {
			for _, fix := range repaired.Applied {
				fmt.Fprintf(os.Stderr, "[ext:repair] %s: applied: %s\n", filepath.Base(path), fix.Description)
			}
			if err2 := e.init(ctx, path, repaired.Source); err2 == nil {
				return e, nil
			}
		}
		return nil, err
	}
	return e, nil
}

// init creates the VM, runs the source, resolves function refs, and calls on_init.
func (e *jsExtension) init(ctx context.Context, path, src string) error {
	tctx, cancel := context.WithTimeout(ctx, extensionCallTimeout)
	defer cancel()

	vm := goja.New()
	vm.SetParserOptions()
	e.vm = vm
	e.callCtx = tctx

	setupNodeShims(vm, e.manifest)
	setupPiAPI(vm, e)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-tctx.Done():
			vm.Interrupt(tctx.Err())
		case <-done:
		}
	}()

	if _, err := vm.RunString(src); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	describeFn, ok := goja.AssertFunction(vm.Get("describe"))
	if !ok {
		return fmt.Errorf("%s: describe() not defined", path)
	}
	result, err := describeFn(goja.Undefined())
	if err != nil {
		return fmt.Errorf("%s: describe: %w", path, err)
	}
	raw, err := json.Marshal(result.Export())
	if err != nil {
		return fmt.Errorf("%s: marshal describe result: %w", path, err)
	}
	if err := json.Unmarshal(raw, &e.info); err != nil {
		return fmt.Errorf("%s: parse describe result: %w", path, err)
	}
	if e.info.Name == "" {
		return fmt.Errorf("%s: describe returned empty name", path)
	}
	// Backfill dispatcher name once we know the extension's declared name.
	e.dispatcher.extName = e.info.Name

	executeFn, ok := goja.AssertFunction(vm.Get("execute"))
	if !ok {
		return fmt.Errorf("%s: execute() not defined", path)
	}
	e.executeFn = executeFn

	if fn, ok := goja.AssertFunction(vm.Get("before_tool")); ok {
		e.beforeToolFn = fn
	}
	if fn, ok := goja.AssertFunction(vm.Get("after_tool")); ok {
		e.afterToolFn = fn
	}

	if initFn, ok := goja.AssertFunction(vm.Get("on_init")); ok {
		if _, err := e.runEventLoop(func() (goja.Value, error) {
			return initFn(goja.Undefined())
		}); err != nil {
			return fmt.Errorf("%s: on_init: %w", path, err)
		}
	}

	vm.ClearInterrupt()
	e.callCtx = nil
	return nil
}

func (e *jsExtension) Info() Info   { return e.info }
func (e *jsExtension) Close() error { return nil }

// Telemetry returns the dispatcher's recorded hostcall telemetry.
func (e *jsExtension) Telemetry() []HostcallTelemetry {
	return e.dispatcher.Telemetry()
}

// callLocked acquires the VM mutex, sets callCtx, arms context cancellation,
// and invokes fn. callCtx is cleared on return.
func (e *jsExtension) callLocked(ctx context.Context, fn func() error) error {
	e.mu.Lock()
	defer func() {
		e.callCtx = nil
		e.mu.Unlock()
	}()

	e.vm.ClearInterrupt()
	e.callCtx = ctx

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			e.vm.Interrupt(ctx.Err())
		case <-done:
		}
	}()
	defer close(done)

	return fn()
}

// runEventLoop drives the goja microtask queue until the value returned by
// startFn settles. Handles both synchronous return values and Promises from
// async functions. Must be called with the VM mutex already held (i.e. inside
// callLocked or init).
func (e *jsExtension) runEventLoop(startFn func() (goja.Value, error)) (goja.Value, error) {
	val, err := startFn()
	if err != nil {
		return goja.Undefined(), err
	}

	// Wrap in Promise.resolve so both sync results and async Promises are
	// handled uniformly.
	promiseResolve, ok := goja.AssertFunction(
		e.vm.GlobalObject().Get("Promise").ToObject(e.vm).Get("resolve"),
	)
	if !ok {
		return val, nil // no Promise support in this VM (shouldn't happen)
	}
	wrapped, err := promiseResolve(goja.Undefined(), val)
	if err != nil {
		return goja.Undefined(), err
	}

	// Attach .then/.catch to detect settlement.
	var finalVal goja.Value = goja.Undefined()
	var finalErr error
	settled := false

	thenFn, ok := goja.AssertFunction(wrapped.ToObject(e.vm).Get("then"))
	if !ok {
		return val, nil
	}
	onFulfilled := e.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		finalVal = call.Argument(0)
		settled = true
		return goja.Undefined()
	})
	onRejected := e.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		finalErr = fmt.Errorf("%s", call.Argument(0).String())
		settled = true
		return goja.Undefined()
	})
	if _, err := thenFn(goja.Undefined(), onFulfilled, onRejected); err != nil {
		return goja.Undefined(), err
	}

	// Drain microtasks one tick at a time until the Promise settles.
	// Each RunString call flushes goja's internal microtask queue.
	for !settled {
		if _, tickErr := e.vm.RunString("void 0"); tickErr != nil {
			return goja.Undefined(), tickErr
		}
	}
	return finalVal, finalErr
}

func (e *jsExtension) Execute(ctx context.Context, params json.RawMessage) (string, bool, error) {
	tctx, cancel := context.WithTimeout(ctx, extensionCallTimeout)
	defer cancel()

	if params == nil {
		params = json.RawMessage("{}")
	}

	var content string
	var isError bool

	err := e.callLocked(tctx, func() error {
		var paramVal any
		if err := json.Unmarshal(params, &paramVal); err != nil {
			return fmt.Errorf("unmarshal params: %w", err)
		}

		result, err := e.runEventLoop(func() (goja.Value, error) {
			return e.executeFn(goja.Undefined(), e.vm.ToValue(paramVal))
		})
		if err != nil {
			return err
		}

		raw, err := json.Marshal(result.Export())
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		var resp struct {
			Content string `json:"content"`
			IsError bool   `json:"is_error"`
		}
		if jsonErr := json.Unmarshal(raw, &resp); jsonErr != nil {
			if s, ok := result.Export().(string); ok {
				content = s
				return nil
			}
			return fmt.Errorf("execute must return {content, is_error}: %w", jsonErr)
		}
		content = resp.Content
		isError = resp.IsError
		return nil
	})
	if err != nil {
		return "", true, err
	}
	return content, isError, nil
}

// BeforeTool implements HookedExtension.
func (e *jsExtension) BeforeTool(ctx context.Context, name string, params json.RawMessage) {
	if e.beforeToolFn == nil {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
	defer cancel()

	var paramVal any
	_ = json.Unmarshal(params, &paramVal)

	_ = e.callLocked(tctx, func() error {
		_, err := e.beforeToolFn(goja.Undefined(), e.vm.ToValue(name), e.vm.ToValue(paramVal))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ext] %s: before_tool: %v\n", e.info.Name, err)
		}
		return nil
	})
}

// AfterTool implements HookedExtension.
func (e *jsExtension) AfterTool(ctx context.Context, name string, result string, isError bool) {
	if e.afterToolFn == nil {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
	defer cancel()

	_ = e.callLocked(tctx, func() error {
		_, err := e.afterToolFn(goja.Undefined(), e.vm.ToValue(name), e.vm.ToValue(result), e.vm.ToValue(isError))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ext] %s: after_tool: %v\n", e.info.Name, err)
		}
		return nil
	})
}

// ── pi host API ───────────────────────────────────────────────────────────────

// setupPiAPI installs the capability-gated pi global into vm.
// All pi.* functions return already-resolved Promises so extensions can use
// await without needing a real async event loop.
func setupPiAPI(vm *goja.Runtime, e *jsExtension) {
	pi := vm.NewObject()

	// pi.tool(name, params) → Promise<string>
	_ = pi.Set("tool", func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		var payload any
		if len(call.Arguments) > 1 {
			payload = call.Argument(1).Export()
		}
		ctx := e.callCtx
		if ctx == nil {
			ctx = context.Background()
		}
		req := e.dispatcher.NewRequest(HCKindTool, name, payload)
		result, isErr, err := e.dispatcher.Dispatch(ctx, req)
		return resolvedPromise(vm, result, isErr, err)
	})

	// pi.http({url, method, headers, body}) → Promise<{status, body, headers}>
	if e.manifest.has(CapNetwork) {
		_ = pi.Set("http", func(call goja.FunctionCall) goja.Value {
			opts, _ := call.Argument(0).Export().(map[string]any)
			ctx := e.callCtx
			if ctx == nil {
				ctx = context.Background()
			}
			req := e.dispatcher.NewRequest(HCKindHTTP, "", opts)
			if e.dispatcher.CheckPolicy(req) == PolicyDeny {
				return rejectedPromise(vm, fmt.Errorf("capability %q not granted", CapNetwork))
			}
			result, isErr, err := httpHostcall(ctx, opts)
			return resolvedPromise(vm, result, isErr, err)
		})
	} else {
		_ = pi.Set("http", func(call goja.FunctionCall) goja.Value {
			return rejectedPromise(vm, fmt.Errorf("capability %q not granted", CapNetwork))
		})
	}

	// pi.exec(cmd, args) → Promise<string>
	if e.manifest.has(CapExec) {
		_ = pi.Set("exec", func(call goja.FunctionCall) goja.Value {
			cmd := call.Argument(0).String()
			var args []string
			if arr, ok := call.Argument(1).Export().([]any); ok {
				for _, a := range arr {
					args = append(args, fmt.Sprint(a))
				}
			}
			payload := map[string]any{"command": cmd + " " + strings.Join(args, " ")}
			ctx := e.callCtx
			if ctx == nil {
				ctx = context.Background()
			}
			req := e.dispatcher.NewRequest(HCKindExec, "bash", payload)
			result, isErr, err := e.dispatcher.Dispatch(ctx, req)
			return resolvedPromise(vm, result, isErr, err)
		})
	} else {
		_ = pi.Set("exec", func(call goja.FunctionCall) goja.Value {
			return rejectedPromise(vm, fmt.Errorf("capability %q not granted", CapExec))
		})
	}

	// pi.env(key) → string (synchronous; always allowed via CapEnv gate)
	_ = pi.Set("env", func(call goja.FunctionCall) goja.Value {
		if !e.manifest.has(CapEnv) {
			return goja.Undefined()
		}
		key := call.Argument(0).String()
		if IsEnvBlocked(key) {
			return goja.Undefined()
		}
		return vm.ToValue(os.Getenv(key))
	})

	// pi.session(op, data) → Promise<any>  [stub: returns empty ok]
	_ = pi.Set("session", func(call goja.FunctionCall) goja.Value {
		if !e.manifest.has(CapSession) {
			return rejectedPromise(vm, fmt.Errorf("capability %q not granted", CapSession))
		}
		p, resolveFn, _ := vm.NewPromise()
		_ = resolveFn(nil)
		return vm.ToValue(p)
	})

	// pi.ui(op, data) → Promise<any>  [stub: returns empty ok]
	_ = pi.Set("ui", func(call goja.FunctionCall) goja.Value {
		if !e.manifest.has(CapUI) {
			return rejectedPromise(vm, fmt.Errorf("capability %q not granted", CapUI))
		}
		p, resolveFn, _ := vm.NewPromise()
		_ = resolveFn(nil)
		return vm.ToValue(p)
	})

	// pi.log(entry) → void  (always allowed)
	_ = pi.Set("log", func(call goja.FunctionCall) goja.Value {
		entry := call.Argument(0).Export()
		logHostcall(entry)
		return goja.Undefined()
	})

	// Legacy filesystem helpers (kept for backwards compatibility).
	if e.manifest.has(CapFSRead) || e.manifest.has(CapFSWrite) {
		allowed := e.manifest.allowedPaths()
		if e.manifest.has(CapFSRead) {
			_ = pi.Set("readFile", readFileFunc(vm, allowed))
		}
		if e.manifest.has(CapFSWrite) {
			_ = pi.Set("writeFile", writeFileFunc(vm, allowed))
		}
	}

	_ = vm.Set("pi", pi)
}

// resolvedPromise creates a Promise already resolved with result,
// or rejected if isErr or err is set.
func resolvedPromise(vm *goja.Runtime, result string, isErr bool, err error) goja.Value {
	if err != nil || isErr {
		msg := ""
		if err != nil {
			msg = err.Error()
		} else {
			msg = result
		}
		return rejectedPromise(vm, fmt.Errorf("%s", msg))
	}
	p, resolveFn, _ := vm.NewPromise()
	_ = resolveFn(result)
	return vm.ToValue(p)
}



// rejectedPromise creates a Promise pre-rejected with err.
func rejectedPromise(vm *goja.Runtime, err error) goja.Value {
	p, _, rejectFn := vm.NewPromise()
	_ = rejectFn(vm.NewGoError(err))
	return vm.ToValue(p)
}

// ── HTTP hostcall ─────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 30 * time.Second}

func httpHostcall(ctx context.Context, opts map[string]any) (string, bool, error) {
	url, _ := opts["url"].(string)
	if url == "" {
		return "", true, fmt.Errorf("pi.http: url is required")
	}
	method := "GET"
	if m, ok := opts["method"].(string); ok && m != "" {
		method = strings.ToUpper(m)
	}

	var bodyReader *strings.Reader
	if body, ok := opts["body"].(string); ok && body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return "", true, fmt.Errorf("pi.http: %w", err)
	}
	if headers, ok := opts["headers"].(map[string]any); ok {
		for k, v := range headers {
			req.Header.Set(k, fmt.Sprint(v))
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("pi.http: %w", err)
	}
	defer resp.Body.Close()

	var buf strings.Builder
	fmt.Fprintf(&buf, "HTTP %d\n", resp.StatusCode)
	for k, vs := range resp.Header {
		fmt.Fprintf(&buf, "%s: %s\n", k, strings.Join(vs, ", "))
	}
	buf.WriteString("\n")

	var body [1 << 20]byte // 1 MB cap
	n, _ := resp.Body.Read(body[:])
	buf.Write(body[:n])

	isErr := resp.StatusCode >= 400
	return buf.String(), isErr, nil
}

// ── legacy host API functions (kept for compatibility) ────────────────────────

func readFileFunc(vm *goja.Runtime, allowed []string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		path := call.Argument(0).String()
		if !pathAllowed(path, allowed) {
			panic(vm.NewGoError(fmt.Errorf("pi.readFile: path %q not in allowed paths", path)))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(string(data))
	}
}

func writeFileFunc(vm *goja.Runtime, allowed []string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		path := call.Argument(0).String()
		content := call.Argument(1).String()
		if !pathAllowed(path, allowed) {
			panic(vm.NewGoError(fmt.Errorf("pi.writeFile: path %q not in allowed paths", path)))
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	}
}

// pathAllowed reports whether path is under one of the allowed prefixes.
func pathAllowed(path string, allowed []string) bool {
	cleanPath := filepath.Clean(path)
	for _, prefix := range allowed {
		cleanPrefix := filepath.Clean(prefix)
		if cleanPath == cleanPrefix || strings.HasPrefix(cleanPath, cleanPrefix+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}
