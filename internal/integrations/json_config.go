package integrations

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func encodeJSON(doc map[string]json.RawMessage) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// upsertJSONEntry sets mcpServers.mora and reports whether anything changed.
func upsertJSONEntry(body []byte, c Client, binary string) ([]byte, bool, error) {
	doc := map[string]json.RawMessage{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, false, err
		}
	}
	if doc == nil {
		return nil, false, fmt.Errorf("expected JSON object")
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, false, err
		}
	}
	if servers == nil {
		return nil, false, fmt.Errorf("expected JSON object")
	}
	mora := map[string]json.RawMessage{}
	if raw, ok := servers["mora"]; ok {
		if err := json.Unmarshal(raw, &mora); err != nil {
			return nil, false, err
		}
	}
	if mora == nil {
		return nil, false, fmt.Errorf("expected JSON object")
	}
	before, _ := json.Marshal(mora)
	if c == Claude {
		mora["type"] = json.RawMessage(`"stdio"`)
		if _, ok := mora["env"]; !ok {
			mora["env"] = json.RawMessage(`{}`)
		}
	}
	mora["command"], _ = json.Marshal(binary)
	mora["args"] = json.RawMessage(`["mcp","serve"]`)
	after, err := json.Marshal(mora)
	if err != nil {
		return nil, false, err
	}
	servers["mora"] = after
	serversRaw, err := encodeJSON(servers)
	if err != nil {
		return nil, false, err
	}
	doc["mcpServers"] = serversRaw
	out, err := encodeJSON(doc)
	if err != nil {
		return nil, false, err
	}
	return out, string(before) != string(after), nil
}

func removeJSONEntry(body []byte) ([]byte, bool, error) {
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false, err
	}
	if doc == nil {
		return nil, false, fmt.Errorf("expected JSON object")
	}
	raw, ok := doc["mcpServers"]
	if !ok {
		return body, false, nil
	}
	servers := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, false, err
	}
	if servers == nil {
		return nil, false, fmt.Errorf("expected JSON object")
	}
	if _, ok := servers["mora"]; !ok {
		return body, false, nil
	}
	delete(servers, "mora")
	serversRaw, err := encodeJSON(servers)
	if err != nil {
		return nil, false, err
	}
	doc["mcpServers"] = serversRaw
	out, err := encodeJSON(doc)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}
