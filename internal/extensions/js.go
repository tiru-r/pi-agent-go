package extensions

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	quickjs "github.com/aperturerobotics/go-quickjs-wasi-reactor/wazero-quickjs"
	"github.com/tetratelabs/wazero"
)

const hookCallTimeout = 5 * time.Second

// global compilation cache: shared across all extension instances so the WASM
// module is only compiled once per process.
var (
	compileCacheOnce sync.Once
	compileCache     wazero.CompilationCache
)

func getCompileCache() wazero.CompilationCache {
	compileCacheOnce.Do(func() { compileCache = wazero.NewCompilationCache() })
	return compileCache
}

// jsExtension runs a JavaScript extension via QuickJS-WASI (WebAssembly +
// wazero). Each call to Execute/BeforeTool/AfterTool creates a fresh QuickJS
// instance; the WASM module is compiled once per process via the global cache.
//
// Communication between Go and JS uses a synchronous JSON-over-pipes RPC:
//   - JS writes  "HC:{json}\n"     to stdout  → Go processes the hostcall
//   - Go writes  "{json}\n"        to stdin   → JS reads via std.in.getline()
//   - JS writes  "DESCRIBE:{json}" or "RESULT:{json}" to signal completion
type jsExtension struct {
	info      Info
	manifest  Manifest
	src       string
	hasBefore bool
	hasAfter  bool
	dispatcher *HostcallDispatcher
}

func loadJS(ctx context.Context, path string) (*jsExtension, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	scan := Scan(string(src))
	if len(scan.ForbiddenPatterns) > 0 {
		return nil, fmt.Errorf("%s: forbidden patterns: %s", path, strings.Join(scan.ForbiddenPatterns, ", "))
	}
	if unavail := scan.UnavailableImports(); len(unavail) > 0 {
		fmt.Fprintf(os.Stderr, "[ext] %s: unavailable require() modules: %s\n",
			filepath.Base(path), strings.Join(unavail, ", "))
	}
	manifest := loadManifest(path)
	e := &jsExtension{
		manifest:   manifest,
		src:        string(src),
		dispatcher: NewHostcallDispatcher(filepath.Base(path), manifest),
	}
	if err := e.init(ctx, path); err != nil {
		repaired := Repair(string(src), RepairAutoSafe, scan)
		if len(repaired.Applied) > 0 {
			for _, fix := range repaired.Applied {
				fmt.Fprintf(os.Stderr, "[ext:repair] %s: applied: %s\n", filepath.Base(path), fix.Description)
			}
			e.src = repaired.Source
			if err2 := e.init(ctx, path); err2 == nil {
				return e, nil
			}
		}
		return nil, err
	}
	return e, nil
}

func (e *jsExtension) Info() Info   { return e.info }
func (e *jsExtension) Close() error { return nil }

func (e *jsExtension) Telemetry() []HostcallTelemetry {
	return e.dispatcher.Telemetry()
}

// ── init ──────────────────────────────────────────────────────────────────────

// initHarness calls on_init (if defined), then describe(), and writes the
// result as a DESCRIBE: line to stdout.
const initHarness = `
;(function(){
  var _done=function(){
    var d=describe();
    std.out.puts('DESCRIBE:'+JSON.stringify({info:d,flags:{hasBefore:typeof before_tool==='function',hasAfter:typeof after_tool==='function'}})+'\n');
    std.out.flush();
  };
  if(typeof on_init==='function'){Promise.resolve(on_init()).then(_done,_done);}else{_done();}
})();
`

type initOutput struct {
	Info  Info `json:"info"`
	Flags struct {
		HasBefore bool `json:"hasBefore"`
		HasAfter  bool `json:"hasAfter"`
	} `json:"flags"`
}

func (e *jsExtension) init(ctx context.Context, path string) error {
	tctx, cancel := context.WithTimeout(ctx, extensionCallTimeout)
	defer cancel()

	var out initOutput
	err := e.runScript(tctx, e.src+"\n"+initHarness, func(line string) (bool, error) {
		if rest, ok := strings.CutPrefix(line, "DESCRIBE:"); ok {
			if jErr := json.Unmarshal([]byte(rest), &out); jErr != nil {
				return false, fmt.Errorf("parse describe: %w", jErr)
			}
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if out.Info.Name == "" {
		return fmt.Errorf("%s: describe() returned empty name", path)
	}
	e.info = out.Info
	e.hasBefore = out.Flags.HasBefore
	e.hasAfter = out.Flags.HasAfter
	e.dispatcher.extName = e.info.Name
	return nil
}

// ── Execute ───────────────────────────────────────────────────────────────────

func (e *jsExtension) Execute(ctx context.Context, params json.RawMessage) (string, bool, error) {
	tctx, cancel := context.WithTimeout(ctx, extensionCallTimeout)
	defer cancel()
	if params == nil {
		params = json.RawMessage("{}")
	}
	paramsLit, _ := json.Marshal(string(params))
	harness := fmt.Sprintf(`;(function(){
  var __p=JSON.parse(%s);
  Promise.resolve(execute(__p)).then(function(__r){
    std.out.puts('RESULT:'+JSON.stringify(__r)+'\n');std.out.flush();
  },function(__e){
    std.out.puts('RESULT:'+JSON.stringify({content:String(__e),is_error:true})+'\n');std.out.flush();
  });
})();`, paramsLit)

	var content string
	var isError bool
	var gotResult bool

	err := e.runScript(tctx, e.src+"\n"+harness, func(line string) (bool, error) {
		if rest, ok := strings.CutPrefix(line, "RESULT:"); ok {
			var resp struct {
				Content string `json:"content"`
				IsError bool   `json:"is_error"`
			}
			if jErr := json.Unmarshal([]byte(rest), &resp); jErr == nil {
				content, isError, gotResult = resp.Content, resp.IsError, true
			}
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return "", true, err
	}
	if !gotResult {
		return "", true, fmt.Errorf("execute() did not produce a result")
	}
	return content, isError, nil
}

// ── hooks ─────────────────────────────────────────────────────────────────────

func (e *jsExtension) BeforeTool(ctx context.Context, name string, params json.RawMessage) {
	if !e.hasBefore {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
	defer cancel()
	var paramVal any
	_ = json.Unmarshal(params, &paramVal)
	nameJ, _ := json.Marshal(name)
	paramsJ, _ := json.Marshal(paramVal)
	h := fmt.Sprintf(`;if(typeof before_tool==='function'){Promise.resolve(before_tool(%s,%s)).then(null,function(e){std.err.puts('[ext:before_tool] '+String(e)+'\n');});}std.out.puts('DONE\n');std.out.flush();`,
		nameJ, paramsJ)
	_ = e.runScript(tctx, e.src+"\n"+h, func(l string) (bool, error) { return l == "DONE", nil })
}

func (e *jsExtension) AfterTool(ctx context.Context, name string, result string, isError bool) {
	if !e.hasAfter {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
	defer cancel()
	nameJ, _ := json.Marshal(name)
	resultJ, _ := json.Marshal(result)
	isErrJ, _ := json.Marshal(isError)
	h := fmt.Sprintf(`;if(typeof after_tool==='function'){Promise.resolve(after_tool(%s,%s,%s)).then(null,function(e){std.err.puts('[ext:after_tool] '+String(e)+'\n');});}std.out.puts('DONE\n');std.out.flush();`,
		nameJ, resultJ, isErrJ)
	_ = e.runScript(tctx, e.src+"\n"+h, func(l string) (bool, error) { return l == "DONE", nil })
}

// ── RPC engine ────────────────────────────────────────────────────────────────

// hcReq is a JSON hostcall request written by JS to stdout as "HC:{json}\n".
type hcReq struct {
	K       string   `json:"k"`
	Name    string   `json:"name,omitempty"`
	Params  any      `json:"params,omitempty"`
	Opts    any      `json:"opts,omitempty"`
	Cmd     string   `json:"cmd,omitempty"`
	Args    []string `json:"args,omitempty"`
	Key     string   `json:"key,omitempty"`
	Entry   any      `json:"entry,omitempty"`
	Path    string   `json:"path,omitempty"`
	Content string   `json:"content,omitempty"`
}

// hcResp is the JSON response Go writes to stdin; JS reads via std.in.getline().
type hcResp struct {
	Result string `json:"result,omitempty"`
	Err    string `json:"err,omitempty"`
}

// runScript creates a fresh QuickJS WASM instance, evaluates the preamble +
// code, drives the event loop, and calls onLine for every non-HC stdout line.
// onLine returns (done, err); if done is true, runScript returns immediately.
func (e *jsExtension) runScript(ctx context.Context, code string, onLine func(string) (bool, error)) error {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	defer stdinR.Close()
	defer stdoutR.Close()

	r := wazero.NewRuntimeWithConfig(ctx,
		wazero.NewRuntimeConfigInterpreter().WithCompilationCache(getCompileCache()))
	defer r.Close(ctx)

	cfg := wazero.NewModuleConfig().
		WithStdin(stdinR).
		WithStdout(stdoutW).
		WithStderr(os.Stderr)

	qjs, err := quickjs.NewQuickJS(ctx, r, cfg)
	if err != nil {
		return fmt.Errorf("quickjs: %w", err)
	}
	defer qjs.Close(ctx)

	if err := qjs.Init(ctx, []string{"qjs", "--std"}); err != nil {
		return fmt.Errorf("quickjs init: %w", err)
	}

	preamble := buildPreamble(e.manifest)

	wasmDone := make(chan error, 1)
	go func() {
		defer stdoutW.Close()
		if err := qjs.Eval(ctx, preamble+"\n"+code, false); err != nil {
			wasmDone <- err
			return
		}
		wasmDone <- qjs.RunLoop(ctx)
	}()

	scanner := bufio.NewScanner(stdoutR)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1 MB max line
	var processErr error
	var resultDone bool
	for !resultDone && scanner.Scan() {
		line := scanner.Text()
		if rest, ok := strings.CutPrefix(line, "HC:"); ok {
			var req hcReq
			if jErr := json.Unmarshal([]byte(rest), &req); jErr != nil {
				writeHCResp(stdinW, hcResp{Err: "parse error"})
				continue
			}
			writeHCResp(stdinW, e.handleHC(ctx, &req))
			continue
		}
		done, callErr := onLine(line)
		if callErr != nil {
			processErr = callErr
			break
		}
		if done {
			resultDone = true
		}
	}

	stdinW.Close()
	io.Copy(io.Discard, stdoutR) // let WASM goroutine finish
	wasmErr := <-wasmDone

	if processErr != nil {
		return processErr
	}
	if resultDone {
		return nil // ignore post-result cleanup errors
	}
	return wasmErr
}

func writeHCResp(w io.Writer, resp hcResp) {
	b, _ := json.Marshal(resp)
	w.Write(append(b, '\n'))
}

// handleHC dispatches a single hostcall request from the JS extension.
func (e *jsExtension) handleHC(ctx context.Context, req *hcReq) hcResp {
	switch req.K {
	case "tool":
		r := e.dispatcher.NewRequest(HCKindTool, req.Name, req.Params)
		res, isErr, err := e.dispatcher.Dispatch(ctx, r)
		if err != nil {
			return hcResp{Err: err.Error()}
		}
		if isErr {
			return hcResp{Err: res}
		}
		return hcResp{Result: res}

	case "http":
		if !e.manifest.has(CapNetwork) {
			return hcResp{Err: fmt.Sprintf("capability %q not granted", CapNetwork)}
		}
		opts, _ := req.Opts.(map[string]any)
		res, isErr, err := httpHostcall(ctx, opts)
		if err != nil {
			return hcResp{Err: err.Error()}
		}
		if isErr {
			return hcResp{Err: res}
		}
		return hcResp{Result: res}

	case "exec":
		if !e.manifest.has(CapExec) {
			return hcResp{Err: fmt.Sprintf("capability %q not granted", CapExec)}
		}
		payload := map[string]any{"command": req.Cmd + " " + strings.Join(req.Args, " ")}
		r := e.dispatcher.NewRequest(HCKindExec, "bash", payload)
		res, isErr, err := e.dispatcher.Dispatch(ctx, r)
		if err != nil {
			return hcResp{Err: err.Error()}
		}
		if isErr {
			return hcResp{Err: res}
		}
		return hcResp{Result: res}

	case "env":
		if !e.manifest.has(CapEnv) || IsEnvBlocked(req.Key) {
			return hcResp{}
		}
		return hcResp{Result: os.Getenv(req.Key)}

	case "readFile":
		if !e.manifest.has(CapFSRead) {
			return hcResp{Err: fmt.Sprintf("capability %q not granted", CapFSRead)}
		}
		if !pathAllowed(req.Path, e.manifest.allowedPaths()) {
			return hcResp{Err: fmt.Sprintf("path %q not in allowed paths", req.Path)}
		}
		data, err := os.ReadFile(req.Path)
		if err != nil {
			return hcResp{Err: err.Error()}
		}
		return hcResp{Result: string(data)}

	case "writeFile":
		if !e.manifest.has(CapFSWrite) {
			return hcResp{Err: fmt.Sprintf("capability %q not granted", CapFSWrite)}
		}
		if !pathAllowed(req.Path, e.manifest.allowedPaths()) {
			return hcResp{Err: fmt.Sprintf("path %q not in allowed paths", req.Path)}
		}
		if err := os.WriteFile(req.Path, []byte(req.Content), 0o644); err != nil {
			return hcResp{Err: err.Error()}
		}
		return hcResp{}

	case "log":
		logHostcall(req.Entry)
		return hcResp{}

	case "session", "ui":
		return hcResp{} // stubs

	default:
		return hcResp{Err: fmt.Sprintf("unknown hostcall kind %q", req.K)}
	}
}

// ── preamble ──────────────────────────────────────────────────────────────────

// buildPreamble returns JS code that sets up console, process, require, and the
// pi API. It is prepended to every extension before evaluation.
//
// stdout is reserved for the RPC protocol; console output goes to stderr.
func buildPreamble(manifest Manifest) string {
	var b strings.Builder

	// console → stderr so stdout stays clean for our RPC protocol.
	b.WriteString(`;(function(){
var _w=function(lvl){return function(){std.err.puts('[ext:'+lvl+'] '+Array.prototype.slice.call(arguments).join(' ')+'\n');};};
console={log:_w('LOG'),info:_w('INF'),warn:_w('WRN'),error:_w('ERR')};
print=function(){std.err.puts(Array.prototype.slice.call(arguments).join(' ')+'\n');};
`)

	// Synchronous JSON-over-pipes RPC bridge.
	b.WriteString(`
var __pi_rpc=function(req){
  std.out.puts('HC:'+JSON.stringify(req)+'\n');
  std.out.flush();
  var resp=std.in.getline();
  if(resp===null)throw new Error('pi: stdin closed');
  var r=JSON.parse(resp);
  if(r.err)throw new Error(r.err);
  return r;
};
`)

	// pi API.
	b.WriteString(`
pi={
  tool:function(name,params){try{var r=__pi_rpc({k:'tool',name:name,params:params||{}});return Promise.resolve(r.result||'');}catch(e){return Promise.reject(e);}},
  http:function(opts){try{var r=__pi_rpc({k:'http',opts:opts});return Promise.resolve(r.result||'');}catch(e){return Promise.reject(e);}},
  exec:function(cmd,args){try{var r=__pi_rpc({k:'exec',cmd:cmd,args:args||[]});return Promise.resolve(r.result||'');}catch(e){return Promise.reject(e);}},
  env:function(key){try{return __pi_rpc({k:'env',key:key}).result||'';}catch(e){return '';}},
  session:function(){return Promise.resolve(null);},
  ui:function(){return Promise.resolve(null);},
  log:function(entry){try{__pi_rpc({k:'log',entry:entry});}catch(e){}},
`)
	if manifest.has(CapFSRead) {
		b.WriteString(`  readFile:function(path){var r=__pi_rpc({k:'readFile',path:path});if(r.err)throw new Error(r.err);return r.result||'';},`)
	}
	if manifest.has(CapFSWrite) {
		b.WriteString(`  writeFile:function(path,content){var r=__pi_rpc({k:'writeFile',path:path,content:content});if(r.err)throw new Error(r.err);},`)
	}
	b.WriteString(`};`)

	// process shim.
	envObj := "{}"
	if manifest.has(CapEnv) {
		var parts []string
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok && !IsEnvBlocked(k) {
				kb, _ := json.Marshal(k)
				vb, _ := json.Marshal(v)
				parts = append(parts, string(kb)+":"+string(vb))
			}
		}
		envObj = "{" + strings.Join(parts, ",") + "}"
	}
	platJ, _ := json.Marshal(goruntime.GOOS)
	fmt.Fprintf(&b, "\nprocess={platform:%s,version:'v18.0.0',env:%s,exit:function(c){std.exit(c||0);}};\n", platJ, envObj)

	// require shim (path + os modules).
	sep := string(os.PathSeparator)
	sepJ, _ := json.Marshal(sep)
	home, _ := os.UserHomeDir()
	homeJ, _ := json.Marshal(home)
	tmpJ, _ := json.Marshal(os.TempDir())
	eol := "\n"
	if goruntime.GOOS == "windows" {
		eol = "\r\n"
	}
	eolJ, _ := json.Marshal(eol)
	fmt.Fprintf(&b, `
require=function(mod){
  switch(mod){
  case 'path':return{
    sep:%s,
    join:function(){var p=Array.prototype.slice.call(arguments).join(%s);return p.replace(/\/{2,}/g,'/');},
    dirname:function(p){var i=p.lastIndexOf(%s);return i<0?'.':(i===0?%s:p.slice(0,i));},
    basename:function(p,x){var b=p.slice(p.lastIndexOf(%s)+1);return x&&b.endsWith(x)?b.slice(0,-x.length):b;},
    extname:function(p){var b=p.slice(p.lastIndexOf(%s)+1);var i=b.lastIndexOf('.');return i<1?'':b.slice(i);},
    resolve:function(){return Array.prototype.slice.call(arguments).join(%s);}
  };
  case 'os':return{EOL:%s,platform:function(){return process.platform;},homedir:function(){return %s;},tmpdir:function(){return %s;}};
  default:throw new Error("require: module '"+mod+"' not available");
  }
};
`, sepJ, sepJ, sepJ, sepJ, sepJ, sepJ, sepJ, eolJ, homeJ, tmpJ)

	b.WriteString("})();")
	return b.String()
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
	bodyStr := ""
	if b, ok := opts["body"].(string); ok {
		bodyStr = b
	}
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(bodyStr))
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
	var sb strings.Builder
	fmt.Fprintf(&sb, "HTTP %d\n", resp.StatusCode)
	for k, vs := range resp.Header {
		fmt.Fprintf(&sb, "%s: %s\n", k, strings.Join(vs, ", "))
	}
	sb.WriteString("\n")
	var buf [1 << 20]byte
	n, _ := resp.Body.Read(buf[:])
	sb.Write(buf[:n])
	return sb.String(), resp.StatusCode >= 400, nil
}

// pathAllowed reports whether path is under one of the allowed prefixes.
func pathAllowed(path string, allowed []string) bool {
	clean := filepath.Clean(path)
	for _, prefix := range allowed {
		p := filepath.Clean(prefix)
		if clean == p || strings.HasPrefix(clean, p+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}
