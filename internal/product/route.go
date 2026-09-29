package product

import (
	"errors"
	"github.com/wyw14/solo-039/internal/model"
)

type Route struct {
	target   string
	open     bool
	revision string
}

func (r *Route) Open(plan model.ProductPlan) error {
	if plan.Target == "" {
		return errors.New("target required")
	}
	r.target = plan.Target
	r.revision = plan.Revision.String()
	r.open = true
	return nil
}

func (r *Route) Close() { r.open = false }

func (r *Route) Opened() bool { return r.open }

func (r *Route) Target() string { return r.target }

func (r *Route) Revision() string { return r.revision }
