package lifecycle

import (
	"errors"
	"testing"
	"time"
)

func TestNotBeforeSatisfied(t *testing.T) {
	now := fixedNow
	cases := []struct {
		name       string
		params     map[string]string
		declaredAt time.Time
		now        time.Time
		want       bool
	}{
		{
			name:   "time in the past is satisfied",
			params: map[string]string{"time": now.Add(-time.Minute).Format(time.RFC3339)},
			now:    now,
			want:   true,
		},
		{
			name:   "time exactly at deadline is satisfied",
			params: map[string]string{"time": now.Format(time.RFC3339)},
			now:    now,
			want:   true,
		},
		{
			name:   "time in the future is not satisfied",
			params: map[string]string{"time": now.Add(time.Minute).Format(time.RFC3339)},
			now:    now,
			want:   false,
		},
		{
			name:       "duration elapsed is satisfied",
			params:     map[string]string{"duration": "30m"},
			declaredAt: now.Add(-time.Hour),
			now:        now,
			want:       true,
		},
		{
			name:       "duration exactly at deadline is satisfied",
			params:     map[string]string{"duration": "30m"},
			declaredAt: now.Add(-30 * time.Minute),
			now:        now,
			want:       true,
		},
		{
			name:       "duration not yet elapsed is not satisfied",
			params:     map[string]string{"duration": "30m"},
			declaredAt: now.Add(-time.Minute),
			now:        now,
			want:       false,
		},
		{
			name:   "missing params is not satisfied",
			params: map[string]string{},
			now:    now,
			want:   false,
		},
		{
			name:   "unparseable time is not satisfied",
			params: map[string]string{"time": "not-a-time"},
			now:    now,
			want:   false,
		},
		{
			name:   "unparseable duration is not satisfied",
			params: map[string]string{"duration": "nope"},
			now:    now,
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NotBeforeSatisfied(tc.params, tc.declaredAt, tc.now); got != tc.want {
				t.Errorf("NotBeforeSatisfied(%v, %v, %v) = %v, want %v", tc.params, tc.declaredAt, tc.now, got, tc.want)
			}
		})
	}
}

func TestNotBeforeDeadline(t *testing.T) {
	now := fixedNow
	t.Run("time param returns the parsed instant", func(t *testing.T) {
		deadline, ok := NotBeforeDeadline(map[string]string{"time": now.Format(time.RFC3339)}, now)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if !deadline.Equal(now) {
			t.Errorf("deadline = %v, want %v", deadline, now)
		}
	})
	t.Run("duration param is anchored at declaredAt", func(t *testing.T) {
		declaredAt := now.Add(-time.Hour)
		deadline, ok := NotBeforeDeadline(map[string]string{"duration": "30m"}, declaredAt)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if want := declaredAt.Add(30 * time.Minute); !deadline.Equal(want) {
			t.Errorf("deadline = %v, want %v", deadline, want)
		}
	})
	t.Run("missing params is not ok", func(t *testing.T) {
		if _, ok := NotBeforeDeadline(map[string]string{}, now); ok {
			t.Errorf("ok = true, want false")
		}
	})
}

func TestGitProxyCheckSatisfied(t *testing.T) {
	cases := []struct {
		overall string
		want    bool
	}{
		{OverallSuccess, true},
		{OverallFailure, true},
		{OverallNeutral, true},
		{OverallCancelled, true},
		{OverallTimedOut, true},
		{"pending", false},
		{"unknown", false},
		{"none", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.overall, func(t *testing.T) {
			if got := GitProxyCheckSatisfied(tc.overall); got != tc.want {
				t.Errorf("GitProxyCheckSatisfied(%q) = %v, want %v", tc.overall, got, tc.want)
			}
		})
	}
}

func TestHTTPGetSatisfied(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		params map[string]string
		want   bool
	}{
		{
			name:   "default expectStatus 200 matches",
			status: 200,
			body:   "ok",
			params: map[string]string{},
			want:   true,
		},
		{
			name:   "status mismatch is not satisfied",
			status: 503,
			body:   "ok",
			params: map[string]string{},
			want:   false,
		},
		{
			name:   "custom expectStatus matches",
			status: 201,
			body:   "created",
			params: map[string]string{"expectStatus": "201"},
			want:   true,
		},
		{
			name:   "custom expectStatus mismatch",
			status: 200,
			body:   "created",
			params: map[string]string{"expectStatus": "201"},
			want:   false,
		},
		{
			name:   "expectBody substring present",
			status: 200,
			body:   "the quick brown fox",
			params: map[string]string{"expectBody": "brown"},
			want:   true,
		},
		{
			name:   "expectBody substring absent",
			status: 200,
			body:   "the quick brown fox",
			params: map[string]string{"expectBody": "purple"},
			want:   false,
		},
		{
			name:   "expectBody present but status mismatch",
			status: 500,
			body:   "the quick brown fox",
			params: map[string]string{"expectBody": "brown"},
			want:   false,
		},
		{
			name:   "unparseable expectStatus falls back to 200",
			status: 200,
			body:   "ok",
			params: map[string]string{"expectStatus": "not-a-number"},
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HTTPGetSatisfied(tc.status, tc.body, tc.params); got != tc.want {
				t.Errorf("HTTPGetSatisfied(%d, %q, %v) = %v, want %v", tc.status, tc.body, tc.params, got, tc.want)
			}
		})
	}
}

func TestS3ObjectExistsSatisfied(t *testing.T) {
	if !S3ObjectExistsSatisfied(true) {
		t.Errorf("S3ObjectExistsSatisfied(true) = false, want true")
	}
	if S3ObjectExistsSatisfied(false) {
		t.Errorf("S3ObjectExistsSatisfied(false) = true, want false")
	}
}

func TestClassifyError(t *testing.T) {
	t.Run("unevaluatable kind", func(t *testing.T) {
		unevaluatable, reason, message := ClassifyError(&ProbeError{Kind: ProbeErrUnevaluatable, Message: "broker returned 401"})
		if !unevaluatable {
			t.Errorf("unevaluatable = false, want true")
		}
		if reason != ReasonProbeFailed {
			t.Errorf("reason = %q, want %q", reason, ReasonProbeFailed)
		}
		if message != "broker returned 401" {
			t.Errorf("message = %q, want %q", message, "broker returned 401")
		}
	})
	t.Run("transient kind", func(t *testing.T) {
		unevaluatable, _, _ := ClassifyError(&ProbeError{Kind: ProbeErrTransient, Message: "connection refused"})
		if unevaluatable {
			t.Errorf("unevaluatable = true, want false for a transient error")
		}
	})
	t.Run("unrecognized error is fail-safe unevaluatable", func(t *testing.T) {
		unevaluatable, reason, message := ClassifyError(errors.New("something unexpected"))
		if !unevaluatable {
			t.Errorf("unevaluatable = false, want true for an unrecognized error")
		}
		if reason != ReasonProbeFailed {
			t.Errorf("reason = %q, want %q", reason, ReasonProbeFailed)
		}
		if message != "something unexpected" {
			t.Errorf("message = %q, want %q", message, "something unexpected")
		}
	})
	t.Run("nil error is unevaluatable", func(t *testing.T) {
		unevaluatable, _, _ := ClassifyError(nil)
		if !unevaluatable {
			t.Errorf("unevaluatable = false, want true for nil")
		}
	})
}
