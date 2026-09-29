package service

import (
	"github.com/google/uuid"
	"sort"
	"sync"
	"time"
)

// 受阻操作类型：事故会阻断的具体工厂操作，复核按类型检查对应联锁与设备。
const (
	OpFeedSwitch   = "feed-switch"
	OpProductRoute = "product-route"
	OpShutdown     = "shutdown"
	OpRefluxLease  = "reflux-lease"
	OpRuntime      = "runtime"
)

// Operation 记录事故阻断的操作及其上下文。
type Operation struct {
	Kind   string
	Detail string
	Amount float64
}

// Occurrence 记录一次告警触发；同一事故的重复触发逐条留存，均可追溯。
type Occurrence struct {
	At      time.Time
	Message string
	Detail  string
}

// ReviewCheck 是结案前对单项联锁/设备结果的复核。
type ReviewCheck struct {
	Name   string
	Passed bool
	Detail string
}

// Review 记录一次结案复核尝试；复核失败也留存，事故保持未结。
type Review struct {
	At     time.Time
	Checks []ReviewCheck
	Passed bool
	Reason string
}

type Incident struct {
	ID          uuid.UUID
	Kind        string
	Message     string
	Revision    string
	Operation   Operation
	Open        bool
	At          time.Time
	UpdatedAt   time.Time
	ResolvedAt  time.Time
	Occurrences []Occurrence
	Reviews     []Review
}

// ReportInput 携带告警上报的完整上下文。
type ReportInput struct {
	Kind      string
	Message   string
	Revision  string
	Operation Operation
}

type Incidents struct {
	mu    sync.Mutex
	items map[uuid.UUID]Incident
}

func NewIncidents() *Incidents { return &Incidents{items: make(map[uuid.UUID]Incident)} }

func defaultOperationKind(kind string) string {
	switch kind {
	case "reflux-capacity":
		return OpRefluxLease
	case "shutdown":
		return OpShutdown
	case "feed":
		return OpFeedSwitch
	case "product-route":
		return OpProductRoute
	default:
		return OpRuntime
	}
}

// Report 打开事故或把重复告警关联到原事件。
// 去重键为 告警类型 + 批次修订 + 受阻操作（含操作目标）；命中已结案事件时重新打开它，
// 无论新建还是关联，本次触发都会追加一条 Occurrence。
func (s *Incidents) Report(input ReportInput) Incident {
	now := time.Now()
	op := input.Operation
	if op.Kind == "" {
		op.Kind = defaultOperationKind(input.Kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.items {
		if !sameIncidentKey(item, input.Kind, input.Revision, op) {
			continue
		}
		item.Occurrences = append(item.Occurrences, Occurrence{At: now, Message: input.Message, Detail: op.Detail})
		item.UpdatedAt = now
		if !item.Open {
			item.Open = true
			item.ResolvedAt = time.Time{}
		}
		s.items[item.ID] = item
		return item
	}
	item := Incident{
		ID:        uuid.New(),
		Kind:      input.Kind,
		Message:   input.Message,
		Revision:  input.Revision,
		Operation: op,
		Open:      true,
		At:        now,
		UpdatedAt: now,
		Occurrences: []Occurrence{{
			At:      now,
			Message: input.Message,
			Detail:  op.Detail,
		}},
	}
	s.items[item.ID] = item
	return item
}

// Open 保留旧入口，按告警类型推导受阻操作。
func (s *Incidents) Open(kind, message, revision string) Incident {
	return s.Report(ReportInput{Kind: kind, Message: message, Revision: revision})
}

func sameIncidentKey(item Incident, kind, revision string, op Operation) bool {
	return item.Kind == kind &&
		item.Revision == revision &&
		item.Operation.Kind == op.Kind &&
		item.Operation.Detail == op.Detail
}

func (s *Incidents) Get(id uuid.UUID) (Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	return item, ok
}

// ResolveWithReview 在结案前登记复核结果：全部检查通过才结案，否则事故保持未结。
// 返回事故最新状态与本次是否完成结案。
func (s *Incidents) ResolveWithReview(id uuid.UUID, checks []ReviewCheck) (Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok {
		return Incident{}, false
	}
	if !item.Open {
		return item, true
	}
	passed := true
	reason := ""
	for _, check := range checks {
		if !check.Passed {
			passed = false
			reason = check.Name + ": " + check.Detail
			break
		}
	}
	now := time.Now()
	review := Review{At: now, Checks: append([]ReviewCheck(nil), checks...), Passed: passed, Reason: reason}
	item.Reviews = append(item.Reviews, review)
	item.UpdatedAt = now
	if passed {
		item.Open = false
		item.ResolvedAt = now
	}
	s.items[id] = item
	return item, passed
}

func (s *Incidents) List(openOnly bool) []Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Incident, 0, len(s.items))
	for _, item := range s.items {
		if openOnly && !item.Open {
			continue
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func (s *Incidents) CountOpen() int { return len(s.List(true)) }
