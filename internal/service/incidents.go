package service

import (
	"errors"
	"github.com/google/uuid"
	"sort"
	"sync"
	"time"
)

// IncidentStatus 是事故的生命周期状态。
type IncidentStatus string

const (
	StatusOpen      IncidentStatus = "open"
	StatusMitigated IncidentStatus = "mitigated"
	StatusResolved  IncidentStatus = "resolved"
)

// OperationClass 把受阻操作按故障域归类，复核时按类选择检查项。
// 字符串取值与事故 kind 常量一一对应。
type OperationClass string

const (
	OpFeedSwitch     OperationClass = "feed-switch"
	OpReboilerRamp   OperationClass = "reboiler-ramp"
	OpProductRoute   OperationClass = "product-route"
	OpRefluxLease    OperationClass = "reflux-lease"
	OpSafetyShutdown OperationClass = "safety-shutdown"
	OpJournal        OperationClass = "journal"
)

// 事故故障域 kind（替代原先不加区分的 "runtime"）。
const (
	KindFeedSwitch     = string(OpFeedSwitch)
	KindReboilerRamp   = string(OpReboilerRamp)
	KindProductRoute   = string(OpProductRoute)
	KindRefluxCapacity = "reflux-capacity"
	KindShutdown       = "shutdown"
	KindJournal        = string(OpJournal)
)

// 关联/结案流程的哨兵错误。
var (
	ErrIncidentNotFound        = errors.New("incident not found")
	ErrIncidentResolved        = errors.New("incident already resolved")
	ErrIncidentNotAcknowledged = errors.New("incident is not acknowledged")
)

// Occurrence 记录事故的每一次触发，重复告警关联到同一事故，但触发记录永不合并。
type Occurrence struct {
	ID         uuid.UUID `json:"id"`
	At         time.Time `json:"at"`
	Message    string    `json:"message"`
	Operations []string  `json:"operations"`
}

// BlockedOperation 按操作名聚合受阻操作及其触发次数，Params 快照触发时的上下文。
type BlockedOperation struct {
	Name        string            `json:"name"`
	Class       OperationClass    `json:"class"`
	Count       int               `json:"count"`
	FirstAt     time.Time         `json:"first_at"`
	LastAt      time.Time         `json:"last_at"`
	LastMessage string            `json:"last_message"`
	Params      map[string]string `json:"params,omitempty"`
}

// Mitigation 记录运维的手动解决确认。
type Mitigation struct {
	Operator string    `json:"operator"`
	Note     string    `json:"note"`
	At       time.Time `json:"at"`
}

// CheckResult 是单个故障域复核检查的结果。
type CheckResult struct {
	Operation string         `json:"operation"`
	Class     OperationClass `json:"class"`
	Passed    bool           `json:"passed"`
	Reason    string         `json:"reason"`
}

// Review 记录一次系统复核。
type Review struct {
	Reviewer string        `json:"reviewer"`
	At       time.Time     `json:"at"`
	Passed   bool          `json:"passed"`
	Checks   []CheckResult `json:"checks"`
}

// Incident 关联触发它的批次修订（Revision，冻结）与受阻操作。
type Incident struct {
	ID          uuid.UUID          `json:"id"`
	Kind        string             `json:"kind"`
	Message     string             `json:"message"`
	Revision    string             `json:"revision"`
	Status      IncidentStatus     `json:"status"`
	Open        bool               `json:"open"`
	FirstAt     time.Time          `json:"first_at"`
	LastAt      time.Time          `json:"last_at"`
	LastMessage string             `json:"last_message"`
	Occurrences []Occurrence       `json:"occurrences"`
	Operations  []BlockedOperation `json:"operations"`
	Mitigations []Mitigation       `json:"mitigations"`
	Reviews     []Review           `json:"reviews"`
	ResolvedAt  *time.Time         `json:"resolved_at,omitempty"`
}

// BlockedOperationInput 是登记一次受阻操作的输入。
type BlockedOperationInput struct {
	Name    string
	Class   OperationClass
	Message string
	Params  map[string]string
}

type Incidents struct {
	mu    sync.Mutex
	items map[uuid.UUID]Incident
}

func NewIncidents() *Incidents { return &Incidents{items: make(map[uuid.UUID]Incident)} }

// Trigger 登记一次事故触发：同 kind+revision 的未结事故关联为同一条，否则新建。
func (s *Incidents) Trigger(kind, revision, message string, ops []BlockedOperationInput) Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if existing, ok := s.findOpen(kind, revision); ok {
		existing.LastAt = now
		existing.LastMessage = message
		existing.Occurrences = append(existing.Occurrences, newOccurrence(now, message, ops))
		existing.Operations = mergeOperations(existing.Operations, ops, now)
		if existing.Status == StatusMitigated {
			existing.Status = StatusOpen
		}
		s.items[existing.ID] = existing
		return existing
	}
	item := Incident{
		ID:          uuid.New(),
		Kind:        kind,
		Message:     message,
		Revision:    revision,
		Status:      StatusOpen,
		Open:        true,
		FirstAt:     now,
		LastAt:      now,
		LastMessage: message,
		Occurrences: []Occurrence{newOccurrence(now, message, ops)},
		Operations:  newOperations(ops, now),
		Mitigations: []Mitigation{},
		Reviews:     []Review{},
	}
	s.items[item.ID] = item
	return item
}

func newOccurrence(at time.Time, message string, ops []BlockedOperationInput) Occurrence {
	names := make([]string, 0, len(ops))
	for _, op := range ops {
		names = append(names, op.Name)
	}
	return Occurrence{ID: uuid.New(), At: at, Message: message, Operations: names}
}

func newOperations(ops []BlockedOperationInput, at time.Time) []BlockedOperation {
	out := make([]BlockedOperation, 0, len(ops))
	for _, op := range ops {
		out = append(out, BlockedOperation{
			Name:        op.Name,
			Class:       op.Class,
			Count:       1,
			FirstAt:     at,
			LastAt:      at,
			LastMessage: op.Message,
			Params:      cloneParams(op.Params),
		})
	}
	return out
}

func mergeOperations(existing []BlockedOperation, ops []BlockedOperationInput, at time.Time) []BlockedOperation {
	index := make(map[string]int, len(existing))
	for i := range existing {
		index[existing[i].Name] = i
	}
	for _, op := range ops {
		if i, ok := index[op.Name]; ok {
			existing[i].Count++
			existing[i].LastAt = at
			existing[i].LastMessage = op.Message
			existing[i].Params = mergeParams(existing[i].Params, op.Params)
			continue
		}
		index[op.Name] = len(existing)
		existing = append(existing, BlockedOperation{
			Name:        op.Name,
			Class:       op.Class,
			Count:       1,
			FirstAt:     at,
			LastAt:      at,
			LastMessage: op.Message,
			Params:      cloneParams(op.Params),
		})
	}
	sort.SliceStable(existing, func(i, j int) bool { return existing[i].FirstAt.Before(existing[j].FirstAt) })
	return existing
}

func cloneParams(params map[string]string) map[string]string {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]string, len(params))
	for key, value := range params {
		out[key] = value
	}
	return out
}

func mergeParams(existing, incoming map[string]string) map[string]string {
	if len(incoming) == 0 {
		return existing
	}
	if existing == nil {
		existing = make(map[string]string, len(incoming))
	}
	for key, value := range incoming {
		existing[key] = value
	}
	return existing
}

// findOpen 必须在持锁状态下调用。
func (s *Incidents) findOpen(kind, revision string) (Incident, bool) {
	for _, item := range s.items {
		if item.Kind == kind && item.Revision == revision && item.Status != StatusResolved {
			return item, true
		}
	}
	return Incident{}, false
}

func (s *Incidents) Get(id uuid.UUID) (Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	return item, ok
}

// Acknowledge 记录运维手动解决：open → mitigated。
func (s *Incidents) Acknowledge(id uuid.UUID, operator, note string) (Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok {
		return Incident{}, ErrIncidentNotFound
	}
	if item.Status == StatusResolved {
		return Incident{}, ErrIncidentResolved
	}
	item.Status = StatusMitigated
	item.Mitigations = append(item.Mitigations, Mitigation{Operator: operator, Note: note, At: time.Now()})
	s.items[id] = item
	return item, nil
}

// RecordReview 记录一次系统复核：全部检查通过才结案，否则保持未结。
// 必须在 Acknowledge 之后调用。
func (s *Incidents) RecordReview(id uuid.UUID, reviewer string, checks []CheckResult) (Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok {
		return Incident{}, ErrIncidentNotFound
	}
	if item.Status == StatusResolved {
		return Incident{}, ErrIncidentResolved
	}
	if item.Status != StatusMitigated {
		return Incident{}, ErrIncidentNotAcknowledged
	}
	passed := true
	for _, check := range checks {
		if !check.Passed {
			passed = false
			break
		}
	}
	review := Review{Reviewer: reviewer, At: time.Now(), Passed: passed, Checks: append([]CheckResult(nil), checks...)}
	item.Reviews = append(item.Reviews, review)
	if passed {
		item.Status = StatusResolved
		item.Open = false
		resolvedAt := review.At
		item.ResolvedAt = &resolvedAt
	}
	s.items[id] = item
	return item, nil
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
	sort.SliceStable(out, func(i, j int) bool { return out[i].FirstAt.Before(out[j].FirstAt) })
	return out
}

func (s *Incidents) CountOpen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, item := range s.items {
		if item.Open {
			count++
		}
	}
	return count
}
