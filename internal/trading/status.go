package trading

import "time"

type SafeLevel string

const (
	SafeLevelSafe   SafeLevel = "SAFE"
	SafeLevelWarn   SafeLevel = "WARN"
	SafeLevelDanger SafeLevel = "DANGER"
)

type OperatorStatusSnapshot struct {
	Safe    SafeLevel
	Profit  string
	State   State
	Blocked string
	Rounds  int
}

func (t *Trader) OperatorStatusSnapshot(now time.Time) OperatorStatusSnapshot {
	_ = now

	t.mu.Lock()
	defer t.mu.Unlock()

	status := OperatorStatusSnapshot{
		Safe:    SafeLevelSafe,
		Profit:  "total:flat",
		State:   t.state,
		Blocked: "none",
		Rounds:  t.completedRounds,
	}

	if t.position != nil || t.openFlow != nil {
		status.Profit = "round:open total:flat"
	}

	switch t.state {
	case StateManualIntervention:
		status.Safe = SafeLevelDanger
		status.Blocked = "manual_intervention"
	case StatePlacingMaker, StateWaitingFill:
		status.Blocked = "maker_wait_fill"
	case StateHedging:
		status.Blocked = "hedge_wait_fill"
	case StatePositionOpen:
		status.Blocked = "position_open"
	case StateClosing:
		status.Blocked = "close_wait_confirm"
	case StateCooldown:
		status.Blocked = "cooldown"
	}

	return status
}
