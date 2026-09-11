package integrations

import (
	"encoding/json"
)

func readJSONEntry(body []byte) (*entry, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	raw, ok := doc["mcpServers"]
	if !ok {
		return nil, nil
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, err
	}
	moraRaw, ok := servers["mora"]
	if !ok {
		return nil, nil
	}
	var e struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(moraRaw, &e); err != nil {
		return nil, err
	}
	return &entry{Command: e.Command, Args: e.Args}, nil
}
