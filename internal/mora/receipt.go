package mora

import (
	"encoding/json"
	"fmt"
	"io"
)

// receiptEnvelope describes the two versioned keys every receipt carries.
// schema_version is a MAJOR-only integer: it changes on field removal or
// retyping, never when fields are added, matching the indexSchemaVersion idiom.
type receiptEnvelope struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
}

// emitReceipt merges the envelope with the payload rather than wrapping it so
// existing payload fields remain at their published top-level locations.
func emitReceipt(w io.Writer, schema string, version int, payload any) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(payloadJSON, &fields); err != nil {
		return fmt.Errorf("receipt payload must marshal as an object: %w", err)
	}
	envelopeJSON, err := json.Marshal(receiptEnvelope{Schema: schema, SchemaVersion: version})
	if err != nil {
		return err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(envelopeJSON, &envelope); err != nil {
		return err
	}
	for key, value := range envelope {
		fields[key] = value
	}
	b, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
