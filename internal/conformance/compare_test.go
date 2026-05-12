package conformance_test

import (
	"strings"
	"testing"

	"github.com/tiru-r/pi-agent-go/internal/conformance"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func mustEqual(t *testing.T, got, want string, opts conformance.CompareOptions) {
	t.Helper()
	if err := conformance.Compare([]byte(got), []byte(want), opts); err != nil {
		t.Errorf("expected equal, got diff:\n%v", err)
	}
}

func mustDiff(t *testing.T, got, want string, opts conformance.CompareOptions) {
	t.Helper()
	if err := conformance.Compare([]byte(got), []byte(want), opts); err == nil {
		t.Error("expected a diff but got nil (equal)")
	}
}

func mustDiffContaining(t *testing.T, got, want, substr string, opts conformance.CompareOptions) {
	t.Helper()
	err := conformance.Compare([]byte(got), []byte(want), opts)
	if err == nil {
		t.Error("expected a diff but got nil (equal)")
		return
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("diff report does not contain %q:\n%v", substr, err)
	}
}

var noOpts = conformance.CompareOptions{}

// ── null / missing equivalence ────────────────────────────────────────────────

func TestNullEquivalence(t *testing.T) {
	t.Run("null vs missing key in got", func(t *testing.T) {
		mustEqual(t, `{"a":null}`, `{}`, noOpts)
	})
	t.Run("missing key in got vs null in want", func(t *testing.T) {
		mustEqual(t, `{}`, `{"a":null}`, noOpts)
	})
	t.Run("null vs null", func(t *testing.T) {
		mustEqual(t, `{"a":null}`, `{"a":null}`, noOpts)
	})
	t.Run("null differs from real value", func(t *testing.T) {
		mustDiff(t, `{"a":null}`, `{"a":"x"}`, noOpts)
	})
	t.Run("real value differs from null", func(t *testing.T) {
		mustDiff(t, `{"a":"x"}`, `{"a":null}`, noOpts)
	})
}

// ── empty-array equivalence ───────────────────────────────────────────────────

func TestEmptyArrayEquivalence(t *testing.T) {
	t.Run("empty array vs missing key", func(t *testing.T) {
		mustEqual(t, `{"a":[]}`, `{}`, noOpts)
	})
	t.Run("missing key vs empty array", func(t *testing.T) {
		mustEqual(t, `{}`, `{"a":[]}`, noOpts)
	})
	t.Run("empty array vs null", func(t *testing.T) {
		mustEqual(t, `{"a":[]}`, `{"a":null}`, noOpts)
	})
	t.Run("empty array vs non-empty differs", func(t *testing.T) {
		mustDiff(t, `{"a":[]}`, `{"a":[1]}`, noOpts)
	})
	t.Run("non-empty vs empty differs", func(t *testing.T) {
		mustDiff(t, `{"a":[1]}`, `{"a":[]}`, noOpts)
	})
}

// ── object key ordering ───────────────────────────────────────────────────────

func TestObjectKeyOrdering(t *testing.T) {
	t.Run("top-level object different key order", func(t *testing.T) {
		mustEqual(t, `{"b":2,"a":1}`, `{"a":1,"b":2}`, noOpts)
	})
	t.Run("nested object different key order", func(t *testing.T) {
		mustEqual(t,
			`{"o":{"z":9,"a":1,"m":5}}`,
			`{"o":{"a":1,"m":5,"z":9}}`,
			noOpts)
	})
	t.Run("value mismatch despite same keys", func(t *testing.T) {
		mustDiff(t, `{"a":1}`, `{"a":2}`, noOpts)
	})
}

// ── float epsilon ─────────────────────────────────────────────────────────────

func TestFloatEpsilon(t *testing.T) {
	t.Run("within default epsilon", func(t *testing.T) {
		mustEqual(t, `1.000000000001`, `1.0`, noOpts)
	})
	t.Run("outside default epsilon", func(t *testing.T) {
		mustDiff(t, `1.1`, `1.0`, noOpts)
	})
	t.Run("custom epsilon larger", func(t *testing.T) {
		mustEqual(t, `1.05`, `1.0`, conformance.CompareOptions{FloatEpsilon: 0.1})
	})
	t.Run("custom epsilon smaller than diff", func(t *testing.T) {
		mustDiff(t, `1.05`, `1.0`, conformance.CompareOptions{FloatEpsilon: 0.01})
	})
	t.Run("negative floats within epsilon", func(t *testing.T) {
		mustEqual(t, `-1.000000000001`, `-1.0`, noOpts)
	})
}

// ── ordered arrays (hostcall logs) ────────────────────────────────────────────

func TestOrderedArrays(t *testing.T) {
	t.Run("same order equal", func(t *testing.T) {
		mustEqual(t, `[1,2,3]`, `[1,2,3]`, noOpts)
	})
	t.Run("different order is a diff", func(t *testing.T) {
		mustDiff(t, `[3,2,1]`, `[1,2,3]`, noOpts)
	})
	t.Run("length mismatch reported", func(t *testing.T) {
		mustDiffContaining(t, `[1,2]`, `[1,2,3]`, "length mismatch", noOpts)
	})
	t.Run("hostcall log order preserved", func(t *testing.T) {
		got := `{"log":[{"k":"tool","name":"read"},{"k":"exec"}]}`
		want := `{"log":[{"k":"exec"},{"k":"tool","name":"read"}]}`
		mustDiff(t, got, want, noOpts)
	})
}

// ── registration list (unordered, keyed) ─────────────────────────────────────

func TestRegistrationList(t *testing.T) {
	opts := conformance.CompareOptions{
		RegistrationLists: map[string]string{"$.commands": "name"},
	}

	t.Run("same entries different order", func(t *testing.T) {
		got := `{"commands":[{"name":"b","val":2},{"name":"a","val":1}]}`
		want := `{"commands":[{"name":"a","val":1},{"name":"b","val":2}]}`
		mustEqual(t, got, want, opts)
	})
	t.Run("missing entry in got", func(t *testing.T) {
		got := `{"commands":[{"name":"a"}]}`
		want := `{"commands":[{"name":"a"},{"name":"b"}]}`
		mustDiffContaining(t, got, want, `name="b"`, opts)
	})
	t.Run("extra entry in got", func(t *testing.T) {
		got := `{"commands":[{"name":"a"},{"name":"extra"}]}`
		want := `{"commands":[{"name":"a"}]}`
		mustDiffContaining(t, got, want, `name="extra"`, opts)
	})
	t.Run("value mismatch inside entry", func(t *testing.T) {
		got := `{"commands":[{"name":"a","val":99}]}`
		want := `{"commands":[{"name":"a","val":1}]}`
		mustDiffContaining(t, got, want, "val", opts)
	})
	t.Run("shortcut key_id as identity field", func(t *testing.T) {
		opts2 := conformance.CompareOptions{
			RegistrationLists: map[string]string{"$.shortcuts": "key_id"},
		}
		got := `{"shortcuts":[{"key_id":"ctrl+z"},{"key_id":"ctrl+c"}]}`
		want := `{"shortcuts":[{"key_id":"ctrl+c"},{"key_id":"ctrl+z"}]}`
		mustEqual(t, got, want, opts2)
	})
}

// ── extra / missing keys ──────────────────────────────────────────────────────

func TestExtraAndMissingKeys(t *testing.T) {
	t.Run("extra key with real value", func(t *testing.T) {
		mustDiff(t, `{"a":1,"extra":true}`, `{"a":1}`, noOpts)
	})
	t.Run("extra key with null value ignored", func(t *testing.T) {
		mustEqual(t, `{"a":1,"extra":null}`, `{"a":1}`, noOpts)
	})
	t.Run("extra key with empty array ignored", func(t *testing.T) {
		mustEqual(t, `{"a":1,"extra":[]}`, `{"a":1}`, noOpts)
	})
	t.Run("missing required key", func(t *testing.T) {
		mustDiffContaining(t, `{}`, `{"required":"value"}`, "missing in got", noOpts)
	})
}

// ── type mismatches ───────────────────────────────────────────────────────────

func TestTypeMismatches(t *testing.T) {
	t.Run("string vs number", func(t *testing.T) {
		mustDiff(t, `{"a":"1"}`, `{"a":1}`, noOpts)
	})
	t.Run("bool vs string", func(t *testing.T) {
		mustDiff(t, `{"a":"true"}`, `{"a":true}`, noOpts)
	})
	t.Run("object vs array", func(t *testing.T) {
		mustDiff(t, `{"a":{}}`, `{"a":[]}`, noOpts)
	})
}

// ── diff report format ────────────────────────────────────────────────────────

func TestDiffReport(t *testing.T) {
	err := conformance.Compare(
		[]byte(`{"a":"got_val","b":1}`),
		[]byte(`{"a":"want_val","b":2}`),
		noOpts,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "2 difference(s)") {
		t.Errorf("expected 2 differences in report: %s", msg)
	}
	if !strings.Contains(msg, `"got_val"`) {
		t.Errorf("expected got value in report: %s", msg)
	}
	if !strings.Contains(msg, `"want_val"`) {
		t.Errorf("expected want value in report: %s", msg)
	}
}
