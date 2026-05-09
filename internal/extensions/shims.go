package extensions

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/dop251/goja"
)

// setupNodeShims installs console, process, and a limited require() into vm,
// giving JS extensions a minimal Node-compatible surface without spawning Node.
// process.env access is gated by the extension's CapEnv capability.
func setupNodeShims(vm *goja.Runtime, manifest Manifest) {
	setupConsole(vm)
	setupProcess(vm, manifest)
	setupRequire(vm)
}

func setupConsole(vm *goja.Runtime) {
	cons := vm.NewObject()
	_ = cons.Set("log", consoleWriter(vm, "LOG"))
	_ = cons.Set("info", consoleWriter(vm, "INF"))
	_ = cons.Set("warn", consoleWriter(vm, "WRN"))
	_ = cons.Set("error", consoleWriter(vm, "ERR"))
	_ = vm.Set("console", cons)
}

func consoleWriter(_ *goja.Runtime, level string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.String()
		}
		fmt.Fprintf(os.Stderr, "[ext:%s] %s\n", level, strings.Join(parts, " "))
		return goja.Undefined()
	}
}

func setupProcess(vm *goja.Runtime, manifest Manifest) {
	proc := vm.NewObject()
	_ = proc.Set("platform", goruntime.GOOS)
	_ = proc.Set("version", "v18.0.0") // Node compatibility stub

	if manifest.has(CapEnv) {
		env := vm.NewObject()
		_ = env.Set("get", func(call goja.FunctionCall) goja.Value {
			return vm.ToValue(os.Getenv(call.Argument(0).String()))
		})
		// Populate declared env vars so extensions can do process.env.PATH etc.
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				_ = env.Set(k, v)
			}
		}
		_ = proc.Set("env", env)
	} else {
		_ = proc.Set("env", vm.NewObject()) // empty: no capability
	}

	_ = proc.Set("exit", func(call goja.FunctionCall) goja.Value {
		code := 0
		if len(call.Arguments) > 0 {
			code = int(call.Argument(0).ToInteger())
		}
		vm.Interrupt(fmt.Errorf("process.exit(%d)", code))
		return goja.Undefined()
	})

	_ = vm.Set("process", proc)
}

func setupRequire(vm *goja.Runtime) {
	_ = vm.Set("require", func(call goja.FunctionCall) goja.Value {
		mod := call.Argument(0).String()
		switch mod {
		case "path":
			return pathModule(vm)
		case "os":
			return osModule(vm)
		default:
			panic(vm.NewGoError(fmt.Errorf("require: module %q not available (built-ins: 'path', 'os')", mod)))
		}
	})
}

func pathModule(vm *goja.Runtime) goja.Value {
	obj := vm.NewObject()
	_ = obj.Set("sep", string(os.PathSeparator))
	_ = obj.Set("join", func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.String()
		}
		return vm.ToValue(filepath.Join(parts...))
	})
	_ = obj.Set("dirname", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(filepath.Dir(call.Argument(0).String()))
	})
	_ = obj.Set("basename", func(call goja.FunctionCall) goja.Value {
		p := filepath.Base(call.Argument(0).String())
		if len(call.Arguments) > 1 {
			p = strings.TrimSuffix(p, call.Argument(1).String())
		}
		return vm.ToValue(p)
	})
	_ = obj.Set("extname", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(filepath.Ext(call.Argument(0).String()))
	})
	_ = obj.Set("resolve", func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.String()
		}
		abs, _ := filepath.Abs(filepath.Join(parts...))
		return vm.ToValue(abs)
	})
	return obj
}

func osModule(vm *goja.Runtime) goja.Value {
	eol := "\n"
	if goruntime.GOOS == "windows" {
		eol = "\r\n"
	}
	obj := vm.NewObject()
	_ = obj.Set("EOL", eol)
	_ = obj.Set("platform", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(goruntime.GOOS)
	})
	_ = obj.Set("homedir", func(call goja.FunctionCall) goja.Value {
		dir, _ := os.UserHomeDir()
		return vm.ToValue(dir)
	})
	_ = obj.Set("tmpdir", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(os.TempDir())
	})
	return obj
}
