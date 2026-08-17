package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Citation struct{ memoryID, channel, source, date string }
type citationJSON struct {
	MemoryID string `json:"memory_id"`
	Channel  string `json:"channel"`
	Source   string `json:"source"`
	Date     string `json:"date"`
}

func NewCitation(memoryID, channel, source, date string) (Citation, error) {
	c := Citation{strings.TrimSpace(memoryID), strings.TrimSpace(channel), strings.TrimSpace(source), strings.TrimSpace(date)}
	if err := c.Validate(); err != nil {
		return Citation{}, err
	}
	return c, nil
}
func (c Citation) MemoryID() string { return c.memoryID }
func (c Citation) Channel() string  { return c.channel }
func (c Citation) Source() string   { return c.source }
func (c Citation) Date() string     { return c.date }
func (c Citation) MarshalJSON() ([]byte, error) {
	return json.Marshal(citationJSON{c.memoryID, c.channel, c.source, c.date})
}
func (c *Citation) UnmarshalJSON(b []byte) error {
	var raw citationJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	parsed, err := NewCitation(raw.MemoryID, raw.Channel, raw.Source, raw.Date)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}
func (c Citation) Validate() error {
	if c.memoryID == "" {
		return errors.New("missing memory_id")
	}
	if c.channel == "" {
		return errors.New("missing channel")
	}
	if c.source == "" {
		return errors.New("missing source")
	}
	if c.date == "" {
		return errors.New("missing date")
	}
	if _, err := time.Parse(time.RFC3339, c.date); err != nil {
		return fmt.Errorf("invalid date %q: %w", c.date, err)
	}
	return nil
}
