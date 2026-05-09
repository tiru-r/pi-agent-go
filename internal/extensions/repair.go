package extensions

import (
	"fmt"
	"regexp"
	"strings"
)

// RepairMode controls how aggressively Pi repairs broken extensions on load.
type RepairMode int

const (
	RepairOff        RepairMode = iota // no repairs attempted
	RepairSuggest                      // log suggestions but do not apply them
	RepairAutoSafe                     // apply provably safe fixes only
	RepairAutoStrict                   // apply aggressive heuristic transforms
)

// RepairFix describes a single repair that was suggested or applied.
type RepairFix struct {
	Description string
	Before      string // excerpt of original source
	After       string // excerpt of repaired source
	Safe        bool   // true = provably safe; false = heuristic
}

// RepairResult summarises the outcome of a repair pass.
type RepairResult struct {
	Mode        RepairMode
	Applied     []RepairFix
	Suggestions []RepairFix
	Source      string // repaired source (same as input if nothing changed)
}

// Repair analyses src and, depending on mode, applies or suggests fixes.
// scan is the pre-computed ScanResult for the same source.
func Repair(src string, mode RepairMode, scan ScanResult) RepairResult {
	if mode == RepairOff {
		return RepairResult{Mode: mode, Source: src}
	}

	res := RepairResult{Mode: mode, Source: src}

	for _, fix := range safeRepairs(src, scan) {
		if mode >= RepairAutoSafe {
			res.Source = strings.ReplaceAll(res.Source, fix.Before, fix.After)
			res.Applied = append(res.Applied, fix)
		} else {
			res.Suggestions = append(res.Suggestions, fix)
		}
	}

	if mode >= RepairAutoStrict {
		for _, fix := range strictRepairs(res.Source, scan) {
			res.Source = fix.After // strict repairs replace the whole source
			res.Applied = append(res.Applied, fix)
		}
	} else {
		for _, fix := range strictRepairs(src, scan) {
			res.Suggestions = append(res.Suggestions, fix)
		}
	}

	return res
}

// safeRepairs returns provably safe fixes: things where the original is
// clearly wrong and there is exactly one correct interpretation.
func safeRepairs(src string, scan ScanResult) []RepairFix {
	var fixes []RepairFix

	// Fix: pi.readfile → pi.readFile (wrong case)
	if reWrongCase.MatchString(src) {
		after := reWrongCase.ReplaceAllStringFunc(src, func(m string) string {
			return "pi.readFile"
		})
		fixes = append(fixes, RepairFix{
			Description: "fix pi.readfile → pi.readFile (case)",
			Before:      src,
			After:       after,
			Safe:        true,
		})
		src = after
	}

	// Fix: pi.writefile → pi.writeFile (wrong case)
	if reWrongCaseWrite.MatchString(src) {
		after := reWrongCaseWrite.ReplaceAllStringFunc(src, func(m string) string {
			return "pi.writeFile"
		})
		fixes = append(fixes, RepairFix{
			Description: "fix pi.writefile → pi.writeFile (case)",
			Before:      src,
			After:       after,
			Safe:        true,
		})
		src = after
	}

	// Fix: missing capability in pi.tool call (warn-only; can't auto-add caps to manifest)
	for _, cap := range scan.MissingCaps(Manifest{}) {
		fixes = append(fixes, RepairFix{
			Description: fmt.Sprintf("manifest missing capability %q inferred from source", cap),
			Safe:        true,
		})
	}

	// Fix: unavailable require() modules
	for _, imp := range scan.UnavailableImports() {
		fixes = append(fixes, RepairFix{
			Description: fmt.Sprintf("require(%q) not available; only 'path' and 'os' are built-in", imp),
			Safe:        true,
		})
	}

	_ = src // updated src used only within strict path
	return fixes
}

// strictRepairs returns heuristic pattern-based transforms. These change
// observable behaviour and are only applied in RepairAutoStrict mode.
func strictRepairs(src string, _ ScanResult) []RepairFix {
	var fixes []RepairFix

	// Transform: synchronous pi.readFile(path) calls that don't await the
	// result into awaited calls inside an async wrapper.
	if reUnawaited.MatchString(src) && !strings.Contains(src, "async function execute") {
		after := strings.Replace(src, "function execute(", "async function execute(", 1)
		after = reUnawaited.ReplaceAllStringFunc(after, func(m string) string {
			return "await " + m
		})
		fixes = append(fixes, RepairFix{
			Description: "wrap execute() as async and await pi.* calls",
			Before:      src,
			After:       after,
			Safe:        false,
		})
	}

	return fixes
}

var (
	reWrongCase      = regexp.MustCompile(`\bpi\.readfile\b`)
	reWrongCaseWrite = regexp.MustCompile(`\bpi\.writefile\b`)
	reUnawaited      = regexp.MustCompile(`(?:pi\.tool|pi\.http|pi\.exec|pi\.readFile|pi\.writeFile)\s*\(`)
)
