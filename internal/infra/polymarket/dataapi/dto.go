package dataapi

// dataTrade is the subset of Polymarket Data API /trades fields we consume.
// Timestamps are unix seconds; size/price are floats.
//
// NOTE: the response field is "conditionId", not "market". The query parameter
// is "market=<conditionId>". Keeping the JSON tag aligned with the wire format
// — the client overwrites Trade.Market with the requested condition id anyway,
// so this is mainly defensive for future callers that read the field directly.
type dataTrade struct {
	TransactionHash string  `json:"transactionHash"`
	ConditionID     string  `json:"conditionId"`
	Asset           string  `json:"asset"` // token id
	Side            string  `json:"side"`  // BUY/SELL
	Size            float64 `json:"size"`
	Price           float64 `json:"price"`
	Timestamp       int64   `json:"timestamp"`
	ProxyWallet     string  `json:"proxyWallet"`
	ID              string  `json:"id"`
}
