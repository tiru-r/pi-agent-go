// Package tools provides the 8 built-in tool implementations for the pi agent.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/model"
)

// Tool is the interface every built-in (and extension) tool must satisfy.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Execute(ctx context.Context, params json.RawMessage) (*Result, error)
}

// Result is the structured output returned by a tool execution.
type Result struct {
	Content []model.ContentBlock
	IsError bool
}

// textResult is a convenience constructor for a plain-text result.
func textResult(text string) *Result {
	return &Result{
		Content: []model.ContentBlock{{Type: model.ContentTypeText, Text: text}},
	}
}

// errorResult is a convenience constructor for an error result.
func errorResult(msg string) *Result {
	return &Result{
		Content: []model.ContentBlock{{Type: model.ContentTypeText, Text: msg}},
		IsError: true,
	}
}

// ============================================================================
// Registry
// ============================================================================

// BuiltinTools is the ordered list of all built-in tools.
var BuiltinTools []Tool

// toolMap is the name→Tool lookup map.
var toolMap map[string]Tool

var registerOnce sync.Once

func init() {
	registerOnce.Do(func() {
		BuiltinTools = []Tool{
			&readTool{},
			&writeTool{},
			&editTool{},
			&bashTool{},
			&grepTool{},
			&findTool{},
			&lsTool{},
			&hashlineEditTool{},
		}
		toolMap = make(map[string]Tool, len(BuiltinTools))
		for _, t := range BuiltinTools {
			toolMap[t.Name()] = t
		}
	})
}

// Get returns the tool with the given name, or false if not found.
func Get(name string) (Tool, bool) {
	t, ok := toolMap[name]
	return t, ok
}

// Register adds a dynamically-loaded tool (e.g. from an extension) to the registry.
// If a tool with the same name already exists it is replaced.
func Register(t Tool) {
	toolMap[t.Name()] = t
	for i, existing := range BuiltinTools {
		if existing.Name() == t.Name() {
			BuiltinTools[i] = t
			return
		}
	}
	BuiltinTools = append(BuiltinTools, t)
}

// ============================================================================
// Working directory context
// ============================================================================

type cwdCtxKey struct{}

// WithCWD returns a copy of ctx carrying the working directory for tools.
func WithCWD(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, cwdCtxKey{}, dir)
}

// CWD returns the working directory stored in ctx, or "" if not set.
func CWD(ctx context.Context) string {
	if dir, ok := ctx.Value(cwdCtxKey{}).(string); ok {
		return dir
	}
	return ""
}

// ToDefinitions converts all built-in tools to model.ToolDefinition slice.
func ToDefinitions() []model.ToolDefinition {
	defs := make([]model.ToolDefinition, 0, len(BuiltinTools))
	for _, t := range BuiltinTools {
		defs = append(defs, model.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.Schema(),
		})
	}
	return defs
}

// ============================================================================
// 1. read
// ============================================================================

type readTool struct{}

func (r *readTool) Name() string        { return "read" }
func (r *readTool) Description() string { return "Read file contents with line numbers." }
func (r *readTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":   {"type": "string", "description": "Absolute path to the file to read."},
    "offset": {"type": "integer", "description": "Line number to start reading from (1-based, optional)."},
    "limit":  {"type": "integer", "description": "Maximum number of lines to read (default 2000)."}
  },
  "required": ["path"]
}`)
}

var imageExtensions = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
}

func (r *readTool) Execute(_ context.Context, params json.RawMessage) (*Result, error) {
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResult("invalid parameters: " + err.Error()), nil
	}
	if p.Path == "" {
		return errorResult("path is required"), nil
	}
	if p.Limit == 0 {
		p.Limit = 2000
	}

	ext := strings.ToLower(filepath.Ext(p.Path))
	if mediaType, ok := imageExtensions[ext]; ok {
		data, err := os.ReadFile(p.Path)
		if err != nil {
			return errorResult("cannot read image: " + err.Error()), nil
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		return &Result{
			Content: []model.ContentBlock{{
				Type: model.ContentTypeImage,
				Source: &model.ImageSource{
					Type:      "base64",
					MediaType: mediaType,
					Data:      encoded,
				},
			}},
		}, nil
	}

	data, err := os.ReadFile(p.Path)
	if err != nil {
		return errorResult("cannot read file: " + err.Error()), nil
	}

	lines := strings.Split(string(data), "\n")
	// Remove trailing empty line from Split if file ends with newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	start := 0
	if p.Offset > 0 {
		start = p.Offset - 1
	}
	if start >= len(lines) {
		start = len(lines)
	}
	end := min(start+p.Limit, len(lines))

	var sb strings.Builder
	for i, line := range lines[start:end] {
		sb.WriteString(strconv.Itoa(start+i+1))
		sb.WriteByte('\t')
		sb.WriteString(line)
		sb.WriteByte('\n')
	}

	if end < len(lines) {
		fmt.Fprintf(&sb, "... (truncated, %d lines total, showing lines %d-%d)\n",
			len(lines), start+1, end)
	}

	result := sb.String()
	if len(result) > bashMaxBytes {
		result = result[:bashMaxBytes] + fmt.Sprintf("\n... (truncated at %d bytes)", bashMaxBytes)
	}
	return textResult(result), nil
}

// ============================================================================
// 2. write
// ============================================================================

type writeTool struct{}

func (w *writeTool) Name() string        { return "write" }
func (w *writeTool) Description() string { return "Write or create a file with the given content." }
func (w *writeTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":    {"type": "string", "description": "Absolute path to the file to write."},
    "content": {"type": "string", "description": "Content to write to the file."}
  },
  "required": ["path", "content"]
}`)
}

func (w *writeTool) Execute(_ context.Context, params json.RawMessage) (*Result, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResult("invalid parameters: " + err.Error()), nil
	}
	if p.Path == "" {
		return errorResult("path is required"), nil
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
		return errorResult("cannot create parent directories: " + err.Error()), nil
	}
	perm := fs.FileMode(0o644)
	if info, err := os.Stat(p.Path); err == nil {
		perm = info.Mode().Perm()
	}
	if err := os.WriteFile(p.Path, []byte(p.Content), perm); err != nil {
		return errorResult("cannot write file: " + err.Error()), nil
	}
	return textResult(fmt.Sprintf("Successfully wrote %d bytes to %s", len(p.Content), p.Path)), nil
}

// ============================================================================
// 3. edit
// ============================================================================

type editTool struct{}

func (e *editTool) Name() string { return "edit" }
func (e *editTool) Description() string {
	return "Replace a string in a file. Fails if old_string not found or if it appears more than once (when replace_all is false)."
}
func (e *editTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":        {"type": "string",  "description": "Absolute path to the file to edit."},
    "old_string":  {"type": "string",  "description": "The exact string to find and replace."},
    "new_string":  {"type": "string",  "description": "The string to replace old_string with."},
    "replace_all": {"type": "boolean", "description": "If true, replace all occurrences (default false)."}
  },
  "required": ["path", "old_string", "new_string"]
}`)
}

func (e *editTool) Execute(_ context.Context, params json.RawMessage) (*Result, error) {
	var p struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResult("invalid parameters: " + err.Error()), nil
	}
	if p.Path == "" {
		return errorResult("path is required"), nil
	}
	if p.OldString == "" {
		return errorResult("old_string cannot be empty"), nil
	}

	data, err := os.ReadFile(p.Path)
	if err != nil {
		return errorResult("cannot read file: " + err.Error()), nil
	}

	perm := fs.FileMode(0o644)
	if info, statErr := os.Stat(p.Path); statErr == nil {
		perm = info.Mode().Perm()
	}

	content := string(data)
	count := strings.Count(content, p.OldString)
	if count == 0 {
		return errorResult(fmt.Sprintf("old_string not found in %s", p.Path)), nil
	}
	if !p.ReplaceAll && count > 1 {
		return errorResult(fmt.Sprintf(
			"old_string appears %d times in %s; use replace_all=true or provide more context to make it unique",
			count, p.Path)), nil
	}

	var updated string
	if p.ReplaceAll {
		updated = strings.ReplaceAll(content, p.OldString, p.NewString)
	} else {
		updated = strings.Replace(content, p.OldString, p.NewString, 1)
	}

	if err := os.WriteFile(p.Path, []byte(updated), perm); err != nil {
		return errorResult("cannot write file: " + err.Error()), nil
	}
	return textResult(fmt.Sprintf("Successfully edited %s", p.Path)), nil
}

// ============================================================================
// 4. bash
// ============================================================================

const defaultBashTimeoutMS = 120_000
const bashMaxLines = 2000    // head+tail window
const bashMaxBytes = 1 << 20 // 1 MB hard cap after line truncation
const grepMaxLineLen = 500   // per-line char cap to handle minified files

type bashTool struct{}

func (b *bashTool) Name() string { return "bash" }
func (b *bashTool) Description() string {
	return "Execute a shell command and return its output."
}
func (b *bashTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string",  "description": "The shell command to execute."},
    "timeout": {"type": "integer", "description": "Timeout in milliseconds (default 120000)."}
  },
  "required": ["command"]
}`)
}

// truncateHeadTail applies the Rust-style head+tail truncation:
//  1. If lines > bashMaxLines: keep first half + last half, insert omission marker.
//  2. If bytes > bashMaxBytes after step 1: hard-cut at the byte limit.
func truncateHeadTail(s string) string {
	if len(s) == 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) > bashMaxLines {
		half := bashMaxLines / 2
		omitted := len(lines) - bashMaxLines
		head := lines[:half]
		tail := lines[len(lines)-half:]
		s = strings.Join(head, "\n") +
			fmt.Sprintf("\n... (%d lines omitted) ...\n", omitted) +
			strings.Join(tail, "\n")
	}
	if len(s) > bashMaxBytes {
		s = s[:bashMaxBytes] + fmt.Sprintf("\n... (truncated at %d bytes)", bashMaxBytes)
	}
	return s
}

// killProcessGroup sends SIGTERM to the process group, waits up to 5 s for a
// clean exit, then sends SIGKILL if the process is still alive.  This matches
// the Rust timeout escalation path and prevents orphaned child processes.
func killProcessGroup(cmd *exec.Cmd, done <-chan error) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		// Fallback: kill just the shell process.
		_ = cmd.Process.Kill()
		<-done
		return
	}
	// SIGTERM to the entire process group.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	grace := time.NewTimer(5 * time.Second)
	defer grace.Stop()
	select {
	case <-done:
		return
	case <-grace.C:
		// Grace period elapsed — escalate to SIGKILL.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
	}
}

func (b *bashTool) Execute(ctx context.Context, params json.RawMessage) (*Result, error) {
	var p struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResult("invalid parameters: " + err.Error()), nil
	}
	if p.Command == "" {
		return errorResult("command is required"), nil
	}
	timeoutMS := p.Timeout
	if timeoutMS <= 0 {
		timeoutMS = defaultBashTimeoutMS
	}

	// Child context combines the caller's cancellation with our own timeout.
	tctx, tcancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer tcancel()

	// Wrap the command with an EXIT trap so background jobs (e.g. "sleep 10 &")
	// are waited for before the shell exits, preventing orphaned processes.
	wrappedCmd := "trap 'wait' EXIT\n" + p.Command
	cmd := exec.Command("bash", "-c", wrappedCmd)
	if dir := CWD(ctx); dir != "" {
		cmd.Dir = dir
	}
	// Put the process in its own group so we can SIGTERM/SIGKILL the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		return errorResult("failed to start command: " + err.Error()), nil
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		output := truncateHeadTail(buf.String())
		if err != nil {
			exitCode := -1
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
			return errorResult(fmt.Sprintf("exit code %d\n%s", exitCode, output)), nil
		}
		return textResult(output), nil

	case <-tctx.Done():
		killProcessGroup(cmd, done)
		output := truncateHeadTail(buf.String())
		if ctx.Err() != nil {
			return errorResult(fmt.Sprintf("command cancelled\n%s", output)), nil
		}
		return errorResult(fmt.Sprintf("command timed out after %dms\n%s", timeoutMS, output)), nil
	}
}

// ============================================================================
// 5. grep
// ============================================================================

type grepTool struct{}

func (g *grepTool) Name() string { return "grep" }
func (g *grepTool) Description() string {
	return "Search file contents using regular expressions."
}
func (g *grepTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern":        {"type": "string",  "description": "Regular expression pattern to search for."},
    "path":           {"type": "string",  "description": "File or directory to search."},
    "context":        {"type": "integer", "description": "Lines of context around each match (default 0)."},
    "case_sensitive": {"type": "boolean", "description": "Whether the search is case-sensitive (default true)."},
    "recursive":      {"type": "boolean", "description": "Search recursively in directory (default true)."}
  },
  "required": ["pattern", "path"]
}`)
}

func (g *grepTool) Execute(ctx context.Context, params json.RawMessage) (*Result, error) {
	var p struct {
		Pattern       string `json:"pattern"`
		Path          string `json:"path"`
		Context       int    `json:"context"`
		CaseSensitive *bool  `json:"case_sensitive"`
		Recursive     *bool  `json:"recursive"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResult("invalid parameters: " + err.Error()), nil
	}
	if p.Pattern == "" {
		return errorResult("pattern is required"), nil
	}
	if p.Path == "" {
		return errorResult("path is required"), nil
	}

	caseSensitive := true
	if p.CaseSensitive != nil {
		caseSensitive = *p.CaseSensitive
	}
	recursive := true
	if p.Recursive != nil {
		recursive = *p.Recursive
	}

	pat := p.Pattern
	if !caseSensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return errorResult("invalid pattern: " + err.Error()), nil
	}

	var results []string
	matchCount := 0
	const maxMatches = 100

	searchFile := func(filePath string) {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return
		}
		if bytes.IndexByte(data, 0) != -1 {
			return // skip binary files
		}
		lines := strings.Split(string(data), "\n")
		lastPrintedEnd := -1 // tracks the next unprinted line to avoid overlap
		for i, line := range lines {
			if matchCount >= maxMatches {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			if re.MatchString(line) {
				matchCount++
				start := max(i-p.Context, 0)
				end := min(i+p.Context+1, len(lines))
				// Emit separator only when there is an actual gap between match
				// context windows (avoids duplicated lines and spurious "--").
				if lastPrintedEnd >= 0 && start > lastPrintedEnd {
					results = append(results, "--")
				}
				// Only print lines not yet covered by the previous match's context.
				printStart := max(start, lastPrintedEnd)
				for j := printStart; j < end; j++ {
					prefix := "  "
					if j == i {
						prefix = "> "
					}
					content := lines[j]
					if len(content) > grepMaxLineLen {
						content = content[:grepMaxLineLen] + "…"
					}
					results = append(results,
						fmt.Sprintf("%s%s:%d:%s", prefix, filePath, j+1, content))
				}
				lastPrintedEnd = end
			}
		}
	}

	info, err := os.Stat(p.Path)
	if err != nil {
		return errorResult("cannot stat path: " + err.Error()), nil
	}

	if info.IsDir() && recursive {
		_ = filepath.WalkDir(p.Path, func(path string, d fs.DirEntry, err error) error {
			if err != nil || matchCount >= maxMatches {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".git" || name == "node_modules" || name == "target" {
					return filepath.SkipDir
				}
				return nil
			}
			searchFile(path)
			return nil
		})
	} else if !info.IsDir() {
		searchFile(p.Path)
	}

	if len(results) == 0 {
		return textResult("No matches found."), nil
	}

	output := strings.Join(results, "\n")
	if matchCount >= maxMatches {
		output += "\n... (result limit reached)"
	}
	return textResult(truncateHeadTail(output)), nil
}

// ============================================================================
// 6. find
// ============================================================================

type findTool struct{}

func (f *findTool) Name() string        { return "find" }
func (f *findTool) Description() string { return "Find files or directories by glob pattern." }
func (f *findTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":      {"type": "string",  "description": "Root directory to search in."},
    "pattern":   {"type": "string",  "description": "Glob pattern to match file names against."},
    "type":      {"type": "string",  "description": "Entry type filter: 'f' for file, 'd' for directory, 'l' for symlink."},
    "max_depth": {"type": "integer", "description": "Maximum directory depth to descend (0 = unlimited)."}
  },
  "required": ["path", "pattern"]
}`)
}

var excludedFindDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"target":       true,
}

type findEntry struct {
	path    string
	modTime time.Time
}

func (f *findTool) Execute(ctx context.Context, params json.RawMessage) (*Result, error) {
	var p struct {
		Path     string `json:"path"`
		Pattern  string `json:"pattern"`
		Type     string `json:"type"`
		MaxDepth int    `json:"max_depth"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResult("invalid parameters: " + err.Error()), nil
	}
	if p.Path == "" {
		return errorResult("path is required"), nil
	}
	if p.Pattern == "" {
		return errorResult("pattern is required"), nil
	}

	rootInfo, err := os.Stat(p.Path)
	if err != nil {
		return errorResult("cannot stat path: " + err.Error()), nil
	}
	if !rootInfo.IsDir() {
		return errorResult("path must be a directory"), nil
	}

	const maxResults = 1000
	var matches []findEntry
	rootDepth := strings.Count(filepath.Clean(p.Path), string(os.PathSeparator))

	_ = filepath.WalkDir(p.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil || len(matches) >= maxResults {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if d.IsDir() && excludedFindDirs[d.Name()] {
			return filepath.SkipDir
		}

		if p.MaxDepth > 0 {
			depth := strings.Count(filepath.Clean(path), string(os.PathSeparator)) - rootDepth
			if d.IsDir() && depth >= p.MaxDepth {
				return filepath.SkipDir
			}
		}

		if path == p.Path {
			return nil
		}

		// Type filter
		switch p.Type {
		case "f":
			if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
		case "d":
			if !d.IsDir() {
				return nil
			}
		case "l":
			if d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
		}

		matched, err := filepath.Match(p.Pattern, d.Name())
		if err != nil {
			return nil
		}
		if matched {
			entry := findEntry{path: path}
			if info, infoErr := d.Info(); infoErr == nil {
				entry.modTime = info.ModTime()
			}
			matches = append(matches, entry)
		}
		return nil
	})

	if len(matches) == 0 {
		return textResult("No matches found."), nil
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].modTime.After(matches[j].modTime)
	})

	paths := make([]string, len(matches))
	for i, m := range matches {
		paths[i] = m.path
	}
	output := strings.Join(paths, "\n")
	if len(matches) >= maxResults {
		output += "\n... (result limit reached)"
	}
	return textResult(output), nil
}

// ============================================================================
// 7. ls
// ============================================================================

type lsTool struct{}

func (l *lsTool) Name() string        { return "ls" }
func (l *lsTool) Description() string { return "List directory contents with metadata." }
func (l *lsTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute path to the directory to list."}
  },
  "required": ["path"]
}`)
}

func (l *lsTool) Execute(_ context.Context, params json.RawMessage) (*Result, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResult("invalid parameters: " + err.Error()), nil
	}
	if p.Path == "" {
		return errorResult("path is required"), nil
	}

	entries, err := os.ReadDir(p.Path)
	if err != nil {
		return errorResult("cannot read directory: " + err.Error()), nil
	}

	const maxEntries = 500
	var sb strings.Builder
	fmt.Fprintf(&sb, "Contents of %s:\n\n", p.Path)
	fmt.Fprintf(&sb, "%-12s %-10s %s\n", "TYPE", "SIZE", "NAME")
	sb.WriteString(strings.Repeat("-", 50) + "\n")

	for i, entry := range entries {
		if i >= maxEntries {
			fmt.Fprintf(&sb, "... (truncated, showing first %d entries)\n", maxEntries)
			break
		}

		var entryType, sizeStr string
		info, err := entry.Info()
		if err != nil {
			entryType = "?"
			sizeStr = "?"
		} else {
			mode := info.Mode()
			switch {
			case mode&fs.ModeSymlink != 0:
				entryType = "symlink"
			case mode.IsDir():
				entryType = "dir"
			default:
				entryType = "file"
				sizeStr = formatSize(info.Size())
			}
		}

		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		fmt.Fprintf(&sb, "%-12s %-10s %s\n", entryType, sizeStr, name)
	}

	return textResult(sb.String()), nil
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// ============================================================================
// 8. hashline_edit
// ============================================================================

type hashlineEditTool struct{}

func (h *hashlineEditTool) Name() string { return "hashline_edit" }
func (h *hashlineEditTool) Description() string {
	return "Edit file lines identified by LINE#HASH tags. Each tag is '{line_number}#{first6_sha256}'. More precise than string matching."
}
func (h *hashlineEditTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute path to the file to edit."},
    "edits": {
      "type": "array",
      "description": "List of edits to apply.",
      "items": {
        "type": "object",
        "properties": {
          "line_hash":   {"type": "string", "description": "Tag in format 'LINE#HASH' e.g. '42#abc123'."},
          "new_content": {"type": "string", "description": "Replacement content for the matched line (without trailing newline)."}
        },
        "required": ["line_hash", "new_content"]
      }
    }
  },
  "required": ["path", "edits"]
}`)
}

type hashlineEdit struct {
	LineHash   string `json:"line_hash"`
	NewContent string `json:"new_content"`
}

func lineHash(line string) string {
	sum := sha256.Sum256([]byte(line))
	return fmt.Sprintf("%x", sum[:3])[:6]
}

func parseLineHash(tag string) (lineNum int, hash string, err error) {
	parts := strings.SplitN(tag, "#", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("invalid line_hash format %q: expected LINE#HASH", tag)
	}
	lineNum, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", fmt.Errorf("invalid line number in %q: %w", tag, err)
	}
	return lineNum, parts[1], nil
}

func (h *hashlineEditTool) Execute(_ context.Context, params json.RawMessage) (*Result, error) {
	var p struct {
		Path  string         `json:"path"`
		Edits []hashlineEdit `json:"edits"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResult("invalid parameters: " + err.Error()), nil
	}
	if p.Path == "" {
		return errorResult("path is required"), nil
	}
	if len(p.Edits) == 0 {
		return errorResult("edits list is empty"), nil
	}

	data, err := os.ReadFile(p.Path)
	if err != nil {
		return errorResult("cannot read file: " + err.Error()), nil
	}

	lines := strings.Split(string(data), "\n")
	// preserve trailing-newline semantics
	hasTrailingNewline := len(data) > 0 && data[len(data)-1] == '\n'
	if hasTrailingNewline && len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	applied := 0
	for _, edit := range p.Edits {
		lineNum, expectedHash, err := parseLineHash(edit.LineHash)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		idx := lineNum - 1
		if idx < 0 || idx >= len(lines) {
			return errorResult(fmt.Sprintf("line %d is out of range (file has %d lines)", lineNum, len(lines))), nil
		}
		actualHash := lineHash(lines[idx])
		if actualHash != expectedHash {
			return errorResult(fmt.Sprintf(
				"hash mismatch on line %d: expected %s, got %s (line content may have changed)",
				lineNum, expectedHash, actualHash)), nil
		}
		lines[idx] = edit.NewContent
		applied++
	}

	joined := strings.Join(lines, "\n")
	if hasTrailingNewline {
		joined += "\n"
	}
	if err := os.WriteFile(p.Path, []byte(joined), 0o644); err != nil {
		return errorResult("cannot write file: " + err.Error()), nil
	}

	return textResult(fmt.Sprintf("Applied %d edit(s) to %s", applied, p.Path)), nil
}

