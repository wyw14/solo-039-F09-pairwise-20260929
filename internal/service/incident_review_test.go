package service

import (
	"context"
	"testing"
)

func lastOpenIncident(t *testing.T, p *Plant) Incident {
	t.Helper()
	open := p.Incidents.List(true)
	if len(open) == 0 {
		t.Fatal("expected the failed operation to have opened an incident")
	}
	return open[len(open)-1]
}

func TestShutdownIncidentStaysOpenUntilSafeStateRestored(t *testing.T) {
	p := NewPlant(t.TempDir())
	ctx := context.Background()
	if _, err := p.Start(ctx, "baseline", "primary"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Shutdown(ctx, true); err == nil {
		t.Fatal("relief timeout must fail shutdown")
	}
	incident := lastOpenIncident(t, p)

	// 泄放未恢复时，复核失败，事故保持未结。
	if _, err := p.ResolveIncident(incident.ID); err == nil {
		t.Fatal("review must fail while relief is timed out")
	}
	if stored, _ := p.Incidents.Get(incident.ID); !stored.Open {
		t.Fatal("incident must remain open after a failed review")
	}

	// 操作员手动恢复泄放路径并完成停机收尾。
	p.Relief.State.Begin("flare")
	p.Relief.State.Release()
	p.Campaign.Stop()

	resolved, err := p.ResolveIncident(incident.ID)
	if err != nil {
		t.Fatalf("review must pass once shutdown results are restored: %v", err)
	}
	if resolved.Open {
		t.Fatal("incident should be closed after a successful review")
	}
	final := resolved.Reviews[len(resolved.Reviews)-1]
	if !final.Passed {
		for _, check := range final.Checks {
			if !check.Passed {
				t.Fatalf("final review still reports failure: %s %s", check.Name, check.Detail)
			}
		}
	}
}

func TestRefluxIncidentClearsAfterCapacityReleased(t *testing.T) {
	p := NewPlant(t.TempDir())
	ctx := context.Background()
	if _, err := p.Start(ctx, "baseline", "primary"); err != nil {
		t.Fatal(err)
	}
	lease, err := p.AcquireReflux("T-101", 0.8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AcquireReflux("T-101", 0.5); err == nil {
		t.Fatal("second lease exceeding capacity must be rejected")
	}
	incident := lastOpenIncident(t, p)

	if _, err := p.ResolveIncident(incident.ID); err == nil {
		t.Fatal("review must fail while requested capacity is unavailable")
	}
	stored, _ := p.Incidents.Get(incident.ID)
	if len(stored.Reviews) != 1 || stored.Reviews[0].Passed {
		t.Fatal("the failed review attempt must be retained")
	}

	p.Reflux.Release(lease)
	resolved, err := p.ResolveIncident(incident.ID)
	if err != nil {
		t.Fatalf("review must pass once capacity frees up: %v", err)
	}
	if resolved.Open || len(resolved.Reviews) != 2 {
		t.Fatalf("incident should close and keep both reviews, got open=%t reviews=%d", resolved.Open, len(resolved.Reviews))
	}
}

func TestFeedSwitchIncidentResolvesWithActiveBoundFeed(t *testing.T) {
	p := NewPlant(t.TempDir())
	ctx := context.Background()
	if _, err := p.Start(ctx, "baseline", "primary"); err != nil {
		t.Fatal(err)
	}
	incident := p.Incidents.Report(ReportInput{
		Kind:      "feed",
		Message:   "manual switch blocked",
		Revision:  p.Campaign.Snapshot().Revision.String(),
		Operation: Operation{Kind: OpFeedSwitch, Detail: "primary"},
	})
	resolved, err := p.ResolveIncident(incident.ID)
	if err != nil {
		t.Fatalf("feed review must pass when active feed is bound to the column: %v", err)
	}
	if resolved.Open {
		t.Fatal("incident should be closed")
	}
}

func TestProductRouteIncidentReopensFlowThenResolves(t *testing.T) {
	p := NewPlant(t.TempDir())
	ctx := context.Background()
	if _, err := p.Start(ctx, "baseline", "primary"); err != nil {
		t.Fatal(err)
	}

	// 证据不足时无法路由：两次尝试只应关联同一事故，且每次触发都可追溯。
	if err := p.RouteProduct("tank-A"); err == nil {
		t.Fatal("routing without composition evidence must be rejected")
	}
	if err := p.RouteProduct("tank-A"); err == nil {
		t.Fatal("repeated routing attempt must still be rejected")
	}
	open := p.Incidents.List(true)
	if len(open) != 1 {
		t.Fatalf("repeated alerts must attach to one incident, got %d", len(open))
	}
	incident := open[0]
	if len(incident.Occurrences) != 2 {
		t.Fatalf("each trigger must be traceable, got %d occurrences", len(incident.Occurrences))
	}
	if incident.Operation.Kind != OpProductRoute || incident.Operation.Detail != "tank-A" {
		t.Fatalf("incident must link the blocked product route, got %+v", incident.Operation)
	}

	// 路由未恢复时复核失败、事故保持未结。
	if _, err := p.ResolveIncident(incident.ID); err == nil {
		t.Fatal("review must fail while the route is closed and proof is missing")
	}

	// 操作员补齐证据后成功路由。
	for _, at := range []int64{100, 110, 120} {
		if _, err := p.AddSample(0.95, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.RouteProduct("tank-A"); err != nil {
		t.Fatalf("routing must succeed once evidence is complete: %v", err)
	}

	// 同一事故（同修订同路由）复核通过后结案，历史触发与复核均保留。
	resolved, err := p.ResolveIncident(incident.ID)
	if err != nil {
		t.Fatalf("review must pass once routing and interlock are restored: %v", err)
	}
	if resolved.Open {
		t.Fatal("incident should be closed")
	}
	if len(resolved.Occurrences) != 2 {
		t.Fatal("trigger history must survive resolution")
	}
}
