package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
type jsExtension struct {
	info     Info
	src      string
	path     string
	manifest Manifest

	hasOnInit    bool
	hasBeforeTool bool
	hasAfterTool  bool
}

func loadJS(ctx context.Context, path string) (*jsExtension, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	manifest := loadManifest(path)
	e := &jsExtension{src: string(src), path: path, manifest: manifest}
	if err := e.runInit(ctx); err != nil {
		return nil, err
	}
	if e.info.Name == "" {
		return nil, fmt.Errorf("describe: extension returned empty name")
	}
	return e, nil
}

func (e *jsExtension) Info() Info  { return e.info }
func (e *jsExtension) Close() error { return nil }

// runInit creates one VM at load time to: call describe(), probe hook existence,
// and call on_init() if defined. Avoids running the source multiple times.
func (e *jsExtension) runInit(ctx context.Context) error {
	tctx, cancel := context.WithTimeout(ctx, extensionCallTimeout)
	defer cancel()

	vm, err := e.newVM(tctx)
	if err != nil {
		return err
	}
	if _, err := vm.RunString(e.src); err != nil {
		return fmt.Errorf("%s: %w", e.path, err)
	}

	describeFn, ok := goja.AssertFunction(vm.Get("describe"))
	if !ok {
		return fmt.Errorf("%s: describe() not defined", e.path)
	}
	result, err := describeFn(goja.Undefined())
	if err != nil {
		return fmt.Errorf("%s: describe: %w", e.path, err)
	}
	raw, err := json.Marshal(result.Export())
	if err != nil {
		return fmt.Errorf("%s: marshal describe result: %w", e.path, err)
	}
	if err := json.Unmarshal(raw, &e.info); err != nil {
		return fmt.Errorf("%s: parse describe result: %w", e.path, err)
	}

	e.hasOnInit = isFunction(vm, "on_init")
	e.hasBeforeTool = isFunction(vm, "before_tool")
	e.hasAfterTool = isFunction(vm, "after_tool")

	if e.hasOnInit {
		initFn, _ := goja.AssertFunction(vm.Get("on_init"))
		if _, err := initFn(goja.Undefined()); err != nil {
			return fmt.Errorf("%s: on_init: %w", e.path, err)
		}
	}

	return nil
}

func (e *jsExtension) Execute(ctx context.Context, params json.RawMessage) (string, bool, error) {
	tctx, cancel := context.WithTimeout(ctx, extensionCallTimeout)
	defer cancel()

	vm, err := e.newVM(tctx)
	if err != nil {
		return "", true, err
	}
	if _, err := vm.RunString(e.src); err != nil {
		return "", true, fmt.Errorf("%s: %w", e.path, err)
	}

	executeFn, ok := goja.AssertFunction(vm.Get("execute"))
	if !ok {
		return "", true, fmt.Errorf("%s: execute() not defined", e.path)
	}

	var paramVal any
	if err := json.Unmarshal(params, &paramVal); err != nil {
		return "", true, fmt.Errorf("unmarshal params: %w", err)
	}

	result, err := executeFn(goja.Undefined(), vm.ToValue(paramVal))
	if err != nil {
		return "", true, fmt.Errorf("%s: execute: %w", e.path, err)
	}

	raw, err := json.Marshal(result.Export())
	if err != nil {
		return "", true, fmt.Errorf("%s: marshal result: %w", e.path, err)
	}
	var resp struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		if s, ok := result.Export().(string); ok {
			return s, false, nil
		}
		return "", true, fmt.Errorf("%s: execute must return {content, is_error}: %w", e.path, err)
	}
	return resp.Content, resp.IsError, nil
}

// BeforeTool implements HookedExtension. Errors are logged and ignored.
func (e *jsExtension) BeforeTool(ctx context.Context, name string, params json.RawMessage) {
	if !e.hasBeforeTool {
		return
	}
	var paramVal any
	_ = json.Unmarshal(params, &paramVal)
	e.runHook(ctx, "before_tool", name, paramVal)
}

// AfterTool implements HookedExtension. Errors are logged and ignored.
func (e *jsExtension) AfterTool(ctx context.Context, name string, result string, isError bool) {
	if !e.hasAfterTool {
		return
	}
	e.runHook(ctx, "after_tool", name, result, isError)
}

// runHook creates a short-lived VM to call a lifecycle hook function.
// Failures are written to stderr and do not propagate — hooks are best-effort.
func (e *jsExtension) runHook(ctx context.Context, fnName string, args ...any) {
	tctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
	defer cancel()

	vm, err := e.newVM(tctx)
	if err != nil {
		return
	}
	if _, err := vm.RunString(e.src); err != nil {
		return
	}
	fn, ok := goja.AssertFunction(vm.Get(fnName))
	if !ok {
		return
	}
	gojaArgs := make([]goja.Value, len(args))
	for i, a := range args {
		gojaArgs[i] = vm.ToValue(a)
	}
	if _, err := fn(goja.Undefined(), gojaArgs...); err != nil {
		fmt.Fprintf(os.Stderr, "[ext] %s: %s: %v\n", e.path, fnName, err)
	}
}

// newVM constructs a goja runtime wired with context cancellation, Node-compatible
// shims (console, process, require), and the capability-gated pi host API.
func (e *jsExtension) newVM(ctx context.Context) (*goja.Runtime, error) {
	vm := goja.New()
	vm.SetParserOptions()

	go func() {
		<-ctx.Done()
		vm.Interrupt(ctx.Err())
	}()

	setupNodeShims(vm, e.manifest)

	piObj := vm.NewObject()

	if e.manifest.has(CapFSRead) || e.manifest.has(CapFSWrite) {
		allowed := e.manifest.allowedPaths()
		if e.manifest.has(CapFSRead) {
			if err := piObj.Set("readFile", readFileFunc(vm, allowed)); err != nil {
				return nil, err
			}
		}
		if e.manifest.has(CapFSWrite) {
			if err := piObj.Set("writeFile", writeFileFunc(vm, allowed)); err != nil {
				return nil, err
			}
		}
	}

	if e.manifest.has(CapEnv) {
		if err := piObj.Set("env", envFunc(vm)); err != nil {
			return nil, err
		}
	}

	if err := vm.Set("pi", piObj); err != nil {
		return nil, err
	}
	return vm, nil
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

func isFunction(vm *goja.Runtime, name string) bool {
	_, ok := goja.AssertFunction(vm.Get(name))
	return ok
}
