package eventpage

import "encoding/json"

func unmarshalJSON(data []byte, v any) error { return json.Unmarshal(data, v) }

func marshalJSON(v any) ([]byte, error) { return json.Marshal(v) }
