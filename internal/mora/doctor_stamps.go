package mora

import (
	"context"
	"sort"
)

// stampCoverage is content-free health accounting for the disposable activity
// projection. It intentionally reports provider/account and derivation basis,
// never titles, senders, timestamps, or memory identifiers.
type stampCoverage struct {
	Provider          string `json:"provider"`
	Account           string `json:"account,omitempty"`
	Total             int    `json:"total"`
	Stamped           int    `json:"stamped"`
	EventAt           int    `json:"event_at"`
	Participation     int    `json:"participation"`
	Automated         int    `json:"automated"`
	UnknownAutomation int    `json:"unknown_automation"`
	Basis             string `json:"basis,omitempty"`
}

func doctorStampCoverage(ctx context.Context, cfg Config) []stampCoverage {
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		return []stampCoverage{}
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT m.provider,m.account,COUNT(*),
  SUM(CASE WHEN s.memory_id IS NOT NULL THEN 1 ELSE 0 END),
  SUM(CASE WHEN s.event_at_unix IS NOT NULL THEN 1 ELSE 0 END),
  SUM(CASE WHEN s.participation_json IS NOT NULL THEN 1 ELSE 0 END),
  SUM(CASE WHEN s.automated IS NOT NULL THEN 1 ELSE 0 END),
  SUM(CASE WHEN s.automated IS NULL THEN 1 ELSE 0 END),COALESCE(s.automation_basis,'')
 FROM memories m LEFT JOIN activity_stamps s ON s.memory_id=m.id AND s.scope=m.scope
 WHERE m.provider IN ('gmail','imessage','whatsapp','calendar','applecalendar')
 GROUP BY m.provider,m.account,COALESCE(s.automation_basis,'')`)
	if err != nil {
		return []stampCoverage{}
	}
	defer rows.Close()
	var out []stampCoverage
	for rows.Next() {
		var c stampCoverage
		if err := rows.Scan(&c.Provider, &c.Account, &c.Total, &c.Stamped, &c.EventAt, &c.Participation, &c.Automated, &c.UnknownAutomation, &c.Basis); err == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Basis < out[j].Basis
	})
	if out == nil {
		return []stampCoverage{}
	}
	return out
}
