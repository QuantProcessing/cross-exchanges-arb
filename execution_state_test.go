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
