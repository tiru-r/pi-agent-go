package extensions

import (
	"context"
	"encoding/json"
	"fmt"
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
// A JS extension must export two functions at the top level:
//
//	function describe() {
//	    return { name: "my-tool", description: "…", schema: { … } };
//	}
//
//	function execute(params) {
//	    return { content: "result", is_error: false };
//	}
//
// Optional lifecycle hooks (any subset may be defined):
//
//	function on_init()                            // called once after load
//	function before_tool(name, params)            // called before any tool runs
//	function after_tool(name, result, is_error)   // called after any tool runs
//
// The host injects console, process, require('path'/'os'), and a capability-gated
// pi global (see policy.go and shims.go).
//
// A single persistent goja.Runtime is shared across all calls and protected by a
// mutex (goja is not goroutine-safe). State set in on_init is visible to all
// subsequent Execute, before_tool, and after_tool calls.
type jsExtension struct {
	info     Info
	manifest Manifest

	mu sync.Mutex    // serialises all VM access; goja is not goroutine-safe
	vm *goja.Runtime

	// pre-resolved function references (valid for the VM's lifetime)
	executeFn    goja.Callable
	beforeToolFn goja.Callable // nil if not defined
	afterToolFn  goja.Callable // nil if not defined
}

func loadJS(ctx context.Context, path string) (*jsExtension, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	manifest := loadManifest(path)
	e := &jsExtension{manifest: manifest}
	if err := e.init(ctx, path, string(src)); err != nil {
		return nil, err
	}
	return e, nil
}

// init creates the VM, runs the source once, resolves all function references,
// and calls on_init if defined. The VM is retained for the extension's lifetime.
// No locking is needed here because init is only called from loadJS.
func (e *jsExtension) init(ctx context.Context, path, src string) error {
	tctx, cancel := context.WithTimeout(ctx, extensionCallTimeout)
	defer cancel()

	vm := newGojaVM(e.manifest)
	e.vm = vm

	// Interrupt goroutine exits via done channel so it cannot fire after init returns.
	done := make(chan struct{})
	defer close(done) // runs before cancel() (LIFO), so goroutine stops first
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
		if _, err := initFn(goja.Undefined()); err != nil {
			return fmt.Errorf("%s: on_init: %w", path, err)
		}
	}

	// Clear any interrupt that may have been set if tctx neared its deadline.
	vm.ClearInterrupt()
	return nil
}

func (e *jsExtension) Info() Info   { return e.info }
func (e *jsExtension) Close() error { return nil }

// callLocked acquires the VM mutex, clears any stale interrupt from a prior
// cancelled call, arms context-driven cancellation for this call, and invokes fn.
func (e *jsExtension) callLocked(ctx context.Context, fn func() error) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.vm.ClearInterrupt() // clear any stale interrupt from a previous cancelled call

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
		result, err := e.executeFn(goja.Undefined(), e.vm.ToValue(paramVal))
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

// BeforeTool implements HookedExtension. Errors are logged; they do not propagate.
func (e *jsExtension) BeforeTool(ctx context.Context, name string, params json.RawMessage) {
	if e.beforeToolFn == nil {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
	defer cancel()

	var paramVal any
	_ = json.Unmarshal(params, &paramVal)

	_ = e.callLocked(tctx, func() error {
		if _, err := e.beforeToolFn(goja.Undefined(), e.vm.ToValue(name), e.vm.ToValue(paramVal)); err != nil {
			fmt.Fprintf(os.Stderr, "[ext] %s: before_tool: %v\n", e.info.Name, err)
		}
		return nil
	})
}

// AfterTool implements HookedExtension. Errors are logged; they do not propagate.
func (e *jsExtension) AfterTool(ctx context.Context, name string, result string, isError bool) {
	if e.afterToolFn == nil {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
	defer cancel()

	_ = e.callLocked(tctx, func() error {
		if _, err := e.afterToolFn(goja.Undefined(), e.vm.ToValue(name), e.vm.ToValue(result), e.vm.ToValue(isError)); err != nil {
			fmt.Fprintf(os.Stderr, "[ext] %s: after_tool: %v\n", e.info.Name, err)
		}
		return nil
	})
}

// newGojaVM constructs a goja runtime with Node-compatible shims and the
// capability-gated pi host API. Context cancellation is managed per-call via
// callLocked; no interrupt goroutine is installed at VM creation time.
func newGojaVM(manifest Manifest) *goja.Runtime {
	vm := goja.New()
	vm.SetParserOptions()
	setupNodeShims(vm, manifest)

	piObj := vm.NewObject()

	if manifest.has(CapFSRead) || manifest.has(CapFSWrite) {
		allowed := manifest.allowedPaths()
		if manifest.has(CapFSRead) {
			_ = piObj.Set("readFile", readFileFunc(vm, allowed))
		}
		if manifest.has(CapFSWrite) {
			_ = piObj.Set("writeFile", writeFileFunc(vm, allowed))
		}
	}
	if manifest.has(CapEnv) {
		_ = piObj.Set("env", envFunc(vm))
	}

	_ = vm.Set("pi", piObj)
	return vm
}

// ── host API functions ────────────────────────────────────────────────────────

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

func envFunc(vm *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(os.Getenv(call.Argument(0).String()))
	}
}

// pathAllowed reports whether path is under one of the allowed prefixes.
// Both path and prefix are cleaned with filepath.Clean before comparison to
// prevent traversal attacks via ".." or redundant separators.
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
