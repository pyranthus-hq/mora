package memory

import "encoding/json"

// NullableBool distinguishes an omitted projection from a projected unknown.
// A nil *NullableBool is omitted; &NullableBool{} serializes as explicit null.
type NullableBool struct{ Value *bool }

func (v NullableBool) MarshalJSON() ([]byte, error)     { return json.Marshal(v.Value) }
func (v *NullableBool) UnmarshalJSON(data []byte) error { return json.Unmarshal(data, &v.Value) }
