package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const (
	reviewMaxNotes = 50
	reviewMaxTitle = 200
	reviewMaxText  = 4000
)

// A leading dash is refused so an id can never read as a flag.
var reviewID = regexp.MustCompile(`^[A-Za-z0-9_.:][A-Za-z0-9_.:-]{0,79}$`)

type ReviewNote struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
	Type  string `json:"type"`
	Title string `json:"title"`
	Text  string `json:"text"`
}
type ReviewList struct {
	Notes []ReviewNote `json:"notes"`
	More  int          `json:"more"`
}

func moraBin() string {
	if bin := os.Getenv("MORA_BIN"); bin != "" {
		return bin
	}
	return filepath.Join(home(), ".local/bin/mora")
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// reviewList prints the agent notes that wait for review, each with its text.
// It never passes mora's own error text on.
func reviewList() error {
	fail := func(msg string) error {
		_ = emit(map[string]string{"error": msg})
		return errors.New(msg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	bin := moraBin()
	b, err := command(ctx, bin, "review", "--list", "--json")
	if err != nil {
		return fail("Mora could not list the agent notes.")
	}
	var raw struct {
		Notes []ReviewNote `json:"notes"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return fail("Mora returned an invalid note list.")
	}
	rows := []ReviewNote{}
	for _, n := range raw.Notes {
		if reviewID.MatchString(n.ID) && (n.Kind == "saved" || n.Kind == "held") {
			rows = append(rows, n)
		}
	}
	out := ReviewList{Notes: rows, More: 0}
	if len(rows) > reviewMaxNotes {
		out.Notes, out.More = rows[:reviewMaxNotes], len(rows)-reviewMaxNotes
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i := range out.Notes {
		wg.Add(1)
		go func(n *ReviewNote) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			n.Text = reviewText(ctx, bin, *n)
			n.Title, n.Text = cut(n.Title, reviewMaxTitle), cut(n.Text, reviewMaxText)
		}(&out.Notes[i])
	}
	wg.Wait()
	return emit(out)
}

// reviewText reads one note's text: a saved note with read, a held one with proposals show.
func reviewText(ctx context.Context, bin string, n ReviewNote) string {
	const missing = "The text could not be read. Use mora review in a terminal for this note."
	if n.Kind == "saved" {
		b, err := command(ctx, bin, "read", n.ID, "--json")
		var m struct {
			Text string `json:"text"`
		}
		if err != nil || json.Unmarshal(b, &m) != nil {
			return missing
		}
		return m.Text
	}
	b, err := command(ctx, bin, "mcp", "proposals", "show", n.ID)
	var m struct {
		Inspection struct {
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		} `json:"inspection"`
	}
	if err != nil || json.Unmarshal(b, &m) != nil {
		return missing
	}
	return m.Inspection.Arguments.Text
}

// reviewActArgs maps one owner action to exactly one mora command. Keep never
// sets owner-confirmed: that stays a terminal action (confirm --mine).
func reviewActArgs(action, kind, id string) ([]string, bool) {
	if !reviewID.MatchString(id) {
		return nil, false
	}
	switch action + " " + kind {
	case "keep saved":
		return []string{"confirm", id}, true
	case "discard saved":
		return []string{"undo", id}, true
	case "keep held":
		return []string{"mcp", "proposals", "approve", id}, true
	case "discard held":
		return []string{"mcp", "proposals", "reject", id}, true
	}
	return nil, false
}

func reviewAct(args []string) error {
	fail := func(msg string) error {
		_ = emit(map[string]any{"ok": false, "error": msg})
		return errors.New(msg)
	}
	if len(args) != 3 {
		return fail("Usage: review-act <keep|discard> <saved|held> <id>.")
	}
	cmd, ok := reviewActArgs(args[0], args[1], args[2])
	if !ok {
		return fail("That action was refused.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := command(ctx, moraBin(), cmd...); err != nil {
		if ctx.Err() != nil {
			return fail("Mora took too long. Refresh to see what changed.")
		}
		return fail("Mora could not complete the action.")
	}
	return emit(map[string]any{"ok": true})
}
