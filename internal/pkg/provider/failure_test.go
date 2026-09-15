// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// failureServer stands in for Morpheus with one failed instance, letting each
// test decide what the instance and its history say.
func failureServer(t *testing.T, instance map[string]any, history map[string]any, historyStatus int) *Provisioner {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/instances/430":
			json.NewEncoder(w).Encode(map[string]any{"instance": instance})
		case "/api/instances/430/history":
			if historyStatus != 0 {
				w.WriteHeader(historyStatus)

				return
			}

			json.NewEncoder(w).Encode(history)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{Endpoint: server.URL, Token: "t"})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	return NewProvisioner(client, nil, "")
}

func failedInstance(extra map[string]any) map[string]any {
	instance := map[string]any{"id": 430, "name": "cluster-01-prod-workers-86m9mq", "status": "failed"}
	for k, v := range extra {
		instance[k] = v
	}

	return instance
}

// The whole point: the step that failed, and what Morpheus said about it,
// reaches the error instead of a pointer to another UI.
func TestDescribeInstanceFailureNamesTheFailedStep(t *testing.T) {
	p := failureServer(t,
		failedInstance(map[string]any{"statusMessage": "Provision failed"}),
		map[string]any{"processes": []map[string]any{
			{"id": 1, "processType": "provision", "displayName": "Provision", "status": "complete"},
			{"id": 2, "processType": "provision", "displayName": "Provision", "status": "failed",
				"events": []map[string]any{
					{"displayName": "Image Prep", "status": "complete"},
					{"displayName": "Create VM", "status": "failed",
						"error": "  UEFI firmware not available on host\n  hvm-node-02  "},
				}},
		}},
		0,
	)

	got := p.describeInstanceFailure(t.Context(), zap.NewNop(), &Instance{ID: 430})

	if !strings.Contains(got, "Create VM") {
		t.Errorf("failed step is not named: %q", got)
	}

	if !strings.Contains(got, "UEFI firmware not available on host hvm-node-02") {
		t.Errorf("step error is missing or not flattened onto one line: %q", got)
	}

	// The generic status message is still worth carrying, after the specific one.
	if !strings.Contains(got, "Provision failed") {
		t.Errorf("status message dropped: %q", got)
	}

	if strings.Index(got, "Create VM") > strings.Index(got, "Provision failed") {
		t.Errorf("specific step should come before the generic status: %q", got)
	}
}

// A completed process later in the history must not hide the failure before
// it, and a failed one must win over a successful event inside it.
func TestDescribeInstanceFailureFindsMostRecentFailure(t *testing.T) {
	p := failureServer(t,
		failedInstance(nil),
		map[string]any{"processes": []map[string]any{
			{"id": 1, "displayName": "Provision", "status": "failed", "error": "first attempt: no capacity"},
			{"id": 2, "displayName": "Provision", "status": "failed", "error": "second attempt: datastore mvm-volumes not found"},
			{"id": 3, "displayName": "Stop", "status": "complete"},
		}},
		0,
	)

	got := p.describeInstanceFailure(t.Context(), zap.NewNop(), &Instance{ID: 430})

	if !strings.Contains(got, "second attempt") {
		t.Errorf("most recent failure not chosen: %q", got)
	}

	if strings.Contains(got, "first attempt") {
		t.Errorf("stale earlier failure included: %q", got)
	}
}

// Older appliances spell the event list processEvents.
func TestDescribeInstanceFailureReadsProcessEvents(t *testing.T) {
	p := failureServer(t,
		failedInstance(nil),
		map[string]any{"processes": []map[string]any{
			{"id": 1, "displayName": "Provision", "status": "failed",
				"processEvents": []map[string]any{
					{"displayName": "Clone Image", "status": "failed", "error": "image not present on cluster"},
				}},
		}},
		0,
	)

	got := p.describeInstanceFailure(t.Context(), zap.NewNop(), &Instance{ID: 430})

	if !strings.Contains(got, "Clone Image") || !strings.Contains(got, "image not present") {
		t.Errorf("processEvents variant not read: %q", got)
	}
}

// A history endpoint that fails must not mask what the instance itself says.
func TestDescribeInstanceFailureSurvivesHistoryError(t *testing.T) {
	p := failureServer(t,
		failedInstance(map[string]any{"errorMessage": "Resource pool has no eligible hosts"}),
		nil,
		http.StatusInternalServerError,
	)

	got := p.describeInstanceFailure(t.Context(), zap.NewNop(), &Instance{ID: 430})

	if !strings.Contains(got, "Resource pool has no eligible hosts") {
		t.Errorf("instance error lost when history was unavailable: %q", got)
	}
}

// The listing an instance was found through may omit the messages, so the
// instance is re-read by id before describing it.
func TestDescribeInstanceFailureRereadsTheInstance(t *testing.T) {
	p := failureServer(t,
		failedInstance(map[string]any{"errorMessage": "only visible on a direct read"}),
		map[string]any{"processes": []any{}},
		0,
	)

	// Deliberately passed with no messages, as a listing would return it.
	got := p.describeInstanceFailure(t.Context(), zap.NewNop(), &Instance{ID: 430, Name: "x", Status: "failed"})

	if !strings.Contains(got, "only visible on a direct read") {
		t.Errorf("instance was not re-read for its messages: %q", got)
	}
}

// Morpheus often sets statusMessage and errorMessage to the same text.
func TestDescribeInstanceFailureDoesNotRepeatItself(t *testing.T) {
	p := failureServer(t,
		failedInstance(map[string]any{"errorMessage": "Provision failed: no capacity", "statusMessage": "no capacity"}),
		map[string]any{"processes": []any{}},
		0,
	)

	got := p.describeInstanceFailure(t.Context(), zap.NewNop(), &Instance{ID: 430})

	if strings.Count(got, "no capacity") != 1 {
		t.Errorf("duplicate message repeated: %q", got)
	}
}

// When Morpheus says nothing at all, say so rather than emitting an empty
// reason -- and keep the pointer to where an operator can look.
func TestDescribeInstanceFailureWithNoReason(t *testing.T) {
	p := failureServer(t, failedInstance(nil), map[string]any{"processes": []any{}}, 0)

	got := p.describeInstanceFailure(t.Context(), zap.NewNop(), &Instance{ID: 430})

	if !strings.Contains(got, "no reason") {
		t.Errorf("empty reason not explained: %q", got)
	}
}

// The text lands in a Machine Request status shown in a UI, not a log, so a
// step that dumps a stack trace must be cut short.
func TestDescribeInstanceFailureIsBounded(t *testing.T) {
	p := failureServer(t,
		failedInstance(nil),
		map[string]any{"processes": []map[string]any{
			{"id": 1, "displayName": "Provision", "status": "failed", "error": strings.Repeat("x", 5000)},
		}},
		0,
	)

	got := p.describeInstanceFailure(t.Context(), zap.NewNop(), &Instance{ID: 430})

	if len(got) > failureDetailLimit+3 {
		t.Errorf("reason is %d bytes, want at most %d", len(got), failureDetailLimit+3)
	}

	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncation not marked: %q", got[len(got)-10:])
	}
}

// A process that merely ran is not a failure, however much it logged.
func TestFailedStepIgnoresSuccessfulOutput(t *testing.T) {
	p := failureServer(t,
		failedInstance(nil),
		map[string]any{"processes": []map[string]any{
			{"id": 1, "displayName": "Provision", "status": "complete", "message": "all good", "output": "lots of output"},
		}},
		0,
	)

	if got := p.failedStep(t.Context(), zap.NewNop(), 430); got != "" {
		t.Errorf("a completed process was reported as the failure: %q", got)
	}
}
