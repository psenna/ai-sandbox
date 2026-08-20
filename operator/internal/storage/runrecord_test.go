package storage

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// testRunRecord returns a fully-populated, valid RunRecord for golden and
// round-trip testing.
func testRunRecord(t *testing.T) RunRecord {
	t.Helper()
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parsing test time %q: %v", s, err)
		}
		return ts
	}
	pendingAt := at("2026-02-01T10:00:00Z")
	return RunRecord{
		SchemaVersion: RunRecordSchemaVersion,
		CreatedAt:     at("2026-02-01T12:30:00Z"),
		ClusterID:     "cluster-a",
		Namespace:     "ns-a",
		Name:          "env-a",
		UID:           "uid-a",
		Generation:    3,
		Repo:          "owner/repo",
		Task: TaskRecord{
			Prompt:   "implement the feature",
			IssueRef: &IssueRecord{Repo: "owner/repo", Number: 42},
		},
		ClassRef:         "class-a",
		AgentImage:       "ghcr.io/example/agent:v1",
		AgentImageDigest: "sha256:" + strings.Repeat("d", 64),
		FinalPhase:       "Done",
		FinalReason:      "agent reported success",
		PhaseHistory: []PhaseTransitionRecord{
			{Phase: "Pending", At: pendingAt, Reason: "Created"},
			{Phase: "Ready", At: at("2026-02-01T10:00:01Z")},
			{Phase: "Restoring", At: at("2026-02-01T10:00:05Z")},
			{Phase: "Running", At: at("2026-02-01T10:00:07Z")},
			{Phase: "Freezing", At: at("2026-02-01T11:00:00Z"), Reason: "SnapshotInProgress"},
			{Phase: "Waiting", At: at("2026-02-01T11:01:00Z"), Reason: "WaitDeclared"},
			{Phase: "Restoring", At: at("2026-02-01T11:10:00Z")},
			{Phase: "Running", At: at("2026-02-01T11:10:02Z")},
			{Phase: "Done", At: at("2026-02-01T12:30:00Z"), Reason: "ArchiveWritten"},
		},
		QueuedDurationMillis:  1_000,
		RunningDurationMillis: 5_400_000,
		WaitingDurationMillis: 540_000,
		SlotWaitMillis:        500,
		FreezeCount:           1,
		WakeCount:             1,
		Snapshots: []SnapshotRecord{
			{
				Seq:            1,
				URI:            "s3://bucket/cluster-a/ns-a/env-a/uid-a/snapshots/00001-2026-02-01t11_00_00z",
				SizeBytes:      1234,
				SHA256:         strings.Repeat("b", 64),
				TakenAt:        at("2026-02-01T11:00:05Z"),
				DurationMillis: 5_000,
			},
		},
		WaitFor: &WaitForRecord{
			Type:       "NotBefore",
			Reason:     "waiting for approval",
			DeclaredAt: ptrTime(at("2026-02-01T11:01:00Z")),
			Params:     map[string]string{"notBefore": "2026-02-01T11:10:00Z"},
		},
		ProbeAttempt: &ProbeAttemptRecord{
			Type:       "NotBefore",
			Phase:      "Satisfied",
			Attempts:   3,
			LastResult: "satisfied",
			Reason:     "ProbeSatisfied",
			Message:    "notBefore reached",
		},
		GitState: &GitStateRecord{
			Branch:  "feat/feature",
			HeadSHA: strings.Repeat("c", 40),
			PullRequest: &PullRequestRecord{
				Repo:   "owner/repo",
				Number: 7,
			},
		},
		Context: ContextRecord{
			Present: true,
			URI:     "s3://bucket/cluster-a/ns-a/env-a/uid-a/archive/context.tar.zst",
		},
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestRunRecord_GoldenMarshal(t *testing.T) {
	rec := testRunRecord(t)
	got, err := rec.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	assertGoldenPath(t, "runrecord.json", got)
}

func TestRunRecord_MarshalParseRoundTrip(t *testing.T) {
	rec := testRunRecord(t)
	b1, err := rec.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, err := ParseRunRecord(bytes.NewReader(b1))
	if err != nil {
		t.Fatalf("ParseRunRecord: %v", err)
	}
	b2, err := parsed.Marshal()
	if err != nil {
		t.Fatalf("Marshal (2nd): %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("Marshal -> Parse -> Marshal not byte-identical:\n--- 1 ---\n%s\n--- 2 ---\n%s", b1, b2)
	}
}

func TestParseRunRecord_RejectsUnknownFields(t *testing.T) {
	b, err := testRunRecord(t).Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Inject a field no schema version could know about.
	bad := strings.Replace(string(b), `"schemaVersion": 1`, `"schemaVersion": 1, "bogusField": true`, 1)
	if _, err := ParseRunRecord(strings.NewReader(bad)); err == nil {
		t.Fatalf("ParseRunRecord: want error for unknown field, got nil")
	}
}

func TestParseRunRecord_RejectsBadSchemaVersion(t *testing.T) {
	rec := testRunRecord(t)
	rec.SchemaVersion = 2
	b, err := rec.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := ParseRunRecord(bytes.NewReader(b)); err == nil {
		t.Fatalf("ParseRunRecord: want error for schemaVersion 2, got nil")
	}
}

func TestRunRecord_Validate(t *testing.T) {
	valid := func() RunRecord { return testRunRecord(t) }

	cases := []struct {
		name    string
		mutate  func(r RunRecord) RunRecord
		wantErr bool
	}{
		{"valid", func(r RunRecord) RunRecord { return r }, false},
		{"bad schema version", func(r RunRecord) RunRecord { r.SchemaVersion = 2; return r }, true},
		{"zero createdAt", func(r RunRecord) RunRecord { r.CreatedAt = time.Time{}; return r }, true},
		{"empty clusterID", func(r RunRecord) RunRecord { r.ClusterID = ""; return r }, true},
		{"empty namespace", func(r RunRecord) RunRecord { r.Namespace = ""; return r }, true},
		{"empty name", func(r RunRecord) RunRecord { r.Name = ""; return r }, true},
		{"empty uid", func(r RunRecord) RunRecord { r.UID = ""; return r }, true},
		{"empty repo", func(r RunRecord) RunRecord { r.Repo = ""; return r }, true},
		{"empty classRef", func(r RunRecord) RunRecord { r.ClassRef = ""; return r }, true},
		{"empty agentImage", func(r RunRecord) RunRecord { r.AgentImage = ""; return r }, true},
		{"bad finalPhase", func(r RunRecord) RunRecord { r.FinalPhase = "Exploded"; return r }, true},
		{"empty finalPhase allowed (deleted before reconcile)", func(r RunRecord) RunRecord { r.FinalPhase = ""; return r }, false},
		{"negative freezeCount", func(r RunRecord) RunRecord { r.FreezeCount = -1; return r }, true},
		{"negative wakeCount", func(r RunRecord) RunRecord { r.WakeCount = -1; return r }, true},
		{"phaseHistory over cap", func(r RunRecord) RunRecord {
			history := make([]PhaseTransitionRecord, 0, maxPhaseHistoryEntries+1)
			for i := 0; i <= maxPhaseHistoryEntries; i++ {
				history = append(history, PhaseTransitionRecord{Phase: "Pending", At: time.Now().Add(time.Duration(i) * time.Second)})
			}
			r.PhaseHistory = history
			return r
		}, true},
		{"empty phaseHistory allowed (deleted before reconcile)", func(r RunRecord) RunRecord { r.PhaseHistory = nil; return r }, false},
		{"phaseHistory unknown phase", func(r RunRecord) RunRecord { r.PhaseHistory[3].Phase = "Exploded"; return r }, true},
		{"phaseHistory zero at", func(r RunRecord) RunRecord { r.PhaseHistory[1].At = time.Time{}; return r }, true},
		{"phaseHistory consecutive duplicate", func(r RunRecord) RunRecord {
			r.PhaseHistory[1] = r.PhaseHistory[0]
			return r
		}, true},
		{"duplicate snapshot seq", func(r RunRecord) RunRecord {
			r.Snapshots = append(r.Snapshots, r.Snapshots[0])
			return r
		}, true},
		{"snapshot negative seq", func(r RunRecord) RunRecord { r.Snapshots[0].Seq = -1; return r }, true},
		{"snapshot malformed sha256", func(r RunRecord) RunRecord { r.Snapshots[0].SHA256 = "nope"; return r }, true},
		{"snapshot negative size", func(r RunRecord) RunRecord { r.Snapshots[0].SizeBytes = -5; return r }, true},
		{"context present with empty uri", func(r RunRecord) RunRecord { r.Context.URI = ""; return r }, true},
		{"context absent with empty reason", func(r RunRecord) RunRecord { r.Context.Present = false; r.Context.URI = ""; r.Context.Reason = ""; return r }, true},
		{"context absent with reason allowed", func(r RunRecord) RunRecord {
			r.Context.Present = false
			r.Context.URI = ""
			r.Context.Reason = "no agent home snapshot"
			return r
		}, false},
		{"waitFor empty type", func(r RunRecord) RunRecord { r.WaitFor.Type = ""; return r }, true},
		{"probeAttempt empty phase", func(r RunRecord) RunRecord { r.ProbeAttempt.Phase = ""; return r }, true},
		{"gitState bad headSHA", func(r RunRecord) RunRecord { r.GitState.HeadSHA = "xyz"; return r }, true},
		{"gitState empty pullRequest repo", func(r RunRecord) RunRecord { r.GitState.PullRequest.Repo = ""; return r }, true},
		{"gitState pullRequest number 0", func(r RunRecord) RunRecord { r.GitState.PullRequest.Number = 0; return r }, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mutate(valid()).Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate(): want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate(): want nil, got %v", err)
			}
		})
	}
}
