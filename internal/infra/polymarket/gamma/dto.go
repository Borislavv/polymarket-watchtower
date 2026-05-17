package gamma

// gammaMarket is the subset of /markets fields we consume. The Gamma response
// is messy (some fields are JSON-encoded strings, e.g. outcomes/clobTokenIds),
// so we keep them as strings and parse in the mapper.
type gammaMarket struct {
	ID               string          `json:"id"`
	Slug             string          `json:"slug"`
	Question         string          `json:"question"`
	ConditionID      string          `json:"conditionId"`
	OutcomesJSON     string          `json:"outcomes"`
	OutcomePricesRaw string          `json:"outcomePrices"`
	ClobTokenIDsRaw  string          `json:"clobTokenIds"`
	Active           bool            `json:"active"`
	Closed           bool            `json:"closed"`
	Archived         bool            `json:"archived"`
	StartDate        string          `json:"startDate"`
	EndDate          string          `json:"endDate"`
	Volume           float64         `json:"volumeNum"`
	Volume24h        float64         `json:"volume24hr"`
	Liquidity        float64         `json:"liquidityNum"`
	Events           []gammaEventRef `json:"events"`
}

// gammaEventRef is the subset of /markets[].events fields we need to build a
// user-facing event URL (https://polymarket.com/event/<slug>). Markets are
// sub-cards inside events; the market slug is NOT a valid page URL.
type gammaEventRef struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

type gammaEvent struct {
	ID      string        `json:"id"`
	Slug    string        `json:"slug"`
	Title   string        `json:"title"`
	Markets []gammaMarket `json:"markets"`
	Tags    []gammaTag    `json:"tags"`
	Active  bool          `json:"active"`
	Closed  bool          `json:"closed"`
}

// gammaTag accepts both stringly-typed (Gamma /tags) and numeric (some
// nested events.tags) id encodings. Slug and label are the usual strings.
type gammaTag struct {
	ID    flexInt64 `json:"id"`
	Slug  string    `json:"slug"`
	Label string    `json:"label"`
}

// flexInt64 decodes either a JSON number or a JSON string of digits into int64.
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' && b[len(b)-1] == '"' {
		b = b[1 : len(b)-1]
	}
	if len(b) == 0 {
		return nil
	}
	var n int64
	var neg bool
	i := 0
	if b[0] == '-' {
		neg = true
		i = 1
	}
	for ; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return &jsonNumberErr{raw: string(b)}
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	*f = flexInt64(n)
	return nil
}

type jsonNumberErr struct{ raw string }

func (e *jsonNumberErr) Error() string { return "gamma: cannot parse int64 from " + e.raw }
