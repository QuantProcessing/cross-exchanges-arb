package main

// IsExecutableSignal returns whether a candidate signal may be acted on by the trader.
// The filter is intentionally generic so strategy emission stays separate from execution policy.
func IsExecutableSignal(state ExecutionState, profile ExecutionProfile, sig *SpreadSignal) bool {
	if sig == nil {
		return false
	}
	return CanAcceptSignal(state)
}
