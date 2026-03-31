package trading

type State string

const (
	StateIdle               State = "idle"
	StatePlacingMaker       State = "placing_maker"
	StateWaitingFill        State = "waiting_fill"
	StateHedging            State = "hedging"
	StatePositionOpen       State = "position_open"
	StateClosing            State = "closing"
	StateCooldown           State = "cooldown"
	StateManualIntervention State = "manual_intervention"
)

// CanAcceptSignal returns whether the trader may begin a new round from the given state.
// Manual intervention and every unresolved execution state must remain blocked.
func CanAcceptSignal(state State) bool {
	return state == StateIdle
}

// IsOpenFlowState reports whether the trader is inside the maker-open workflow.
func IsOpenFlowState(state State) bool {
	switch state {
	case StatePlacingMaker, StateWaitingFill, StateHedging:
		return true
	default:
		return false
	}
}

// IsTerminalRoundBlocker reports whether the state represents an in-flight, unresolved,
// or order-placement round that must block the next round until the round is cleared.
func IsTerminalRoundBlocker(state State) bool {
	switch state {
	case StatePlacingMaker, StateWaitingFill, StateHedging, StatePositionOpen, StateClosing, StateManualIntervention:
		return true
	default:
		return false
	}
}
