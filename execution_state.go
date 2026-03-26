package main

type ExecutionState string

const (
	StateIdle               ExecutionState = "idle"
	StatePlacingMaker       ExecutionState = "placing_maker"
	StateWaitingFill        ExecutionState = "waiting_fill"
	StateHedging            ExecutionState = "hedging"
	StatePositionOpen       ExecutionState = "position_open"
	StateClosing            ExecutionState = "closing"
	StateCooldown           ExecutionState = "cooldown"
	StateManualIntervention ExecutionState = "manual_intervention"
)

// CanAcceptSignal returns whether the trader may begin a new round from the given state.
func CanAcceptSignal(state ExecutionState) bool {
	return state == StateIdle
}

// IsTerminalRoundBlocker reports whether the state represents an in-flight, unresolved,
// or order-placement round that must block the next round until the round is cleared.
func IsTerminalRoundBlocker(state ExecutionState) bool {
	switch state {
	case StatePlacingMaker, StateWaitingFill, StateHedging, StatePositionOpen, StateClosing, StateManualIntervention:
		return true
	default:
		return false
	}
}
