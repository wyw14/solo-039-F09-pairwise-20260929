package service

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/wyw14/solo-039/internal/feed"
	"github.com/wyw14/solo-039/internal/journal"
	"github.com/wyw14/solo-039/internal/model"
	"github.com/wyw14/solo-039/internal/product"
	"github.com/wyw14/solo-039/internal/relief"
	"github.com/wyw14/solo-039/internal/utility"
	"strconv"
	"strings"
	"time"
)

// 受阻操作名（BlockedOperation.Name 与复核检查的 Operation 字段共用）。
const (
	opNameFeedSwitch           = "feed.switch"
	opNameReboilerRamp         = "reboiler.ramp"
	opNameProductRoute         = "product.route"
	opNameRefluxLease          = "reflux.lease"
	opNameReliefShutdown       = "relief.shutdown"
	opNameSteamClose           = "steam.close"
	opNameJournalPersist       = "journal.persist"
	opNameFeedTransitionRecord = "feed.transition.record"
)

// AcknowledgeIncident 记录运维对手动解决的确认；必须先确认，系统复核才允许执行。
func (p *Plant) AcknowledgeIncident(id uuid.UUID, operator, note string) (Incident, error) {
	if strings.TrimSpace(operator) == "" {
		return Incident{}, errors.New("operator required")
	}
	inc, err := p.Incidents.Acknowledge(id, operator, note)
	if err != nil {
		return Incident{}, err
	}
	p.recordIncidentEvent("incident.acknowledged", inc, nil)
	return inc, nil
}

// ReviewIncident 执行结案前复核：按受阻操作的故障域逐项复核联锁与设备结果。
// 任一检查不过，事故保持未结（返回 updated 与 checks，error 为 nil）。
func (p *Plant) ReviewIncident(id uuid.UUID, reviewer string) (Incident, []CheckResult, error) {
	if strings.TrimSpace(reviewer) == "" {
		reviewer = "system"
	}
	inc, ok := p.Incidents.Get(id)
	if !ok {
		return Incident{}, nil, ErrIncidentNotFound
	}
	if inc.Status == StatusResolved {
		return inc, nil, ErrIncidentResolved
	}
	if inc.Status != StatusMitigated {
		return inc, nil, ErrIncidentNotAcknowledged
	}
	checks := p.incidentChecks(inc)
	updated, err := p.Incidents.RecordReview(id, reviewer, checks)
	if err != nil {
		return inc, checks, err
	}
	passed := updated.Status == StatusResolved
	p.recordIncidentEvent("incident.reviewed", updated, map[string]string{
		"passed": strconv.FormatBool(passed),
		"checks": strconv.Itoa(len(checks)),
	})
	if passed {
		p.recordIncidentEvent("incident.resolved", updated, nil)
	}
	return updated, checks, nil
}

// RevisionStale 报告事故冻结的修订是否已不是当前批次修订（仅警示，不阻断复核）。
func (p *Plant) RevisionStale(inc Incident) bool {
	return p.Campaign.Snapshot().Revision.String() != inc.Revision
}

// incidentChecks 按受阻操作出现顺序逐类构造检查；同一类只检查一次，
// safety-shutdown 例外：泄压与蒸汽切断分别出检查项。
func (p *Plant) incidentChecks(inc Incident) []CheckResult {
	seen := make(map[OperationClass]bool)
	checks := make([]CheckResult, 0)
	for _, op := range inc.Operations {
		if op.Class == OpSafetyShutdown {
			switch op.Name {
			case opNameReliefShutdown:
				checks = append(checks, p.checkReliefShutdown(op))
			case opNameSteamClose:
				checks = append(checks, p.checkSteamClose(op))
			default:
				checks = append(checks, CheckResult{Operation: op.Name, Class: op.Class, Passed: false, Reason: "unknown safety shutdown operation"})
			}
			continue
		}
		if seen[op.Class] {
			continue
		}
		seen[op.Class] = true
		switch op.Class {
		case OpFeedSwitch:
			checks = append(checks, p.checkFeedSwitch(inc, op))
		case OpReboilerRamp:
			checks = append(checks, p.checkReboilerRamp(op))
		case OpProductRoute:
			checks = append(checks, p.checkProductRoute(inc, op))
		case OpRefluxLease:
			checks = append(checks, p.checkRefluxLease(op))
		case OpJournal:
			checks = append(checks, p.checkJournal(op))
		default:
			checks = append(checks, CheckResult{Operation: op.Name, Class: op.Class, Passed: false, Reason: "unknown incident operation class"})
		}
	}
	return checks
}

// checkFeedSwitch 复核进料切换：旧源隔离、新源激活、塔绑定到新进料修订。
func (p *Plant) checkFeedSwitch(inc Incident, op BlockedOperation) CheckResult {
	result := CheckResult{Operation: op.Name, Class: op.Class}
	current := p.Feed.Current
	if current == nil || !current.Open || current.Source == "" {
		result.Reason = "current feed source is not active"
		return result
	}
	if wanted := op.Params["source"]; wanted != "" && current.Source != wanted {
		result.Reason = fmt.Sprintf("feed source is %q, wanted %q", current.Source, wanted)
		return result
	}
	if p.Column.Snapshot().FeedRevision != current.Revision {
		result.Reason = "column is not bound to the active feed revision"
		return result
	}
	// 首次启动没有旧源；换料过渡必须证明旧源已隔离且修订确实推进。
	if op.Params["context"] != "start" && p.Feed.Previous != nil && p.Feed.Previous.Revision != uuid.Nil {
		if err := feed.BuildSwitchProof(p.Feed.Previous, current).Validate(); err != nil {
			result.Reason = err.Error()
			return result
		}
	}
	result.Passed = true
	return result
}

// checkReboilerRamp 复核再沸器 ramp 的设备结果（联锁评估要求 OnSpec，不能用于预热阶段）。
func (p *Plant) checkReboilerRamp(op BlockedOperation) CheckResult {
	result := CheckResult{Operation: op.Name, Class: op.Class}
	reasons := make([]string, 0)
	if !p.Column.Safe() {
		reasons = append(reasons, "column outside safe operating window")
	}
	boiler := p.Reboiler.Snapshot()
	if boiler.Closed || boiler.Steam <= 0 {
		reasons = append(reasons, fmt.Sprintf("reboiler steam not established (steam=%.2f, closed=%t)", boiler.Steam, boiler.Closed))
	}
	if len(reasons) > 0 {
		result.Reason = strings.Join(reasons, "; ")
		return result
	}
	result.Passed = true
	return result
}

// checkProductRoute 按事故冻结修订复核产品路由结果与路由联锁。
func (p *Plant) checkProductRoute(inc Incident, op BlockedOperation) CheckResult {
	result := CheckResult{Operation: op.Name, Class: op.Class}
	revision, err := uuid.Parse(inc.Revision)
	if err != nil {
		result.Reason = "incident revision is invalid"
		return result
	}
	var opened *product.Decision
	for _, decision := range p.ProductAudit.ForRevision(inc.Revision) {
		decision := decision
		if decision.Opened {
			opened = &decision
		}
	}
	if opened == nil {
		result.Reason = "no opened product route decision for incident revision"
		return result
	}
	columnState := p.Column.Snapshot()
	pressureProof := model.Proof{ID: uuid.New(), Revision: revision, Source: "column-pressure", Digest: fmt.Sprintf("%.3f", columnState.Pressure), Durable: true, Independent: true, At: time.Now().Unix()}
	campaignNow := p.Campaign.Snapshot()
	permit := p.Interlock.Evaluate([]model.Proof{opened.Proof, pressureProof}, model.Campaign{ID: opened.Plan.Campaign, Revision: revision, Phase: model.OnSpec, Epoch: campaignNow.Epoch, Fencing: campaignNow.Fencing})
	if err := permit.Error(); err != nil {
		result.Reason = "route interlock denied: " + err.Error()
		return result
	}
	target := p.Product.Route.Target()
	if !p.Product.Route.Opened() || target != opened.Plan.Target {
		result.Reason = fmt.Sprintf("product route is %q, wanted %q", target, opened.Plan.Target)
		return result
	}
	result.Passed = true
	return result
}

// checkRefluxLease 复核回流容量：容量已释放可重试，或受阻租约本身已成为有效租约。
func (p *Plant) checkRefluxLease(op BlockedOperation) CheckResult {
	result := CheckResult{Operation: op.Name, Class: op.Class}
	capacity := p.RefluxState.Snapshot().PumpCapacity
	used := p.Reflux.Used()
	if used < capacity {
		result.Passed = true
		return result
	}
	if owner := op.Params["owner"]; owner != "" {
		if lease, ok := p.Leases.Get(owner); ok && utility.ValidLease(lease, p.Campaign.Snapshot().Fencing) {
			result.Passed = true
			return result
		}
	}
	result.Reason = fmt.Sprintf("reflux capacity exhausted: %.2f of %.2f in use", used, capacity)
	return result
}

// checkReliefShutdown 复核安全停机的泄压路径（TimedOut 锁存，只有重新成功停机才能复位）。
func (p *Plant) checkReliefShutdown(op BlockedOperation) CheckResult {
	result := CheckResult{Operation: op.Name, Class: OpSafetyShutdown}
	state := p.Relief.State.Snapshot()
	if p.Relief.Safe() {
		result.Passed = true
		return result
	}
	result.Reason = relief.Evaluate(state, state.Pressure).Error().Error()
	return result
}

// checkSteamClose 复核蒸汽切断的设备结果与命令代际闭合。
// 注意：成功停机末尾 Campaign.Stop() 会让批次 epoch +1，因此闭合判定必须用
// reboiler 快照自身的 CommandEpoch，而不是复核时的批次 epoch。
func (p *Plant) checkSteamClose(op BlockedOperation) CheckResult {
	result := CheckResult{Operation: op.Name, Class: OpSafetyShutdown}
	boiler := p.Reboiler.Snapshot()
	reasons := make([]string, 0)
	if !p.ReboilerCommands.Confirmed("steam-valve", "closed") {
		reasons = append(reasons, "steam-valve close command not acknowledged")
	}
	if !boiler.Closed || boiler.Steam != 0 {
		reasons = append(reasons, fmt.Sprintf("steam not isolated (steam=%.2f, closed=%t)", boiler.Steam, boiler.Closed))
	}
	if !p.Reboiler.ClosureConfirmed(boiler.CommandEpoch) {
		reasons = append(reasons, "steam closure generation mismatch")
	}
	if len(reasons) > 0 {
		result.Reason = strings.Join(reasons, "; ")
		return result
	}
	result.Passed = true
	return result
}

// checkJournal 复核事件链序列与持久化文件。
func (p *Plant) checkJournal(op BlockedOperation) CheckResult {
	result := CheckResult{Operation: op.Name, Class: op.Class}
	reasons := make([]string, 0)
	if err := journal.ValidateSequence(p.Journal.Events()); err != nil {
		reasons = append(reasons, err.Error())
	}
	if _, err := p.FileLog.Read(); err != nil {
		reasons = append(reasons, "event log unreadable: "+err.Error())
	}
	if len(reasons) > 0 {
		result.Reason = strings.Join(reasons, "; ")
		return result
	}
	result.Passed = true
	return result
}

// recordIncidentEvent 把事故生命周期写入 journal；失败只记 lastError，绝不再开事故（防递归）。
func (p *Plant) recordIncidentEvent(kind string, inc Incident, extra map[string]string) {
	parts := []string{
		"incident=" + inc.ID.String(),
		"kind=" + inc.Kind,
		"revision=" + inc.Revision,
		"status=" + string(inc.Status),
		"occurrences=" + strconv.Itoa(len(inc.Occurrences)),
	}
	for key, value := range extra {
		parts = append(parts, key+"="+value)
	}
	if err := p.record(kind, strings.Join(parts, " ")); err != nil {
		p.noteError(err)
	}
}
