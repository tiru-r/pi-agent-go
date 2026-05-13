package conformance

import (
	"context"
	"errors"
	"strings"
)

// FlakeClass indicates whether a test failure is deterministic or transient.
type FlakeClass int8

const (
	// FlakeUnknown means the failure pattern is not recognized.
	FlakeUnknown FlakeClass = iota
	// FlakeDeterministic means the failure recurs on every run — a code or
	// schema bug that must be fixed before retrying.
	FlakeDeterministic
	// FlakeTransient means the failure may resolve on retry due to network,
	// timing, or external-service conditions.
	FlakeTransient
)

func (f FlakeClass) String() string {
	switch f {
	case FlakeDeterministic:
		return "deterministic"
	case FlakeTransient:
		return "transient"
	default:
		return "unknown"
	}
}

// transientPhrases is the set of lower-cased substrings that identify
// transient errors from network, I/O, and rate-limiting sources.
var transientPhrases = []string{
	"connection refused",
	"connection reset",
	"broken pipe",
	"no route to host",
	"network unreachable",
	"temporary failure",
	"i/o timeout",
	"read: connection timed out",
	"write: connection timed out",
	"unexpected eof",
	"eof",
	"too many requests",
	"rate limit",
	"service unavailable",
	"bad gateway",
	"gateway timeout",
}

// ClassifyError classifies err as deterministic, transient, or unknown.
// Returns FlakeUnknown when err is nil.
//
// Rules applied in order:
//  1. context.Canceled / context.DeadlineExceeded → FlakeTransient
//  2. Error message prefix "conformance mismatch" → FlakeDeterministic
//  3. Substring match against known transient network/IO phrases → FlakeTransient
//  4. Anything else → FlakeUnknown (callers should treat unknown as deterministic)
func ClassifyError(err error) FlakeClass {
	if err == nil {
		return FlakeUnknown
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return FlakeTransient
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "conformance mismatch") {
		return FlakeDeterministic
	}
	lower := strings.ToLower(msg)
	for _, phrase := range transientPhrases {
		if strings.Contains(lower, phrase) {
			return FlakeTransient
		}
	}
	return FlakeUnknown
}
