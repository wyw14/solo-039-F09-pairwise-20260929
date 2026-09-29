package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func findOpenIncident(t *testing.T, plant *Plant, kind string) Incident {
	t.Helper()
	for _, inc := range plant.Incidents.List(true) {
		if inc.Kind == kind {
			return inc
		}
	}
	t.Fatalf("open incident %q not found", kind)
	return Incident{}
}

func TestIncidentTriggerCorrelatesRepeats(t *testing.T) {
	registry := NewIncidents()
	rev := uuid.NewString()
	ops := []BlockedOperationInput{{Name: opNameRefluxLease, Class: OpRefluxLease, Message: "capacity unavailable"}}

	first := registry.Trigger(KindRefluxCapacity, rev, "first", ops)
	second := registry.Trigger(KindRefluxCapacity, rev, "second", ops)

	if first.ID != second.ID {
		t.Fatal("repeat alert was not correlated to the original incident")
	}
	if len(second.Occurrences) != 2 {
		t.Fatalf("expected 2 traceable occurrences, got %d", len(second.Occurrences))
	}
	if second.Occurrences[0].ID == second.Occurrences[1].ID {
		t.Fatal("each trigger must keep its own occurrence identity")
	}
	if len(second.Operations) != 1 || second.Operations[0].Count != 2 {
		t.Fatalf("blocked operation must aggregate triggers: %+v", second.Operations)
	}
	if second.LastMessage != "second" {
		t.Fatal("latest trigger message must be visible")
	}

	if other := registry.Trigger(KindRefluxCapacity, uuid.NewString(), "other revision", ops); other.ID == first.ID {
		t.Fatal("alerts on another batch revision must open a separate incident")
	}
	if other := registry.Trigger(KindShutdown, rev, "other kind", ops); other.ID == first.ID {
		t.Fatal("alerts of another fault kind must open a separate incident")
	}
}

func TestIncidentTriggerKeepsMultipleBlockedOperations(t *testing.T) {
	registry := NewIncidents()
	inc := registry.Trigger(KindShutdown, uuid.NewString(), "shutdown failed", []BlockedOperationInput{
		{Name: opNameReliefShutdown, Class: OpSafetyShutdown, Message: "relief timeout"},
		{Name: opNameSteamClose, Class: OpSafetyShutdown, Message: "steam error"},
	})
	if len(inc.Operations) != 2 {
		t.Fatalf("expected 2 blocked operations, got %d", len(inc.Operations))
	}
	if len(inc.Occurrences[0].Operations) != 2 {
		t.Fatalf("occurrence must record both blocked operations, got %v", inc.Occurrences[0].Operations)
	}
}

func TestIncidentLifecycleStateMachine(t *testing.T) {
	registry := NewIncidents()
	inc := registry.Trigger(KindShutdown, uuid.NewString(), "boom", []BlockedOperationInput{
		{Name: opNameReliefShutdown, Class: OpSafetyShutdown},
	})

	if _, err := registry.RecordReview(inc.ID, "system", []CheckResult{{Class: OpSafetyShutdown, Passed: true}}); !errors.Is(err, ErrIncidentNotAcknowledged) {
		t.Fatalf("review before acknowledgement must be rejected, got %v", err)
	}

	inc, err := registry.Acknowledge(inc.ID, "ops", "manual fix")
	if err != nil {
		t.Fatal(err)
	}
	if inc.Status != StatusMitigated || !inc.Open || len(inc.Mitigations) != 1 {
		t.Fatalf("acknowledged incident must be mitigated and still open: %+v", inc)
	}

	// Fault recurs while mitigated: occurrence is appended, incident falls back to open.
	inc = registry.Trigger(KindShutdown, inc.Revision, "still broken", []BlockedOperationInput{
		{Name: opNameReliefShutdown, Class: OpSafetyShutdown},
	})
	if inc.Status != StatusOpen || len(inc.Occurrences) != 2 || len(inc.Mitigations) != 1 {
		t.Fatalf("recurring fault must reopen incident while keeping history: %+v", inc)
	}
	if inc, _ = registry.Acknowledge(inc.ID, "ops", "fixed again"); inc.Status != StatusMitigated {
		t.Fatalf("incident must be mitigated again: %+v", inc)
	}

	// Failing review keeps the incident open.
	inc, err = registry.RecordReview(inc.ID, "system", []CheckResult{{Operation: opNameReliefShutdown, Class: OpSafetyShutdown, Passed: false, Reason: "still timed out"}})
	if err != nil {
		t.Fatal(err)
	}
	if inc.Status != StatusMitigated || !inc.Open || len(inc.Reviews) != 1 || inc.Reviews[0].Passed {
		t.Fatalf("failed review must keep incident open: %+v", inc)
	}

	// Passing review after the repair closes the incident without another acknowledgement.
	inc, err = registry.RecordReview(inc.ID, "system", []CheckResult{{Operation: opNameReliefShutdown, Class: OpSafetyShutdown, Passed: true}})
	if err != nil {
		t.Fatal(err)
	}
	if inc.Status != StatusResolved || inc.Open || inc.ResolvedAt == nil || len(inc.Reviews) != 2 {
		t.Fatalf("passing review must resolve incident: %+v", inc)
	}

	if _, err := registry.Acknowledge(inc.ID, "ops", ""); !errors.Is(err, ErrIncidentResolved) {
		t.Fatalf("acknowledging a resolved incident must fail, got %v", err)
	}
	if _, err := registry.RecordReview(inc.ID, "system", nil); !errors.Is(err, ErrIncidentResolved) {
		t.Fatalf("reviewing a resolved incident must fail, got %v", err)
	}

	// A resolved incident is never reused: the next trigger opens a fresh one.
	fresh := registry.Trigger(KindShutdown, inc.Revision, "again", nil)
	if fresh.ID == inc.ID {
		t.Fatal("a trigger after resolution must create a new incident")
	}
}

func TestRefluxIncidentReviewFlow(t *testing.T) {
	plant := NewPlant(t.TempDir())
	if _, err := plant.Start(context.Background(), "baseline", "primary"); err != nil {
		t.Fatal(err)
	}
	lease, err := plant.AcquireReflux("T-101", 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plant.AcquireReflux("overdrawn", 1.0); err == nil {
		t.Fatal("second lease beyond capacity must fail")
	}

	inc := findOpenIncident(t, plant, KindRefluxCapacity)
	if inc.Revision != plant.Campaign.Snapshot().Revision.String() {
		t.Fatal("incident must be linked to the batch revision that triggered it")
	}
	if len(inc.Operations) != 1 || inc.Operations[0].Name != opNameRefluxLease || inc.Operations[0].Params["owner"] != "overdrawn" {
		t.Fatalf("blocked reflux operation must be linked: %+v", inc.Operations)
	}

	if _, _, err := plant.ReviewIncident(inc.ID, "ops"); !errors.Is(err, ErrIncidentNotAcknowledged) {
		t.Fatalf("review before acknowledgement must be rejected, got %v", err)
	}
	if _, err := plant.AcknowledgeIncident(inc.ID, "", ""); err == nil {
		t.Fatal("acknowledgement without operator must be rejected")
	}
	if inc, err = plant.AcknowledgeIncident(inc.ID, "ops", "freed capacity"); err != nil {
		t.Fatal(err)
	}

	// Capacity still exhausted: review runs, fails, incident stays open.
	inc, checks, err := plant.ReviewIncident(inc.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if inc.Open != true || len(checks) != 1 || checks[0].Passed || checks[0].Reason == "" {
		t.Fatalf("review must fail and explain why: inc=%+v checks=%+v", inc, checks)
	}

	// Release the capacity, then re-review: incident closes without a second acknowledgement.
	plant.Reflux.Release(lease)
	inc, checks, err = plant.ReviewIncident(inc.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if inc.Open || inc.Status != StatusResolved || !checks[0].Passed {
		t.Fatalf("incident must resolve once reflux capacity is restored: inc=%+v checks=%+v", inc, checks)
	}
}

func TestFeedSwitchIncidentReviewFlow(t *testing.T) {
	plant := NewPlant(t.TempDir())
	if _, err := plant.Start(context.Background(), "baseline", "primary"); err != nil {
		t.Fatal(err)
	}

	// Cancelled transition fails while isolating the old feed source.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := plant.Transition(cancelled, "premium", "secondary"); err == nil {
		t.Fatal("cancelled feed switch must fail")
	}
	inc := findOpenIncident(t, plant, KindFeedSwitch)
	if inc.Operations[0].Params["context"] != "transition" || inc.Operations[0].Params["source"] != "secondary" {
		t.Fatalf("feed incident must snapshot switch context: %+v", inc.Operations[0].Params)
	}
	if inc, _ = plant.AcknowledgeIncident(inc.ID, "ops", "retrying"); inc.Status != StatusMitigated {
		t.Fatal(inc.Status)
	}
	if inc, checks, _ := plant.ReviewIncident(inc.ID, "ops"); !inc.Open || checks[0].Passed {
		t.Fatalf("feed review must fail while the switch is incomplete: %+v %+v", inc, checks)
	}

	// Complete the switch with a healthy context: old source isolated, new source active and bound.
	if _, err := plant.Transition(context.Background(), "premium", "secondary"); err != nil {
		t.Fatal(err)
	}
	inc, checks, err := plant.ReviewIncident(inc.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if inc.Open || !checks[0].Passed {
		t.Fatalf("feed review must pass after a successful switch: inc=%+v checks=%+v", inc, checks)
	}
}

func TestShutdownIncidentReviewFlow(t *testing.T) {
	plant := NewPlant(t.TempDir())
	if _, err := plant.Start(context.Background(), "baseline", "primary"); err != nil {
		t.Fatal(err)
	}

	if _, err := plant.Shutdown(context.Background(), true); err == nil {
		t.Fatal("relief timeout must fail the shutdown")
	}
	open := plant.Incidents.List(true)
	if len(open) != 1 || open[0].Kind != KindShutdown {
		t.Fatalf("failed shutdown must open exactly one incident (no double reporting), got %+v", open)
	}
	if len(open[0].Operations) != 1 || open[0].Operations[0].Name != opNameReliefShutdown {
		t.Fatalf("only the relief operation should be blocked: %+v", open[0].Operations)
	}

	inc, _ := plant.AcknowledgeIncident(open[0].ID, "ops", "reset valve")
	if inc, checks, _ := plant.ReviewIncident(inc.ID, "ops"); !inc.Open || checks[0].Passed {
		t.Fatalf("latched relief timeout must keep the incident open: %+v %+v", inc, checks)
	}

	// A successful shutdown re-runs both relief and steam closure at a new epoch.
	result, err := plant.Shutdown(context.Background(), false)
	if err != nil || !result.Safe() {
		t.Fatalf("second shutdown must succeed safely: %+v %v", result, err)
	}
	inc, checks, err := plant.ReviewIncident(inc.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if inc.Open || inc.Status != StatusResolved {
		t.Fatalf("shutdown incident must resolve: %+v", inc)
	}
	if len(checks) != 1 || !checks[0].Passed {
		t.Fatalf("relief check must pass: %+v", checks)
	}
}

func TestShutdownIncidentLinksBothBlockedOperations(t *testing.T) {
	// When both relief and steam fail, both operations must be reviewable separately.
	registry := NewIncidents()
	inc := registry.Trigger(KindShutdown, uuid.NewString(), "shutdown failed", []BlockedOperationInput{
		{Name: opNameReliefShutdown, Class: OpSafetyShutdown, Message: "relief timeout"},
		{Name: opNameSteamClose, Class: OpSafetyShutdown, Message: "steam error"},
	})
	if inc, _ = registry.Acknowledge(inc.ID, "ops", ""); inc.Status != StatusMitigated {
		t.Fatal(inc.Status)
	}
	inc, err := registry.RecordReview(inc.ID, "ops", []CheckResult{
		{Operation: opNameReliefShutdown, Class: OpSafetyShutdown, Passed: true},
		{Operation: opNameSteamClose, Class: OpSafetyShutdown, Passed: false, Reason: "steam not isolated"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inc.Open {
		t.Fatal("one failing safety check must keep the incident open")
	}
}

func addValidSamples(t *testing.T, plant *Plant) {
	t.Helper()
	for _, at := range []int64{1000, 1010, 1020} {
		if _, err := plant.AddSample(0.95, at); err != nil {
			t.Fatalf("sample %d rejected: %v", at, err)
		}
	}
}

func TestProductRouteReviewUsesFrozenRevision(t *testing.T) {
	plant := NewPlant(t.TempDir())
	if _, err := plant.Start(context.Background(), "baseline", "primary"); err != nil {
		t.Fatal(err)
	}

	// Routing without composition evidence opens a product-route incident on revision A.
	if err := plant.RouteProduct("tank-1"); err == nil {
		t.Fatal("routing without evidence must fail")
	}
	inc := findOpenIncident(t, plant, KindProductRoute)
	revisionA := inc.Revision
	if inc.Operations[0].Params["target"] != "tank-1" {
		t.Fatalf("product incident must link the blocked target: %+v", inc.Operations[0].Params)
	}

	// Batch moves on to revision B; the incident still reviews evidence scoped to revision A.
	if _, err := plant.Transition(context.Background(), "premium", "secondary"); err != nil {
		t.Fatal(err)
	}
	inc, _ = plant.AcknowledgeIncident(inc.ID, "ops", "routing restored")
	if !plant.RevisionStale(inc) {
		t.Fatal("review must flag that the batch revision has moved on")
	}
	if inc.Revision != revisionA || inc.Revision == plant.Campaign.Snapshot().Revision.String() {
		t.Fatalf("review must stay scoped to frozen revision %s", revisionA)
	}
	inc, checks, err := plant.ReviewIncident(inc.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if !inc.Open || checks[0].Passed {
		t.Fatalf("revision A never routed successfully: current-revision evidence must not close it: %+v %+v", inc, checks)
	}
}

func TestProductRouteReviewPassesAfterSuccessfulRoute(t *testing.T) {
	plant := NewPlant(t.TempDir())
	if _, err := plant.Start(context.Background(), "baseline", "primary"); err != nil {
		t.Fatal(err)
	}
	addValidSamples(t, plant)
	if err := plant.RouteProduct("tank-1"); err != nil {
		t.Fatalf("routing with complete evidence must succeed: %v", err)
	}

	inc := plant.reportIncident(KindProductRoute, "route alarm", plant.Campaign.Snapshot().Revision.String(),
		BlockedOperationInput{Name: opNameProductRoute, Class: OpProductRoute, Message: "route alarm", Params: map[string]string{"target": "tank-1"}})
	inc, _ = plant.AcknowledgeIncident(inc.ID, "ops", "verified route")
	inc, checks, err := plant.ReviewIncident(inc.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if inc.Open || len(checks) != 1 || !checks[0].Passed {
		t.Fatalf("routed revision with a valid interlock permit must resolve: inc=%+v checks=%+v", inc, checks)
	}
}

func TestOperationalAuditIncidentProbeStillResolves(t *testing.T) {
	plant := NewPlant(t.TempDir())
	audit, ok := plant.Status()["operational_audit"].(map[string]any)
	if !ok {
		t.Fatal("operational audit missing from status")
	}
	serviceSection, ok := audit["service"].(map[string]any)
	if !ok {
		t.Fatal("service section missing from operational audit")
	}
	if serviceSection["incident_resolved"] != true {
		t.Fatalf("operational audit incident probe must resolve, got %v", serviceSection["incident_resolved"])
	}
}
