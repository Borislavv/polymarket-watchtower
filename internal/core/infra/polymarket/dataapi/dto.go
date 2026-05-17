package dataapi

// dataTrade is the subset of /trades fields we consume.
// Polymarket Data API returns timestamps as unix seconds and amounts as
// either numbers or stringified numbers depending on the field; we accept both
// shapes via the flex helpers in client.go.
type dataTrade struct {
	TransactionHash string  `json:"transactionHash"`
	Market          string  `json:"market"` // condition id
	Asset           string  `json:"asset"`  // token id
	Side            string  `json:"side"`   // BUY/SELL
	Size            float64 `json:"size"`
	Price           float64 `json:"price"`
	Timestamp       int64   `json:"timestamp"`
	Maker           string  `json:"maker"`
	Taker           string  `json:"taker"`
	ID              string  `json:"id"`
}
