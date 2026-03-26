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

func TestCanAcceptSignal_CoversAllExecutionStates(t *testing.T) {
	tests := []struct {
		name  string
		state ExecutionState
		want  bool
	}{
		{name: "idle", state: StateIdle, want: true},
		{name: "placing_maker", state: StatePlacingMaker, want: false},
		{name: "waiting_fill", state: StateWaitingFill, want: false},
		{name: "hedging", state: StateHedging, want: false},
		{name: "position_open", state: StatePositionOpen, want: false},
		{name: "closing", state: StateClosing, want: false},
		{name: "cooldown", state: StateCooldown, want: false},
		{name: "manual_intervention", state: StateManualIntervention, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanAcceptSignal(tt.state); got != tt.want {
				t.Fatalf("CanAcceptSignal(%s) = %t, want %t", tt.state, got, tt.want)
			}
		})
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
