package acp

import (
	"os"
	"path/filepath"
	"strings"
)

// projectSnapshot builds a compact, human-readable context block for the given
// working directory. It is injected into the system prompt once at session start
// so the model immediately knows the project layout without a discovery turn.
//
// All I/O is local filesystem — no network, no subprocesses.
func projectSnapshot(cwd string) string {
	if cwd == "" {
		return ""
	}

	var sb strings.Builder

	sb.WriteString("Working directory: ")
	sb.WriteString(cwd)
	sb.WriteByte('\n')

	if langs := detectLangs(cwd); len(langs) > 0 {
		sb.WriteString("Language: ")
		sb.WriteString(strings.Join(langs, ", "))
		sb.WriteByte('\n')
	}

	if branch := gitBranch(cwd); branch != "" {
		sb.WriteString("Branch: ")
		sb.WriteString(branch)
		sb.WriteByte('\n')
	}

	if listing := topLevelListing(cwd); listing != "" {
		sb.WriteString("Top-level: ")
		sb.WriteString(listing)
		sb.WriteByte('\n')
	}

	if instr := projectInstructions(cwd); instr != "" {
		sb.WriteString("\nProject instructions:\n")
		sb.WriteString(instr)
		sb.WriteByte('\n')
	}

	return sb.String()
}

// langManifests maps well-known manifest filenames to display language names.
// Order determines priority — first match per language wins.
var langManifests = []struct {
	file string
	lang string
}{
	{"go.mod", "Go"},
	{"Cargo.toml", "Rust"},
	{"pyproject.toml", "Python"},
	{"setup.py", "Python"},
	{"requirements.txt", "Python"},
	{"package.json", "JavaScript/TypeScript"},
	{"pom.xml", "Java"},
	{"build.gradle", "Java/Kotlin"},
	{"build.gradle.kts", "Kotlin"},
	{"CMakeLists.txt", "C/C++"},
	{"Makefile", "C/C++"},
	{"Gemfile", "Ruby"},
	{"mix.exs", "Elixir"},
	{"pubspec.yaml", "Dart/Flutter"},
	{"composer.json", "PHP"},
}

func detectLangs(cwd string) []string {
	seen := map[string]bool{}
	var langs []string
	for _, m := range langManifests {
		if _, err := os.Stat(filepath.Join(cwd, m.file)); err == nil {
			if !seen[m.lang] {
				seen[m.lang] = true
				langs = append(langs, m.lang)
			}
		}
	}
	return langs
}

// gitBranch reads .git/HEAD directly — no subprocess.
func gitBranch(cwd string) string {
	data, err := os.ReadFile(filepath.Join(cwd, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if branch, ok := strings.CutPrefix(s, "ref: refs/heads/"); ok {
		return branch
	}
	if len(s) >= 7 {
		return s[:7] // detached HEAD — show short SHA
	}
	return s
}

// topLevelListing returns a space-separated list of non-hidden top-level entries.
func topLevelListing(cwd string) string {
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	out := strings.Join(names, "  ")
	if len(out) > 400 {
		out = out[:400] + "…"
	}
	return out
}

// instructionsCandidates lists filenames checked for project-level instructions,
// in priority order. First match wins.
var instructionsCandidates = []string{
	".pi/instructions.md",
	"AGENTS.md",
	"agents.md",
}

const maxInstructionsBytes = 4000

func projectInstructions(cwd string) string {
	for _, name := range instructionsCandidates {
		data, err := os.ReadFile(filepath.Join(cwd, name))
		if err != nil || len(data) == 0 {
			continue
		}
		s := strings.TrimSpace(string(data))
		if len(s) > maxInstructionsBytes {
			s = s[:maxInstructionsBytes] + "\n… (truncated)"
		}
		return s
	}
	return ""
}
