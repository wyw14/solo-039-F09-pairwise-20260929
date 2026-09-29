package service

import (
	"testing"
)

func TestReportLinksRepeatedAlertToOriginalIncident(t *testing.T) {
	registry := NewIncidents()
	first := registry.Report(ReportInput{Kind: "shutdown", Message: "relief valve timeout", Revision: "rev-1", Operation: Operation{Kind: OpShutdown, Detail: "relief"}})
	second := registry.Report(ReportInput{Kind: "shutdown", Message: "relief valve timeout again", Revision: "rev-1", Operation: Operation{Kind: OpShutdown, Detail: "relief"}})

	if first.ID != second.ID {
		t.Fatalf("repeated alert opened a new incident: first=%s second=%s", first.ID, second.ID)
	}
	stored, ok := registry.Get(first.ID)
	if !ok {
		t.Fatal("incident missing")
	}
	if len(stored.Occurrences) != 2 {
		t.Fatalf("expected 2 traceable occurrences, got %d", len(stored.Occurrences))
	}
	if stored.Occurrences[1].Message != "relief valve timeout again" {
		t.Fatalf("second trigger message not retained: %+v", stored.Occurrences[1])
	}
	if registry.CountOpen() != 1 {
		t.Fatalf("expected a single open incident, got %d", registry.CountOpen())
	}
}

func TestReportSeparatesDistinctRevisionAndOperation(t *testing.T) {
	registry := NewIncidents()
	a := registry.Report(ReportInput{Kind: "feed", Message: "switch failed", Revision: "rev-1", Operation: Operation{Kind: OpFeedSwitch, Detail: "primary"}})
	b := registry.Report(ReportInput{Kind: "feed", Message: "switch failed", Revision: "rev-2", Operation: Operation{Kind: OpFeedSwitch, Detail: "primary"}})
	c := registry.Report(ReportInput{Kind: "feed", Message: "switch failed", Revision: "rev-1", Operation: Operation{Kind: OpFeedSwitch, Detail: "backup"}})

	if a.ID == b.ID || a.ID == c.ID || b.ID == c.ID {
		t.Fatal("distinct revision or operation target must open separate incidents")
	}
	if registry.CountOpen() != 3 {
		t.Fatalf("expected 3 open incidents, got %d", registry.CountOpen())
	}
}

func TestFailedReviewKeepsIncidentOpen(t *testing.T) {
	registry := NewIncidents()
	incident := registry.Report(ReportInput{Kind: "shutdown", Message: "relief valve timeout", Revision: "rev-1", Operation: Operation{Kind: OpShutdown}})

	updated, passed := registry.ResolveWithReview(incident.ID, []ReviewCheck{
		{Name: "relief-released", Passed: false, Detail: "timed out"},
	})
	if passed {
		t.Fatal("a failed review must not resolve the incident")
	}
	if !updated.Open {
		t.Fatal("incident must remain open after failed review")
	}
	if len(updated.Reviews) != 1 || updated.Reviews[0].Passed {
		t.Fatalf("failed review attempt must be recorded: %+v", updated.Reviews)
	}

	// 复核通过后才结案，且保留失败历史。
	updated, passed = registry.ResolveWithReview(incident.ID, []ReviewCheck{
		{Name: "relief-released", Passed: true, Detail: "released"},
	})
	if !passed || updated.Open {
		t.Fatal("incident should close once every check passes")
	}
	if len(updated.Reviews) != 2 {
		t.Fatalf("expected both review attempts retained, got %d", len(updated.Reviews))
	}
	if updated.ResolvedAt.IsZero() {
		t.Fatal("resolved timestamp should be set")
	}
}

func TestAlertOnResolvedIncidentReopensAndStaysTraceable(t *testing.T) {
	registry := NewIncidents()
	incident := registry.Report(ReportInput{Kind: "shutdown", Message: "first", Revision: "rev-1", Operation: Operation{Kind: OpShutdown}})
	registry.ResolveWithReview(incident.ID, []ReviewCheck{{Name: "all", Passed: true}})

	repeated := registry.Report(ReportInput{Kind: "shutdown", Message: "recurrence", Revision: "rev-1", Operation: Operation{Kind: OpShutdown}})
	if repeated.ID != incident.ID {
		t.Fatal("recurring alert should attach to the original incident")
	}
	if !repeated.Open {
		t.Fatal("recurring alert should reopen the incident")
	}
	if len(repeated.Occurrences) != 2 {
		t.Fatalf("expected both triggers traceable, got %d", len(repeated.Occurrences))
	}
}
