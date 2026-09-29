package reflux

import (
	"errors"
	"github.com/wyw14/solo-039/internal/model"
	"sync"
)

type Coordinator struct {
	mu       sync.Mutex
	capacity float64
	used     float64
	states   map[string]*State
}

func NewCoordinator(capacity float64) *Coordinator {
	return &Coordinator{capacity: capacity, states: make(map[string]*State)}
}

func (c *Coordinator) Register(name string, state *State) {
	c.mu.Lock()
	c.states[name] = state
	c.mu.Unlock()
}

func (c *Coordinator) Acquire(name string, amount float64, fence uint64) (model.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if amount <= 0 || c.used+amount > c.capacity {
		return model.Lease{}, errors.New("reflux capacity unavailable")
	}
	c.used += amount
	lease := model.Lease{ID: model.NewCampaign(name).ID, Owner: name, Capacity: amount, Fence: fence, Active: true}
	if state := c.states[name]; state != nil {
		state.SetLease(lease)
	}
	return lease, nil
}

func (c *Coordinator) Release(lease model.Lease) {
	c.mu.Lock()
	if lease.Active {
		c.used -= lease.Capacity
	}
	c.mu.Unlock()
}

func (c *Coordinator) Used() float64 { c.mu.Lock(); defer c.mu.Unlock(); return c.used }

func (c *Coordinator) Capacity() float64 { c.mu.Lock(); defer c.mu.Unlock(); return c.capacity }

func (c *Coordinator) Available() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacity - c.used
}
