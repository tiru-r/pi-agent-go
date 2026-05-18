package agent

import (
	"encoding/json"
	"testing"
)

func TestRepairToolJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOK  bool
		wantOut string // expected JSON (empty = any valid JSON)
	}{
		{
			name:   "already valid — no repair needed",
			input:  `{"path":"/tmp/foo.go"}`,
			wantOK: false, // returns false when already valid
		},
		{
			name:    "trailing comma before }",
			input:   `{"path":"/tmp/foo.go",}`,
			wantOK:  true,
			wantOut: `{"path":"/tmp/foo.go"}`,
		},
		{
			name:    "trailing comma before ]",
			input:   `{"items":["a","b",]}`,
			wantOK:  true,
			wantOut: `{"items":["a","b"]}`,
		},
		{
			name:    "multiple trailing commas",
			input:   `{"a":1,"b":2,}`,
			wantOK:  true,
			wantOut: `{"a":1,"b":2}`,
		},
		{
			name:    "single-quoted keys and values",
			input:   `{'path':'/tmp/foo.go'}`,
			wantOK:  true,
			wantOut: `{"path":"/tmp/foo.go"}`,
		},
		{
			// A double-quoted bare string is valid JSON — no repair needed.
			name:   "already-valid bare string — no repair",
			input:  `"hello world"`,
			wantOK: false,
		},
		{
			// Single-quoted bare string → double-quote repair produces valid JSON string.
			// The {"value":...} wrapping only applies to strings that are still invalid after repair.
			name:    "single-quoted bare string",
			input:   `'hello world'`,
			wantOK:  true,
			wantOut: `"hello world"`,
		},
		{
			// The peek-ahead in dropTrailingCommas skips consecutive commas so that
			// all of them are recognised as trailing and dropped in one pass.
			name:    "multiple consecutive trailing commas before }",
			input:   `{"a":1,,,}`,
			wantOK:  true,
			wantOut: `{"a":1}`,
		},
		{
			name:    "multiple consecutive trailing commas before ]",
			input:   `[1,,,]`,
			wantOK:  true,
			wantOut: `[1]`,
		},
		{
			// Mid-sequence double comma is not trailing — must not be dropped.
			name:   "mid-sequence double comma — no repair",
			input:  `[1,,2]`,
			wantOK: false,
		},
		{
			name:   "completely invalid — no repair",
			input:  `not json at all }{`,
			wantOK: false,
		},
		{
			// A string value that contains ", }" must be preserved unchanged;
			// only the actual trailing comma (outside strings) should be dropped.
			name:    "comma in string value not mistaken for trailing comma",
			input:   `{"msg": "hello, }", "extra": 1,}`,
			wantOK:  true,
			wantOut: `{"msg": "hello, }", "extra": 1}`,
		},
		{
			// Escaped double-quote inside a string must not confuse the string-span
			// tracker in dropTrailingCommas.
			name:    "escaped double-quote inside string value",
			input:   `{"msg": "say \"hi\", }","x":1,}`,
			wantOK:  true,
			wantOut: `{"msg": "say \"hi\", }","x":1}`,
		},
		{
			name:    "BOM prefix",
			input:   "\xEF\xBB\xBF{\"key\":\"val\"}",
			wantOK:  true,
			wantOut: `{"key":"val"}`,
		},
		{
			name:    "control characters inside string value",
			input:   "{\"cmd\":\"\x01go test ./...\"}",
			wantOK:  true,
			wantOut: `{"cmd":"go test ./..."}`,
		},
		{
			// Repair removes the comma; whitespace is preserved (output is still valid JSON).
			name:   "trailing comma with whitespace before }",
			input:  "{\n  \"k\": \"v\" ,\n}",
			wantOK: true,
		},
		{
			// \' outside a double-quoted span is an escaped apostrophe → emit '
			// so the result is valid JSON: {"msg":"it's fine"}
			name:    "single quotes with escaped apostrophe",
			input:   `{'msg':'it\'s fine'}`,
			wantOK:  true,
			wantOut: `{"msg":"it's fine"}`,
		},
		{
			// Double-quote inside a single-quoted value must be treated as a literal
			// character, not a span toggle. Without inSingle tracking the old code
			// would flip inDouble=true mid-value and corrupt the output.
			name:    "double-quote literal inside single-quoted value",
			input:   `{'key':'val "x" here'}`,
			wantOK:  true,
			wantOut: `{"key":"val \"x\" here"}`,
		},
		{
			// Nested: double-quoted key, single-quoted value with a double-quote inside.
			name:    "double-quoted key, single-quoted value with inner double-quote",
			input:   `{"key":'say "hi"'}`,
			wantOK:  true,
			wantOut: `{"key":"say \"hi\""}`,
		},
		{
			// Literal newline (0x0A) inside a single-quoted value must be escaped to \n,
			// otherwise the resulting double-quoted string is invalid JSON.
			name:    "literal newline inside single-quoted value",
			input:   "{'key': 'line1\nline2'}",
			wantOK:  true,
			wantOut: `{"key": "line1\nline2"}`,
		},
		{
			// Literal tab inside a single-quoted value must be escaped to \t.
			name:    "literal tab inside single-quoted value",
			input:   "{'key': 'col1\tcol2'}",
			wantOK:  true,
			wantOut: `{"key": "col1\tcol2"}`,
		},
		{
			// Literal carriage return inside a single-quoted value must be escaped to \r.
			name:    "literal carriage return inside single-quoted value",
			input:   "{'key': 'a\rb'}",
			wantOK:  true,
			wantOut: `{"key": "a\rb"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := repairToolJSON([]byte(tc.input))
			if ok != tc.wantOK {
				t.Errorf("repairToolJSON(%q) ok=%v, want %v (output: %s)", tc.input, ok, tc.wantOK, out)
				return
			}
			if !ok {
				return
			}
			if !json.Valid(out) {
				t.Errorf("repairToolJSON(%q) output is not valid JSON: %s", tc.input, out)
			}
			if tc.wantOut != "" && string(out) != tc.wantOut {
				t.Errorf("repairToolJSON(%q) = %s, want %s", tc.input, out, tc.wantOut)
			}
		})
	}
}
