package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tiru-r/pi-agent-go/internal/conformance"
)

func TestClassifyError_Nil(t *testing.T) {
	if got := conformance.ClassifyError(nil); got != conformance.FlakeUnknown {
		t.Errorf("nil error: got %s, want unknown", got)
	}
}

func TestClassifyError_ContextErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"DeadlineExceeded", context.DeadlineExceeded},
		{"Canceled", context.Canceled},
		{"wrapped DeadlineExceeded", fmt.Errorf("probe failed: %w", context.DeadlineExceeded)},
		{"wrapped Canceled", fmt.Errorf("invoke: %w", context.Canceled)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conformance.ClassifyError(tc.err); got != conformance.FlakeTransient {
				t.Errorf("got %s, want transient", got)
			}
		})
	}
}

func TestClassifyError_ConformanceMismatch(t *testing.T) {
	cases := []string{
		"conformance mismatch (3 difference(s)):\n  $.foo: string mismatch",
		"conformance mismatch (1 difference(s)):\n  $[0]: missing in got",
	}
	for _, msg := range cases {
		t.Run(msg[:30], func(t *testing.T) {
			if got := conformance.ClassifyError(errors.New(msg)); got != conformance.FlakeDeterministic {
				t.Errorf("got %s, want deterministic", got)
			}
		})
	}
}

func TestClassifyError_TransientPhrases(t *testing.T) {
	cases := []string{
		"connection refused",
		"connection reset by peer",
		"broken pipe",
		"no route to host",
		"network unreachable",
		"temporary failure in name resolution",
		"i/o timeout",
		"read: connection timed out",
		"write: connection timed out",
		"unexpected eof",
		"EOF",
		"too many requests",
		"rate limit exceeded",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
	}
	for _, phrase := range cases {
		t.Run(phrase, func(t *testing.T) {
			err := errors.New("dial tcp: " + phrase)
			if got := conformance.ClassifyError(err); got != conformance.FlakeTransient {
				t.Errorf("phrase %q: got %s, want transient", phrase, got)
			}
		})
	}
}

func TestClassifyError_Unknown(t *testing.T) {
	cases := []string{
		"invalid argument",
		"permission denied",
		"not implemented",
		"panic: nil pointer dereference",
		"assertion failed: expected true",
		"schema validation error",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			if got := conformance.ClassifyError(errors.New(msg)); got != conformance.FlakeUnknown {
				t.Errorf("msg %q: got %s, want unknown", msg, got)
			}
		})
	}
}

func TestFlakeClassString(t *testing.T) {
	cases := map[conformance.FlakeClass]string{
		conformance.FlakeUnknown:       "unknown",
		conformance.FlakeDeterministic: "deterministic",
		conformance.FlakeTransient:     "transient",
	}
	for class, want := range cases {
		if got := class.String(); got != want {
			t.Errorf("FlakeClass(%d).String() = %q, want %q", class, got, want)
		}
	}
}
