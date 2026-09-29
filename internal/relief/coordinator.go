package relief

import (
	"context"
	"errors"
	"github.com/wyw14/solo-039/internal/model"
)

type Coordinator struct{ State *State }

type ShutdownResult struct {
	ReliefError   error
	SteamError    error
	Depressurized bool
	SteamClosed   bool
}

func NewCoordinator() *Coordinator { return &Coordinator{State: New()} }

func (c *Coordinator) Shutdown(ctx context.Context, cpg model.Campaign, valve string, timeout bool) error {
	c.State.Begin(valve)
	if timeout {
		c.State.Timeout()
		return errors.New("relief valve timeout")
	}
	c.State.Release()
	return nil
}

func (c *Coordinator) Safe() bool { return c.State.Snapshot().Released && !c.State.Snapshot().TimedOut }

func CombineShutdown(reliefErr, steamErr error, reliefState model.Relief, steamClosed bool) ShutdownResult {
	return ShutdownResult{ReliefError: reliefErr, SteamError: steamErr, Depressurized: reliefErr == nil && reliefState.Released && !reliefState.TimedOut, SteamClosed: steamErr == nil && steamClosed}
}

func (r ShutdownResult) Error() error {
	if r.ReliefError != nil {
		return r.ReliefError
	}
	return r.SteamError
}

func (r ShutdownResult) Safe() bool { return r.Depressurized && r.SteamClosed && r.Error() == nil }

// Failure 用简短名称标识停机中未恢复的环节，供事故复核引用。
func (r ShutdownResult) Failure() string {
	switch {
	case r.ReliefError != nil:
		return "relief"
	case r.SteamError != nil:
		return "steam"
	case !r.Depressurized:
		return "depressurization"
	case !r.SteamClosed:
		return "steam-closure"
	default:
		return ""
	}
}
