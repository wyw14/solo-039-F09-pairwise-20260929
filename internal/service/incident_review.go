package service

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/wyw14/solo-039/internal/feed"
	"github.com/wyw14/solo-039/internal/journal"
	"github.com/wyw14/solo-039/internal/model"
	"github.com/wyw14/solo-039/internal/relief"
	"strings"
)

// ResolveIncident 由操作员在手动处置后请求结案。系统不会直接结案，
// 而是按事故受阻操作复核对应的联锁与设备结果；任一复核失败，事故保持未结。
func (p *Plant) ResolveIncident(id uuid.UUID) (Incident, error) {
	incident, ok := p.Incidents.Get(id)
	if !ok {
		return Incident{}, errors.New("incident not found")
	}
	if !incident.Open {
		return incident, nil
	}
	checks := p.reviewIncident(incident)
	updated, passed := p.Incidents.ResolveWithReview(id, checks)
	if !passed {
		return updated, fmt.Errorf("incident %s review failed: %s", id, failedChecks(checks))
	}
	return updated, nil
}

func failedChecks(checks []ReviewCheck) string {
	failed := make([]string, 0)
	for _, check := range checks {
		if !check.Passed {
			failed = append(failed, check.Name+" ("+check.Detail+")")
		}
	}
	return strings.Join(failed, "; ")
}

func check(name string, passed bool, detail string) ReviewCheck {
	return ReviewCheck{Name: name, Passed: passed, Detail: detail}
}

// reviewIncident 依据事故记录的批次修订与受阻操作，检查受影响设备是否真正恢复。
func (p *Plant) reviewIncident(inc Incident) []ReviewCheck {
	limits := model.DefaultLimits()
	columnSnap := p.Column.Snapshot()
	switch inc.Operation.Kind {
	case OpFeedSwitch:
		return p.reviewFeed(inc, columnSnap)
	case OpProductRoute:
		return p.reviewProductRoute(inc, columnSnap)
	case OpShutdown:
		return p.reviewShutdown(inc)
	case OpRefluxLease:
		return p.reviewReflux(inc, limits, columnSnap)
	default:
		return p.reviewRuntime(limits)
	}
}

// reviewFeed 复核进料：新源已带流量投用；若发生过切换，旧源必须隔离且切换证据完整；
// 同时塔须绑定到新进料修订且状态安全。
func (p *Plant) reviewFeed(inc Incident, columnSnap model.Column) []ReviewCheck {
	current := p.Feed.Current
	previous := p.Feed.Previous
	checks := []ReviewCheck{
		check("feed-source-active", current != nil && current.Open && current.Flow > 0, feedSource(current)),
	}
	if previous != nil && previous.Source != "" {
		checks = append(checks,
			check("feed-old-source-isolated", previous.Isolated(), previous.Source),
			check("feed-switch-proof", feed.BuildSwitchProof(previous, current).Safe(), feedSource(current)),
		)
	}
	checks = append(checks,
		check("feed-column-bound", current != nil && columnSnap.FeedRevision == current.Revision, "column="+columnSnap.ID),
		check("column-safe", p.Column.Safe(), fmt.Sprintf("pressure=%.2f level=%.2f", columnSnap.Pressure, columnSnap.Level)),
	)
	return checks
}

func feedSource(r *feed.Route) string {
	if r == nil {
		return "no active feed"
	}
	return fmt.Sprintf("source=%s open=%t flow=%.2f", r.Source, r.Open, r.Flow)
}

// reviewProductRoute 复核产品路由：路由对触发事故的修订处于打开状态、有持久证据，且当前联锁仍放行。
func (p *Plant) reviewProductRoute(inc Incident, columnSnap model.Column) []ReviewCheck {
	route := p.Product.Route
	revision := inc.Revision
	opened := route.Opened() && route.Revision() == revision
	durableErr := journal.RequireProof(p.Journal, revision)

	current := p.Campaign.Snapshot()
	proofs := p.Evidence.Independent(revision)
	pressureProof := model.Proof{
		Revision:    current.Revision,
		Source:      "column-pressure",
		Digest:      fmt.Sprintf("%.3f", columnSnap.Pressure),
		Durable:     true,
		Independent: true,
	}
	permit := p.Interlock.Evaluate(append(proofs, pressureProof), model.Campaign{
		ID: current.ID, Revision: current.Revision, Phase: current.Phase, Epoch: current.Epoch, Fencing: current.Fencing,
	})

	return []ReviewCheck{
		check("product-route-open", route.Opened(), route.Target()),
		check("product-route-revision", opened, "route="+route.Revision()+" incident="+revision),
		check("product-durable-proof", durableErr == nil, auditText(durableErr)),
		check("product-interlock-permit", permit.Allowed, permitSummary(permit.Reasons)),
		check("column-safe", p.Column.Safe(), fmt.Sprintf("pressure=%.2f level=%.2f", columnSnap.Pressure, columnSnap.Level)),
	}
}

// reviewShutdown 复核安全停机：泄放已完成且无超时、蒸汽阀在当前纪元确认关闭、装置已停且热源切除。
func (p *Plant) reviewShutdown(inc Incident) []ReviewCheck {
	current := p.Campaign.Snapshot()
	reliefSnap := p.Relief.State.Snapshot()
	boiler := p.Reboiler.Snapshot()
	evaluation := relief.Evaluate(reliefSnap, model.DefaultLimits().MaxPressure)
	return []ReviewCheck{
		check("relief-released", p.Relief.Safe(), fmt.Sprintf("released=%t timed_out=%t", reliefSnap.Released, reliefSnap.TimedOut)),
		check("relief-pressure-safe", evaluation.Safe(), evaluation.Message),
		check("steam-closure-confirmed", p.ReboilerCommands.Confirmed("steam-valve", "closed"), fmt.Sprintf("epoch=%d", current.Epoch)),
		check("campaign-stopped", current.Phase == model.Stopped, string(current.Phase)),
		check("heat-removed", !model.PhaseAllowsHeat(current.Phase) && boiler.Closed && boiler.Steam == 0, fmt.Sprintf("steam=%.2f closed=%t", boiler.Steam, boiler.Closed)),
	}
}

// reviewReflux 复核受阻的回流容量请求：所需容量现已可用，且能量与塔状态仍在运行边界内。
func (p *Plant) reviewReflux(inc Incident, limits model.OperatingLimits, columnSnap model.Column) []ReviewCheck {
	refluxSnap := p.RefluxState.Snapshot()
	boiler := p.Reboiler.Snapshot()
	return []ReviewCheck{
		check("reflux-capacity-available", p.Reflux.Available() >= inc.Operation.Amount, fmt.Sprintf("available=%.2f requested=%.2f", p.Reflux.Available(), inc.Operation.Amount)),
		check("energy-within-limits", limits.ValidateEnergy(boiler, refluxSnap) == nil, auditText(limits.ValidateEnergy(boiler, refluxSnap))),
		check("column-safe", p.Column.Safe(), fmt.Sprintf("pressure=%.2f level=%.2f", columnSnap.Pressure, columnSnap.Level)),
	}
}

// reviewRuntime 复核通用运行事故：塔与能量均回到运行边界内。
func (p *Plant) reviewRuntime(limits model.OperatingLimits) []ReviewCheck {
	snapshot := p.Snapshot()
	return []ReviewCheck{
		check("column-within-limits", limits.ValidateColumn(snapshot.Column) == nil, auditText(limits.ValidateColumn(snapshot.Column))),
		check("energy-within-limits", limits.ValidateEnergy(snapshot.Reboiler, snapshot.Reflux) == nil, auditText(limits.ValidateEnergy(snapshot.Reboiler, snapshot.Reflux))),
		check("column-safe", p.Column.Safe(), fmt.Sprintf("pressure=%.2f level=%.2f", snapshot.Column.Pressure, snapshot.Column.Level)),
	}
}

func auditText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func permitSummary(reasons []string) string {
	if len(reasons) == 0 {
		return "allowed"
	}
	return strings.Join(reasons, "; ")
}
