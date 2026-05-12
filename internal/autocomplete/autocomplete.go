// Package autocomplete provides completion suggestions for interactive editor input.
//
// It is rendering-agnostic: Complete takes editor text + cursor position and
// returns structured suggestions plus the byte range to replace on selection.
//
// Current suggestion sources:
//   - Built-in slash commands  (/help, /model, …)
//   - Prompt templates         (/<name>) from a TemplateLoader
//   - Skills                   (/skill:<name>) when an extension manager is provided
//   - File references          (@path) with a cached project file index
//   - Path completions         when the cursor is inside a path-like token
package autocomplete

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/extensions"
)

// ── Public types ──────────────────────────────────────────────────────────────

// Kind identifies the source of a suggestion.
type Kind int

const (
	KindSlashCommand Kind = iota
	KindTemplate
	KindSkill
	KindFileRef
	KindPath
)

func (k Kind) String() string {
	switch k {
	case KindSlashCommand:
		return "slash_command"
	case KindTemplate:
		return "template"
	case KindSkill:
		return "skill"
	case KindFileRef:
		return "file_ref"
	case KindPath:
		return "path"
	default:
		return "unknown"
	}
}

// Suggestion is a single completion item.
type Suggestion struct {
	Label      string // display text shown in the picker
	Detail     string // secondary description / hint
	Kind       Kind
	InsertText string // text that replaces Range on selection
}

// Range is the half-open byte range [Start, End) in the input to replace.
type Range struct {
	Start, End int
}

// TemplateLoader provides prompt templates by name.
// Implement this interface to plug in template sources.
type TemplateLoader interface {
	// Names returns all available template names (without the leading slash).
	Names() []string
	// Description returns a short description for the named template.
	Description(name string) string
}

// ── Built-in slash commands ───────────────────────────────────────────────────

type slashEntry struct {
	name   string
	detail string
}

// builtinSlashCommands is the authoritative list of interactive slash commands.
// Extend this slice to register new built-ins.
var builtinSlashCommands = []slashEntry{
	{"/help", "Show available commands"},
	{"/model", "Switch the active model"},
	{"/mode", "Switch agent mode (act / plan / interactive / …)"},
	{"/clear", "Clear conversation history"},
	{"/session", "Session management"},
	{"/config", "Show or edit configuration"},
	{"/think", "Toggle extended thinking"},
	{"/cancel", "Cancel the current request"},
}

// ── Provider ──────────────────────────────────────────────────────────────────

// Provider generates completion suggestions.
// All fields are optional; a nil field simply skips that suggestion source.
type Provider struct {
	extMgr    *extensions.Manager
	templates TemplateLoader

	indexMu sync.Mutex
	indices map[string]*fileIndex // keyed by canonical cwd
}

// New creates a Provider. extMgr and templates may be nil.
func New(extMgr *extensions.Manager, templates TemplateLoader) *Provider {
	return &Provider{
		extMgr:    extMgr,
		templates: templates,
		indices:   make(map[string]*fileIndex),
	}
}

// Complete returns suggestions for editor text at the given cursor byte offset,
// and the Range that should be replaced when a suggestion is applied.
// cwd is the project root used to resolve relative @file paths; may be empty.
func (p *Provider) Complete(text string, cursor int, cwd string) ([]Suggestion, Range, error) {
	if cursor < 0 || cursor > len(text) {
		cursor = len(text)
	}
	prefix := text[:cursor]

	if tok, r, ok := slashToken(prefix); ok {
		return p.completeSlash(tok), r, nil
	}
	if tok, r, ok := atToken(prefix); ok {
		return p.completeAt(tok, cwd), r, nil
	}
	if tok, r, ok := pathToken(prefix); ok {
		return p.completePath(tok, cwd), r, nil
	}
	return nil, Range{}, nil
}

// ── Slash completions ─────────────────────────────────────────────────────────

func (p *Provider) completeSlash(tok string) []Suggestion {
	var out []Suggestion

	for _, cmd := range builtinSlashCommands {
		if strings.HasPrefix(cmd.name, tok) {
			out = append(out, Suggestion{
				Label:      cmd.name,
				Detail:     cmd.detail,
				Kind:       KindSlashCommand,
				InsertText: cmd.name,
			})
		}
	}

	if p.templates != nil {
		for _, name := range p.templates.Names() {
			full := "/" + name
			if strings.HasPrefix(full, tok) {
				out = append(out, Suggestion{
					Label:      full,
					Detail:     p.templates.Description(name),
					Kind:       KindTemplate,
					InsertText: full,
				})
			}
		}
	}

	if p.extMgr != nil {
		for _, ext := range p.extMgr.All() {
			info := ext.Info()
			full := "/skill:" + info.Name
			if strings.HasPrefix(full, tok) {
				out = append(out, Suggestion{
					Label:      full,
					Detail:     info.Description,
					Kind:       KindSkill,
					InsertText: full,
				})
			}
		}
	}

	return out
}

// ── @file completions ─────────────────────────────────────────────────────────

const maxSuggestions = 50

func (p *Provider) completeAt(tok string, cwd string) []Suggestion {
	query := strings.TrimPrefix(tok, "@")
	paths := p.getIndex(cwd)

	var out []Suggestion
	for _, path := range paths {
		if strings.HasPrefix(path, query) || (query != "" && fuzzyMatch(query, path)) {
			out = append(out, Suggestion{
				Label:      "@" + path,
				Detail:     path,
				Kind:       KindFileRef,
				InsertText: "@" + path,
			})
			if len(out) >= maxSuggestions {
				break
			}
		}
	}
	return out
}

// ── Path completions ──────────────────────────────────────────────────────────

func (p *Provider) completePath(tok string, cwd string) []Suggestion {
	dir, partial := filepath.Split(tok)
	searchDir := dir
	if searchDir == "" {
		searchDir = "."
	}
	if cwd != "" && !filepath.IsAbs(searchDir) {
		searchDir = filepath.Join(cwd, searchDir)
	}

	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil
	}

	var out []Suggestion
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), partial) {
			continue
		}
		insert := dir + e.Name()
		detail := "file"
		if e.IsDir() {
			insert += "/"
			detail = "directory"
		}
		out = append(out, Suggestion{
			Label:      insert,
			Detail:     detail,
			Kind:       KindPath,
			InsertText: insert,
		})
		if len(out) >= maxSuggestions {
			break
		}
	}
	return out
}

// ── File index ────────────────────────────────────────────────────────────────

const (
	indexTTL      = 30 * time.Second
	maxIndexFiles = 5000
)

type fileIndex struct {
	mu      sync.RWMutex
	root    string
	paths   []string
	builtAt time.Time
}

func (p *Provider) getIndex(cwd string) []string {
	if cwd == "" {
		return nil
	}
	p.indexMu.Lock()
	idx, ok := p.indices[cwd]
	if !ok {
		idx = &fileIndex{root: cwd}
		p.indices[cwd] = idx
	}
	p.indexMu.Unlock()

	idx.mu.RLock()
	stale := time.Since(idx.builtAt) > indexTTL
	paths := idx.paths
	idx.mu.RUnlock()

	if stale {
		fresh := buildIndex(cwd)
		idx.mu.Lock()
		idx.paths = fresh
		idx.builtAt = time.Now()
		idx.mu.Unlock()
		return fresh
	}
	return paths
}

// buildIndex walks root and returns relative paths, skipping hidden dirs and
// common noise directories (node_modules, vendor, __pycache__).
func buildIndex(root string) []string {
	var paths []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name != "." && (name[0] == '.' || name == "node_modules" || name == "vendor" || name == "__pycache__") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		paths = append(paths, rel)
		if len(paths) >= maxIndexFiles {
			return filepath.SkipAll
		}
		return nil
	})
	return paths
}

// ── Token detectors ───────────────────────────────────────────────────────────

// slashToken reports whether the current word (up to cursor) starts with '/'.
func slashToken(prefix string) (string, Range, bool) {
	start := wordStart(prefix)
	tok := prefix[start:]
	if !strings.HasPrefix(tok, "/") {
		return "", Range{}, false
	}
	return tok, Range{Start: start, End: len(prefix)}, true
}

// atToken reports whether the current word starts with '@'.
func atToken(prefix string) (string, Range, bool) {
	start := wordStart(prefix)
	tok := prefix[start:]
	if !strings.HasPrefix(tok, "@") {
		return "", Range{}, false
	}
	return tok, Range{Start: start, End: len(prefix)}, true
}

// pathToken reports whether the current word looks like a filesystem path
// (starts with ./, ../, /, ~/, or contains an internal '/').
func pathToken(prefix string) (string, Range, bool) {
	start := wordStart(prefix)
	tok := prefix[start:]
	if tok == "" {
		return "", Range{}, false
	}
	if strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") ||
		strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~/") {
		return tok, Range{Start: start, End: len(prefix)}, true
	}
	if strings.ContainsRune(tok, '/') {
		return tok, Range{Start: start, End: len(prefix)}, true
	}
	return "", Range{}, false
}

// wordStart returns the byte index where the current word begins in prefix.
func wordStart(prefix string) int {
	i := len(prefix)
	for i > 0 && !isWordSep(prefix[i-1]) {
		i--
	}
	return i
}

func isWordSep(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// fuzzyMatch reports whether all runes of query appear in order within target.
func fuzzyMatch(query, target string) bool {
	qi, qr := 0, []rune(strings.ToLower(query))
	for _, r := range strings.ToLower(target) {
		if r == qr[qi] {
			qi++
			if qi == len(qr) {
				return true
			}
		}
	}
	return false
}

// ── Shared @file expansion ────────────────────────────────────────────────────

// atFileRe matches @<non-whitespace> tokens used for file expansion.
var atFileRe = regexp.MustCompile(`@(\S+)`)

// ExpandAtFiles replaces @filepath tokens in text with the contents of those
// files. Relative paths are resolved against cwd when non-empty. Tokens whose
// paths cannot be read are left unchanged. Returns the expanded text and the
// list of successfully expanded file paths.
func ExpandAtFiles(text, cwd string) (string, []string) {
	var expanded []string
	result := atFileRe.ReplaceAllStringFunc(text, func(match string) string {
		path := match[1:] // strip leading '@'
		if cwd != "" && !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return match
		}
		expanded = append(expanded, path)
		return string(data)
	})
	return result, expanded
}
