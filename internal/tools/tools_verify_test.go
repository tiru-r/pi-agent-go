package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiru-r/pi-agent-go/internal/tools"
)

// repoRoot walks up from the package directory until it finds go.mod, which
// gives a stable, machine-independent path regardless of where the repo is
// cloned. It is more robust than searching for a specific path segment like
// "/internal" that may appear elsewhere in the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repoRoot: could not find go.mod walking up from %q", wd)
		}
		dir = parent
	}
}

// TestPrintTree_BasicExecution verifies print_tree exists and runs.
func TestPrintTree_BasicExecution(t *testing.T) {
	tool, ok := tools.Get(tools.ToolNamePrintTree)
	if !ok {
		t.Fatal("print_tree tool not registered")
	}

	ctx := tools.WithCWD(context.Background(), repoRoot(t))
	params, _ := json.Marshal(map[string]any{"path": ".", "max_depth": 2})
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("print_tree returned error: %s", result.Content[0].Text)
	}
	out := result.Content[0].Text
	if !strings.Contains(out, "internal/") {
		t.Errorf("expected 'internal/' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "director") {
		t.Errorf("expected summary line with 'director', got:\n%s", out)
	}
}

// TestPrintTree_DefaultsToCWD verifies empty path defaults to working dir.
func TestPrintTree_DefaultsToCWD(t *testing.T) {
	tool, ok := tools.Get(tools.ToolNamePrintTree)
	if !ok {
		t.Fatal("print_tree tool not registered")
	}
	ctx := tools.WithCWD(context.Background(), t.TempDir())
	params := json.RawMessage(`{}`)
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].Text)
	}
}

// TestLS_DefaultsToCWD verifies ls with empty path uses cwd.
func TestLS_DefaultsToCWD(t *testing.T) {
	tool, ok := tools.Get(tools.ToolNameLS)
	if !ok {
		t.Fatal("ls tool not registered")
	}
	ctx := tools.WithCWD(context.Background(), repoRoot(t))
	params := json.RawMessage(`{}`)
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("ls returned error: %s", result.Content[0].Text)
	}
	out := result.Content[0].Text
	if !strings.Contains(out, "internal") {
		t.Errorf("expected 'internal' in ls output, got:\n%s", out)
	}
}

// TestLS_DotPath verifies ls "." resolves to cwd.
func TestLS_DotPath(t *testing.T) {
	tool, ok := tools.Get(tools.ToolNameLS)
	if !ok {
		t.Fatal("ls tool not registered")
	}
	ctx := tools.WithCWD(context.Background(), t.TempDir())
	params, _ := json.Marshal(map[string]string{"path": "."})
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("ls '.' returned unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("ls '.' returned error result: %s", result.Content[0].Text)
	}
}

// TestResolvePath_RelativeViaRead verifies read resolves a relative path.
func TestResolvePath_RelativeViaRead(t *testing.T) {
	tool, ok := tools.Get(tools.ToolNameRead)
	if !ok {
		t.Fatal("read tool not registered")
	}
	ctx := tools.WithCWD(context.Background(), repoRoot(t))
	params, _ := json.Marshal(map[string]any{"path": "go.mod", "limit": 3})
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("read relative path failed: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, "pi-agent-go") {
		t.Errorf("unexpected go.mod content: %s", result.Content[0].Text)
	}
}
