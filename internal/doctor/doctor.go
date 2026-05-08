// Package doctor provides an environment health checker for the pi agent.
package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/tiru-r/pi-agent-go/internal/config"
	_ "modernc.org/sqlite"
)

// Check is a single health check result.
type Check struct {
	Name   string
	Status string // "ok" | "warning" | "error"
	Detail string
}

// Report aggregates all check results.
type Report struct {
	Checks []Check
	Passed int
	Warned int
	Failed int
}

// Run executes all health checks and returns a Report.
func Run(ctx context.Context, cfg *config.Config) (*Report, error) {
	r := &Report{}

	checks := []func(context.Context, *config.Config) Check{
		checkConfigFile,
		checkAPIKeys,
		checkSessionDir,
		checkShellTools,
		checkGoVersion,
		checkDiskSpace,
		checkSQLite,
	}

	for _, fn := range checks {
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		default:
		}
		c := fn(ctx, cfg)
		r.Checks = append(r.Checks, c)
		switch c.Status {
		case "ok":
			r.Passed++
		case "warning":
			r.Warned++
		case "error":
			r.Failed++
		}
	}

	return r, nil
}

// String returns a formatted, human-readable report.
func (r *Report) String() string {
	var sb strings.Builder
	sb.WriteString("=== Pi Agent Health Check ===\n\n")

	maxName := 0
	for _, c := range r.Checks {
		if len(c.Name) > maxName {
			maxName = len(c.Name)
		}
	}

	for _, c := range r.Checks {
		icon := statusIcon(c.Status)
		padding := strings.Repeat(" ", maxName-len(c.Name)+1)
		sb.WriteString(fmt.Sprintf("%s %-*s  %s", icon, maxName, c.Name, padding))
		if c.Detail != "" {
			sb.WriteString("  " + c.Detail)
		}
		sb.WriteByte('\n')
	}

	sb.WriteString(fmt.Sprintf("\nResult: %d passed, %d warned, %d failed\n",
		r.Passed, r.Warned, r.Failed))
	return sb.String()
}

func statusIcon(status string) string {
	switch status {
	case "ok":
		return "[OK]"
	case "warning":
		return "[WARN]"
	case "error":
		return "[FAIL]"
	}
	return "[?]"
}

// ============================================================================
// Individual checks
// ============================================================================

func checkConfigFile(_ context.Context, cfg *config.Config) Check {
	c := Check{Name: "Config file"}
	if cfg == nil {
		c.Status = "error"
		c.Detail = "config is nil"
		return c
	}
	// If we got here, config.Load() already succeeded.
	c.Status = "ok"
	c.Detail = "settings.json loaded successfully"
	return c
}

func checkAPIKeys(_ context.Context, cfg *config.Config) Check {
	c := Check{Name: "API keys"}
	if cfg == nil {
		c.Status = "error"
		c.Detail = "config unavailable"
		return c
	}

	type kv struct{ name, val string }
	keys := []kv{
		{"anthropic", cfg.AnthropicAPIKey},
		{"openai", cfg.OpenAIAPIKey},
		{"gemini", cfg.GeminiAPIKey},
		{"cohere", cfg.CohereAPIKey},
	}

	var missing []string
	for _, k := range keys {
		if k.val == "" {
			missing = append(missing, k.name)
		}
	}

	switch {
	case len(missing) == len(keys):
		c.Status = "error"
		c.Detail = "no API keys configured"
	case len(missing) > 0:
		c.Status = "warning"
		c.Detail = "missing keys for: " + strings.Join(missing, ", ")
	default:
		c.Status = "ok"
		c.Detail = "all provider keys present"
	}
	return c
}

func checkSessionDir(_ context.Context, cfg *config.Config) Check {
	c := Check{Name: "Session directory"}
	dir := ""
	if cfg != nil {
		dir = cfg.SessionDir
	}
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".pi", "agent", "sessions")
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.Status = "error"
		c.Detail = fmt.Sprintf("cannot create %s: %v", dir, err)
		return c
	}

	// Test writability by creating a temp file.
	f, err := os.CreateTemp(dir, ".doctor-probe-*")
	if err != nil {
		c.Status = "error"
		c.Detail = fmt.Sprintf("directory not writable: %v", err)
		return c
	}
	_ = f.Close()
	_ = os.Remove(f.Name())

	c.Status = "ok"
	c.Detail = dir
	return c
}

func checkShellTools(_ context.Context, _ *config.Config) Check {
	c := Check{Name: "Shell tools"}
	required := []string{"bash", "grep", "find", "ls"}
	var missing []string
	for _, tool := range required {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		c.Status = "error"
		c.Detail = "missing: " + strings.Join(missing, ", ")
	} else {
		c.Status = "ok"
		c.Detail = "bash, grep, find, ls all found"
	}
	return c
}

func checkGoVersion(_ context.Context, _ *config.Config) Check {
	c := Check{Name: "Go version"}
	ver := runtime.Version() // e.g. "go1.23.4"
	c.Detail = ver

	// Parse major.minor from "go1.23.4"
	verStr := strings.TrimPrefix(ver, "go")
	parts := strings.Split(verStr, ".")
	if len(parts) < 2 {
		c.Status = "warning"
		c.Detail = fmt.Sprintf("cannot parse version %q", ver)
		return c
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		c.Status = "warning"
		c.Detail = fmt.Sprintf("cannot parse version %q", ver)
		return c
	}

	if major > 1 || (major == 1 && minor >= 23) {
		c.Status = "ok"
	} else {
		c.Status = "warning"
		c.Detail = fmt.Sprintf("%s is below minimum 1.23", ver)
	}
	return c
}

func checkDiskSpace(_ context.Context, cfg *config.Config) Check {
	c := Check{Name: "Disk space"}
	dir := ""
	if cfg != nil {
		dir = cfg.SessionDir
	}
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".pi", "agent", "sessions")
	}
	_ = os.MkdirAll(dir, 0o755)

	availGB, err := diskFreeGB(dir)
	if err != nil {
		c.Status = "warning"
		c.Detail = fmt.Sprintf("cannot determine free disk space: %v", err)
		return c
	}

	if availGB < 1.0 {
		c.Status = "warning"
		c.Detail = fmt.Sprintf("%.1f GB free (below 1 GB threshold)", availGB)
	} else {
		c.Status = "ok"
		c.Detail = fmt.Sprintf("%.1f GB free", availGB)
	}
	return c
}

// diskFreeGB returns free gigabytes for the filesystem containing dir.
// It shells out to df(1) for cross-platform compatibility.
func diskFreeGB(dir string) (float64, error) {
	cmd := exec.Command("df", "-k", dir)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("df: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	// df output: header line + data line
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output")
	}
	// The "Available" column index varies; parse by fields.
	// Standard POSIX df: Filesystem 1K-blocks Used Available Use% Mounted-on
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, fmt.Errorf("cannot parse df output: %q", lines[1])
	}
	// field[3] is Available (in 1K blocks)
	kblocks, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse available blocks: %w", err)
	}
	availBytes := kblocks * 1024
	return float64(availBytes) / (1 << 30), nil
}

func checkSQLite(_ context.Context, cfg *config.Config) Check {
	c := Check{Name: "SQLite"}
	dir := ""
	if cfg != nil {
		dir = cfg.SessionDir
	}
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".pi", "agent", "sessions")
	}
	_ = os.MkdirAll(dir, 0o755)

	testDB := filepath.Join(dir, ".doctor-sqlite-probe.db")
	defer os.Remove(testDB)

	db, err := sql.Open("sqlite", testDB)
	if err != nil {
		c.Status = "error"
		c.Detail = fmt.Sprintf("cannot open SQLite: %v", err)
		return c
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _probe (id INTEGER PRIMARY KEY)`); err != nil {
		c.Status = "error"
		c.Detail = fmt.Sprintf("cannot execute SQL: %v", err)
		return c
	}

	c.Status = "ok"
	c.Detail = "SQLite operational"
	return c
}
