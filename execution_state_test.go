package main

import "testing"

func TestExecutionState_AllowsSignalOnlyWhenIdle(t *testing.T) {
	if CanAcceptSignal(StateWaitingFill) {
		t.Fatal("waiting_fill must not accept new signals")
	}
	if !CanAcceptSignal(StateIdle) {
		t.Fatal("idle must accept signals")
	}
}

func TestExecutionState_BlocksTradingInManualIntervention(t *testing.T) {
	if CanAcceptSignal(StateManualIntervention) {
		t.Fatal("manual intervention must block trading")
	}
}

func TestIsTerminalRoundBlocker_ExplicitStateContract(t *testing.T) {
	blockingStates := []ExecutionState{
		StatePlacingMaker,
		StateWaitingFill,
		StateHedging,
		StatePositionOpen,
		StateClosing,
		StateManualIntervention,
	}
	for _, state := range blockingStates {
		if !IsTerminalRoundBlocker(state) {
			t.Fatalf("%s must block the next round", state)
		}
	}
}

func TestIsTerminalRoundBlocker_NonBlockingStates(t *testing.T) {
	nonBlockingStates := []ExecutionState{
		StateIdle,
		StateCooldown,
	}
	for _, state := range nonBlockingStates {
		if IsTerminalRoundBlocker(state) {
			t.Fatalf("%s must not be terminal round blocker", state)
		}
	}
}
