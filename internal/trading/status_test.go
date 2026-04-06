package trading

import (
	"testing"
	"time"
)

func TestBuildOperatorStatus_MapsSafeProfitAndBlocked(t *testing.T) {
	tests := []struct {
		name string
		tr   *Trader
		want OperatorStatusSnapshot
	}{
		{
			name: "idle safe",
			tr: &Trader{
				state:           StateIdle,
				completedRounds: 3,
			},
			want: OperatorStatusSnapshot{
				Safe:    SafeLevelSafe,
				Profit:  "total:flat",
				Blocked: "none",
				State:   StateIdle,
				Rounds:  3,
			},
		},
		{
			name: "manual intervention with no exposure is danger but flat",
			tr: &Trader{
				state: StateManualIntervention,
			},
			want: OperatorStatusSnapshot{
				Safe:    SafeLevelDanger,
				Profit:  "total:flat",
				Blocked: "manual_intervention",
				State:   StateManualIntervention,
			},
		},
		{
			name: "placing maker blocks on maker fill",
			tr: &Trader{
				state:    StatePlacingMaker,
				openFlow: &openFlowState{},
			},
			want: OperatorStatusSnapshot{
				Safe:    SafeLevelSafe,
				Profit:  "round:open total:flat",
				Blocked: "maker_wait_fill",
				State:   StatePlacingMaker,
			},
		},
		{
			name: "waiting fill blocks on maker fill",
			tr: &Trader{
				state:    StateWaitingFill,
				openFlow: &openFlowState{},
			},
			want: OperatorStatusSnapshot{
				Safe:    SafeLevelSafe,
				Profit:  "round:open total:flat",
				Blocked: "maker_wait_fill",
				State:   StateWaitingFill,
			},
		},
		{
			name: "hedging blocks on hedge fill",
			tr: &Trader{
				state:    StateHedging,
				openFlow: &openFlowState{},
			},
			want: OperatorStatusSnapshot{
				Safe:    SafeLevelSafe,
				Profit:  "round:open total:flat",
				Blocked: "hedge_wait_fill",
				State:   StateHedging,
			},
		},
		{
			name: "closing blocks on close confirm",
			tr: &Trader{
				state:    StateClosing,
				position: &ArbPosition{},
			},
			want: OperatorStatusSnapshot{
				Safe:    SafeLevelSafe,
				Profit:  "round:open total:flat",
				Blocked: "close_wait_confirm",
				State:   StateClosing,
			},
		},
		{
			name: "position open remains blocked",
			tr: &Trader{
				state:    StatePositionOpen,
				position: &ArbPosition{},
			},
			want: OperatorStatusSnapshot{
				Safe:    SafeLevelSafe,
				Profit:  "round:open total:flat",
				Blocked: "position_open",
				State:   StatePositionOpen,
			},
		},
		{
			name: "cooldown remains blocked after a round",
			tr: &Trader{
				state:           StateCooldown,
				completedRounds: 4,
			},
			want: OperatorStatusSnapshot{
				Safe:    SafeLevelSafe,
				Profit:  "total:flat",
				Blocked: "cooldown",
				State:   StateCooldown,
				Rounds:  4,
			},
		},
		{
			name: "manual intervention with open flow keeps round open profit",
			tr: &Trader{
				state:    StateManualIntervention,
				openFlow: &openFlowState{},
			},
			want: OperatorStatusSnapshot{
				Safe:    SafeLevelDanger,
				Profit:  "round:open total:flat",
				Blocked: "manual_intervention",
				State:   StateManualIntervention,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.tr.OperatorStatusSnapshot(time.Now())
			if got.Safe != tc.want.Safe || got.Profit != tc.want.Profit || got.Blocked != tc.want.Blocked || got.State != tc.want.State || got.Rounds != tc.want.Rounds {
				t.Fatalf("status = %#v, want %#v", got, tc.want)
			}
		})
	}
}
