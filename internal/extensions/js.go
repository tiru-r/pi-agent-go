package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
)

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
// The host injects a `pi` global whose available methods depend on the
// extension's declared capabilities (see policy.go).
type jsExtension struct {
	info     Info
	src      string   // JS source loaded at startup
	path     string   // file path (for error messages)
	manifest Manifest
}

func loadJS(ctx context.Context, path string) (*jsExtension, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	manifest := loadManifest(path)
	e := &jsExtension{src: string(src), path: path, manifest: manifest}

	info, err := e.runDescribe(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe: %w", err)
	}
	if info.Name == "" {
		return nil, fmt.Errorf("describe: extension returned empty name")
	}
	e.info = info
	return e, nil
}

func (e *jsExtension) Info() Info { return e.info }
func (e *jsExtension) Close() error { return nil }

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

	// Unmarshal params into a plain Go value so goja gets a native object.
	var paramVal any
	if err := json.Unmarshal(params, &paramVal); err != nil {
		return "", true, fmt.Errorf("unmarshal params: %w", err)
	}

	result, err := executeFn(goja.Undefined(), vm.ToValue(paramVal))
	if err != nil {
		return "", true, fmt.Errorf("%s: execute: %w", e.path, err)
	}

	// Marshal the JS return value back to JSON then into our response struct.
	raw, err := json.Marshal(result.Export())
	if err != nil {
		return "", true, fmt.Errorf("%s: marshal result: %w", e.path, err)
	}
	var resp struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		// If the extension returned a plain string, use it directly.
		if s, ok := result.Export().(string); ok {
			return s, false, nil
		}
		return "", true, fmt.Errorf("%s: execute must return {content, is_error}: %w", e.path, err)
	}
	return resp.Content, resp.IsError, nil
}

// runDescribe creates a fresh VM, evaluates the source, calls describe(), and
// returns the parsed Info.
func (e *jsExtension) runDescribe(ctx context.Context) (Info, error) {
	tctx, cancel := context.WithTimeout(ctx, extensionCallTimeout)
	defer cancel()

	vm, err := e.newVM(tctx)
	if err != nil {
		return Info{}, err
	}
	if _, err := vm.RunString(e.src); err != nil {
		return Info{}, fmt.Errorf("%s: %w", e.path, err)
	}

	describeFn, ok := goja.AssertFunction(vm.Get("describe"))
	if !ok {
		return Info{}, fmt.Errorf("%s: describe() not defined", e.path)
	}
	result, err := describeFn(goja.Undefined())
	if err != nil {
		return Info{}, fmt.Errorf("%s: describe: %w", e.path, err)
	}

	raw, err := json.Marshal(result.Export())
	if err != nil {
		return Info{}, fmt.Errorf("%s: marshal describe result: %w", e.path, err)
	}
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil {
		return Info{}, fmt.Errorf("%s: parse describe result: %w", e.path, err)
	}
	return info, nil
}

// newVM constructs a goja runtime wired with context cancellation and the
// capability-gated `pi` host API.
func (e *jsExtension) newVM(ctx context.Context) (*goja.Runtime, error) {
	vm := goja.New()

	// Cancel the VM when ctx is done.
	vm.SetParserOptions()
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-stop:
		}
	}()
	// We close stop when the VM is done via defer in callers — keep it simple
	// by leaking the goroutine for the (short) extension call duration.
	_ = stop

	// Expose a `pi` global with capability-gated host functions.
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
