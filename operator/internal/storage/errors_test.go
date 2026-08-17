package storage

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestError_ImplementsErrorAndUnwrap(t *testing.T) {
	inner := fmt.Errorf("dial tcp: connection refused")
	e := &Error{Op: "Get", Backend: "s3", Key: "some/key", Kind: ErrUnreachable, Err: inner}

	if !errors.Is(e, ErrUnreachable) {
		t.Error("errors.Is(e, ErrUnreachable) = false, want true")
	}
	if !errors.Is(e, inner) {
		t.Error("errors.Is(e, inner) = false, want true (Unwrap must expose both Kind and Err)")
	}
	if errors.Is(e, ErrNotFound) {
		t.Error("errors.Is(e, ErrNotFound) = true, want false")
	}

	msg := e.Error()
	for _, want := range []string{"s3", "Get", "some/key"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}
}

func TestError_UnwrapWithoutInnerErr(t *testing.T) {
	e := &Error{Op: "Stat", Backend: "fs", Key: "k", Kind: ErrNotFound}
	if !errors.Is(e, ErrNotFound) {
		t.Error("errors.Is with nil Err should still match Kind")
	}
}

func TestIsHelpers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		is   func(error) bool
		want bool
	}{
		{"NotFound matches", &Error{Kind: ErrNotFound}, IsNotFound, true},
		{"NotFound doesn't match Unreachable", &Error{Kind: ErrNotFound}, IsUnreachable, false},
		{"Unreachable matches", &Error{Kind: ErrUnreachable}, IsUnreachable, true},
		{"Corrupt matches", &Error{Kind: ErrCorrupt}, IsCorrupt, true},
		{"Permission matches", &Error{Kind: ErrPermission}, IsPermission, true},
		{"Invalid matches", &Error{Kind: ErrInvalid}, IsInvalid, true},
		{"nil error never matches", nil, IsNotFound, false},
		{"plain error never matches", errors.New("boom"), IsNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.is(tc.err); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	retryable := []error{ErrUnreachable, &Error{Kind: ErrUnreachable}}
	notRetryable := []error{ErrNotFound, ErrInvalid, ErrPermission, ErrCorrupt,
		&Error{Kind: ErrNotFound}, &Error{Kind: ErrInvalid}, &Error{Kind: ErrPermission}, &Error{Kind: ErrCorrupt},
		nil, errors.New("boom")}

	for _, err := range retryable {
		if !IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = false, want true", err)
		}
	}
	for _, err := range notRetryable {
		if IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = true, want false", err)
		}
	}
}
