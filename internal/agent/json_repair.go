package agent

import (
	"bytes"
	"encoding/json"
	"strings"
)

// repairToolJSON attempts to fix common mistakes that small models make when
// emitting tool-call arguments. It returns the repaired bytes and true when the
// result is valid JSON. If repair fails or the input is already valid, it returns
// false and the caller should use the original bytes.
//
// Repairs attempted (in order):
//  1. Strip UTF-8 BOM and ASCII control characters (< 0x20 except tab/newline/CR).
//  2. Replace single-quoted string delimiters with double quotes.
//  3. Drop trailing commas before ] or }.
//  4. When the entire value is a bare top-level string (no surrounding braces),
//     wrap it as {"value":"<string>"} so the schema-object expectation is met.
func repairToolJSON(in []byte) ([]byte, bool) {
	if json.Valid(in) {
		return in, false // nothing to do
	}

	b := in

	// 1. Strip UTF-8 BOM.
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})

	// 2. Strip ASCII control chars (keep \t=0x09, \n=0x0A, \r=0x0D).
	b = stripControls(b)

	// 3. Replace single-quote string delimiters with double quotes.
	b = singleToDouble(b)

	// 4. Drop trailing commas before } or ].
	b = dropTrailingCommas(b)

	if json.Valid(b) {
		return b, true
	}

	// 5. Last resort: if the whole thing looks like a bare string, wrap it.
	trimmed := strings.TrimSpace(string(b))
	if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
		wrapped := []byte(`{"value":` + trimmed + `}`)
		if json.Valid(wrapped) {
			return wrapped, true
		}
	}

	return in, false
}

// stripControls removes ASCII control characters that aren't allowed in JSON
// strings (everything below 0x20 except tab, newline, carriage return).
// Always allocates a fresh buffer — must not alias the input.
func stripControls(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			continue
		}
		out = append(out, c)
	}
	return out
}

// singleToDouble replaces single-quote string delimiters with double quotes.
//
// State machine tracks two mutually exclusive spans:
//   - inDouble: inside a "…" span — single quotes are literal, escape sequences
//     are passed through unchanged.
//   - inSingle: inside a '…' span — double quotes are literal, \' is an escaped
//     apostrophe (emit ' drop backslash), other escapes pass through unchanged.
//   - neither: bare ' opens a single-quoted span (emitted as "), bare " opens a
//     double-quoted span.
func singleToDouble(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inDouble := false
	inSingle := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == '\\' && inDouble:
			// Inside double-quoted span: pass escape sequences through unchanged.
			out = append(out, c)
			i++
			if i < len(b) {
				out = append(out, b[i])
			}
		case c == '\\' && inSingle:
			// Inside single-quoted span: \' is an escaped apostrophe — unescape it.
			// Any other backslash sequence is passed through as-is.
			if i+1 < len(b) && b[i+1] == '\'' {
				out = append(out, '\'')
				i++
			} else {
				out = append(out, c)
			}
		case c == '"' && inSingle:
			// Literal double-quote inside a single-quoted span: escape it so the
			// output is valid JSON once the outer delimiters are converted.
			out = append(out, '\\', '"')
		case c == '"' && !inSingle:
			inDouble = !inDouble
			out = append(out, c)
		case c == '\'' && !inDouble:
			// Toggle single-quoted span; emit double-quote as the delimiter.
			inSingle = !inSingle
			out = append(out, '"')
		case inSingle && c == '\n':
			out = append(out, '\\', 'n')
		case inSingle && c == '\r':
			out = append(out, '\\', 'r')
		case inSingle && c == '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, c)
		}
	}
	return out
}

// dropTrailingCommas removes commas that appear immediately before a } or ]
// (possibly separated by whitespace and consecutive trailing commas).
// It tracks double-quoted string spans so commas inside string values
// (e.g. {"msg": "hello, }"}) are never mistakenly dropped.
func dropTrailingCommas(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inString := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < len(b) {
				// Pass escape sequence through so \" doesn't toggle inString.
				i++
				out = append(out, b[i])
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r' || b[j] == ',') {
				j++
			}
			if j < len(b) && (b[j] == '}' || b[j] == ']') {
				continue // drop trailing comma
			}
		}
		out = append(out, c)
	}
	return out
}
