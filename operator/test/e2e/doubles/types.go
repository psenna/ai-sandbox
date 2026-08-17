package main

import "time"

// The shapes below are duplicated (not shared) with the e2e module's own
// harness/doubles client types deliberately: broker and model are
// independent HTTP contracts, not shared Go types, and the doubles module
// must stay a separate, zero-dependency Go module (see go.mod). JSON on the
// wire is the actual contract between the two.

// PRState is a canned pull-request state served by the fake broker.
type PRState struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"` // "open" | "closed"
	Mergeable *bool  `json:"mergeable"`
	Head      string `json:"head"`
	Base      string `json:"base"`
	URL       string `json:"url"`
}

// CheckRun is a single named check within a CheckSummary.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued|in_progress|completed
	Conclusion string `json:"conclusion"` // success|failure|neutral|cancelled|skipped|timed_out|""
}

// WorkflowRun is a single named workflow run within a CheckSummary.
type WorkflowRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
}

// CheckSummary is the canned CI status served by the fake broker for a ref.
type CheckSummary struct {
	Overall   string        `json:"overall"` // none|pending|failure|success|unknown
	Checks    []CheckRun    `json:"checks"`
	Workflows []WorkflowRun `json:"workflows"`
}

// IssueState is a canned issue state served by the fake broker.
type IssueState struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	State  string   `json:"state"` // "open" | "closed"
	Body   string   `json:"body"`
	URL    string   `json:"url"`
	Labels []string `json:"labels"`
}

// RecordedRequest is one entry in a double's request log, exposed at
// GET /_control/requests.
type RecordedRequest struct {
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Time   time.Time `json:"time"`
}
