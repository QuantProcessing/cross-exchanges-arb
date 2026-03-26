package main

import "testing"

func TestSignalFilter_RejectsWhenTraderNotIdle(t *testing.T) {
	ok := IsExecutableSignal(StateWaitingFill, DefaultExecutionProfile(), &SpreadSignal{})
	if ok {
		t.Fatal("expected signal rejection while waiting_fill")
	}
}

func TestSignalFilter_AcceptsIdleSignal(t *testing.T) {
	ok := IsExecutableSignal(StateIdle, DefaultExecutionProfile(), &SpreadSignal{})
	if !ok {
		t.Fatal("expected idle signal to be executable")
	}
}
