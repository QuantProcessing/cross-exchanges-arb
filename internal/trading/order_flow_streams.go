package trading

import (
	exchanges "github.com/QuantProcessing/exchanges"
	"github.com/QuantProcessing/exchanges/account"
)

type orderFlowWithFills interface {
	Fills() <-chan *exchanges.Fill
}

func orderFlowOrders(flow *account.OrderFlow) <-chan *exchanges.Order {
	if flow == nil {
		return nil
	}
	return flow.C()
}

func orderFlowFills(flow *account.OrderFlow) <-chan *exchanges.Fill {
	if flow == nil {
		return nil
	}
	withFills, ok := any(flow).(orderFlowWithFills)
	if !ok {
		return nil
	}
	return withFills.Fills()
}
