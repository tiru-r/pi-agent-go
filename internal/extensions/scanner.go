package extensions

import (
	"regexp"
	"strings"
)

// ScanResult holds the output of a static analysis pass over extension JS source.
type ScanResult struct {
	// ForbiddenPatterns lists any detected dangerous patterns (eval, etc.).
	ForbiddenPatterns []string
	// Imports lists every module passed to require().
	Imports []string
	// InferredCaps lists capabilities inferred from pi.* API usage.
	InferredCaps []Capability
	// EvidenceLedger maps each inferred capability to the source patterns that support it.
	EvidenceLedger map[string][]string
}

var (
	reForbidden = []*regexp.Regexp{
		regexp.MustCompile(`\beval\s*\(`),
		regexp.MustCompile(`\bnew\s+Function\s*\(`),
		regexp.MustCompile(`process\.binding\s*\(`),
		regexp.MustCompile(`\bdlopen\b`),
	}
	reForbiddenNames = []string{
		"eval()",
		"new Function()",
		"process.binding()",
		"dlopen",
	}

	reRequire = regexp.MustCompile(`\brequire\s*\(\s*['"]([^'"]+)['"]\s*\)`)

	// pi.* API usage patterns → inferred capability
	piPatterns = []struct {
		re  *regexp.Regexp
		cap Capability
		key string
	}{
		{regexp.MustCompile(`pi\.tool\s*\(\s*['"](?:read|grep|find|ls)['"]\s*`), CapFSRead, "pi.tool(read/grep/find/ls)"},
		{regexp.MustCompile(`pi\.tool\s*\(\s*['"](?:write|edit|hashline_edit)['"]\s*`), CapFSWrite, "pi.tool(write/edit)"},
		{regexp.MustCompile(`pi\.tool\s*\(\s*['"]bash['"]\s*`), CapExec, "pi.tool(bash)"},
		{regexp.MustCompile(`pi\.exec\s*\(`), CapExec, "pi.exec()"},
		{regexp.MustCompile(`pi\.http\s*\(`), CapNetwork, "pi.http()"},
		{regexp.MustCompile(`pi\.env\s*\(`), CapEnv, "pi.env()"},
		{regexp.MustCompile(`pi\.readFile\s*\(`), CapFSRead, "pi.readFile()"},
		{regexp.MustCompile(`pi\.writeFile\s*\(`), CapFSWrite, "pi.writeFile()"},
		{regexp.MustCompile(`pi\.session\s*\(`), CapSession, "pi.session()"},
		{regexp.MustCompile(`pi\.ui\s*\(`), CapUI, "pi.ui()"},
		{regexp.MustCompile(`process\.env\b`), CapEnv, "process.env"},
	}
)

// Scan performs static analysis on JS extension source code.
// It does not execute the source. Returns a ScanResult with inferred
// capabilities, forbidden patterns detected, and evidence for each finding.
func Scan(src string) ScanResult {
	result := ScanResult{
		EvidenceLedger: make(map[string][]string),
	}

	// Forbidden patterns
	for i, re := range reForbidden {
		if re.MatchString(src) {
			result.ForbiddenPatterns = append(result.ForbiddenPatterns, reForbiddenNames[i])
		}
	}

	// require() imports
	for _, m := range reRequire.FindAllStringSubmatch(src, -1) {
		if len(m) > 1 {
			result.Imports = append(result.Imports, m[1])
		}
	}

	// pi.* capability evidence
	capSeen := make(map[Capability]bool)
	for _, p := range piPatterns {
		matches := p.re.FindAllString(src, -1)
		if len(matches) == 0 {
			continue
		}
		result.EvidenceLedger[string(p.cap)] = append(
			result.EvidenceLedger[string(p.cap)],
			p.key,
		)
		if !capSeen[p.cap] {
			capSeen[p.cap] = true
			result.InferredCaps = append(result.InferredCaps, p.cap)
		}
	}

	return result
}

// MissingCaps returns capabilities that the scan inferred as needed but
// are absent from the manifest.
func (s ScanResult) MissingCaps(manifest Manifest) []Capability {
	var missing []Capability
	for _, cap := range s.InferredCaps {
		if !manifest.has(cap) {
			missing = append(missing, cap)
		}
	}
	return missing
}

// ExcessCaps returns capabilities declared in the manifest but not inferred
// from the source (possible over-privilege).
func (s ScanResult) ExcessCaps(manifest Manifest) []Capability {
	inferred := make(map[Capability]bool, len(s.InferredCaps))
	for _, c := range s.InferredCaps {
		inferred[c] = true
	}
	var excess []Capability
	for _, c := range manifest.Capabilities {
		if !inferred[c] {
			excess = append(excess, c)
		}
	}
	return excess
}

// UnavailableImports returns require() modules that Pi does not provide.
func (s ScanResult) UnavailableImports() []string {
	available := map[string]bool{"path": true, "os": true}
	var unavail []string
	for _, imp := range s.Imports {
		if !available[imp] {
			unavail = append(unavail, imp)
		}
	}
	return unavail
}

// Summary returns a human-readable one-line summary of the scan result.
func (s ScanResult) Summary() string {
	var parts []string
	if len(s.ForbiddenPatterns) > 0 {
		parts = append(parts, "forbidden:"+strings.Join(s.ForbiddenPatterns, ","))
	}
	if len(s.InferredCaps) > 0 {
		caps := make([]string, len(s.InferredCaps))
		for i, c := range s.InferredCaps {
			caps[i] = string(c)
		}
		parts = append(parts, "caps:"+strings.Join(caps, ","))
	}
	if len(parts) == 0 {
		return "ok"
	}
	return strings.Join(parts, "; ")
}
