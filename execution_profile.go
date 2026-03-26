package main

import (
	"time"

	exchanges "github.com/QuantProcessing/exchanges"
)

type ExecutionProfile struct {
	LiveValidation      bool
	EntryMakerOrderType exchanges.OrderType
	HedgeUsesSlippage   bool
	MakerTimeout        time.Duration
	MaxRounds           int
}

func DefaultExecutionProfile() ExecutionProfile {
	return ExecutionProfile{
		LiveValidation:      true,
		EntryMakerOrderType: exchanges.OrderTypePostOnly,
		HedgeUsesSlippage:   true,
		MakerTimeout:        15 * time.Second,
		MaxRounds:           1,
	}
}
