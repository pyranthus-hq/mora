package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

const NodeBytes = 512

type Node struct {
	Start int    `json:"start"`
	Count int    `json:"count"`
	Text  string `json:"text"`
	Date  string `json:"date,omitempty"`
}
type View struct {
	Session     string `json:"session"`
	Text        string `json:"text"`
	Nodes       []Node `json:"nodes"`
	SummaryKind string `json:"summary_kind"`
}
type Zoom struct {
	Session string   `json:"session"`
	Start   int      `json:"start"`
	Count   int      `json:"count"`
	Message *Message `json:"message,omitempty"`
	Nodes   []Node   `json:"nodes,omitempty"`
}

// preview is explicitly extractive, not an assertion that omitted material is
// irrelevant. Exact-message FTS is independent of these lossy navigation nodes.
func preview(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	head := (limit - 7) * 2 / 3
	tail := limit - 7 - head
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	start := len(text) - tail
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[:head] + " [...] " + text[start:]
}
func key(start, count int) string { return fmt.Sprintf("%d+%d", start, count) }
func buildNodes(messages []Message) map[string]Node {
	nodes := map[string]Node{}
	for i, m := range messages {
		nodes[key(i, 1)] = Node{i, 1, preview(m.Kind+": "+m.Text, NodeBytes), m.Date}
	}
	for count := 2; count <= len(messages); count *= 2 {
		for start := 0; start+count <= len(messages); start += count {
			a := nodes[key(start, count/2)]
			b := nodes[key(start+count/2, count/2)]
			nodes[key(start, count)] = Node{start, count, preview(a.Text+"; "+b.Text, NodeBytes), a.Date}
		}
	}
	return nodes
}

// nodes reads the persistent navigation cache when complete; corruption or a
// missing cache falls back to deterministic reconstruction without a read-side write.
func (s Store) nodes(a archive) map[string]Node {
	b, err := os.ReadFile(s.path(a.Session.ID) + ".tree")
	var nodes map[string]Node
	if err == nil && json.Unmarshal(b, &nodes) == nil {
		expected := 0
		valid := true
		for count := 1; count <= len(a.Messages); count *= 2 {
			for start := 0; start+count <= len(a.Messages); start += count {
				expected++
				n, ok := nodes[key(start, count)]
				if !ok || n.Start != start || n.Count != count || len(n.Text) > NodeBytes || !utf8.ValidString(n.Text) {
					valid = false
				}
			}
		}
		if valid && len(nodes) == expected {
			return nodes
		}
	}
	return buildNodes(a.Messages)
}
func render(nodes []Node) string {
	var b strings.Builder
	for _, n := range nodes {
		fmt.Fprintf(&b, "%d+%d|%s\n", n.Start, n.Count, n.Text)
	}
	return b.String()
}
func (s Store) View(id string, budget int) (View, error) {
	v := View{Session: id, SummaryKind: "lossy-extractive-preview"}
	if budget < 1024 || budget > 128000 {
		return v, errors.New("conversation view budget must be 1024..128000 bytes")
	}
	a, err := s.load(id)
	if err != nil {
		return v, err
	}
	all := s.nodes(a)
	// Replay append+fit deterministically: never split a merged view node. This
	// keeps old prefixes stable while detail near the end fades with age.
	for i := range a.Messages {
		v.Nodes = append(v.Nodes, all[key(i, 1)])
		for len(render(v.Nodes)) > budget {
			best := -1
			due := -1.0
			for j := 0; j+1 < len(v.Nodes); j++ {
				x, y := v.Nodes[j], v.Nodes[j+1]
				if x.Count != y.Count || x.Start%(2*x.Count) != 0 || y.Start != x.Start+x.Count {
					continue
				}
				if _, ok := all[key(x.Start, x.Count*2)]; !ok {
					continue
				}
				d := float64(i+1-x.Start) / float64(x.Count*4)
				if d > due {
					due = d
					best = j
				}
			}
			if best < 0 {
				break
			}
			x := v.Nodes[best]
			v.Nodes[best] = all[key(x.Start, x.Count*2)]
			v.Nodes = append(v.Nodes[:best+1], v.Nodes[best+2:]...)
		}
	}
	v.Text = render(v.Nodes)
	if len(v.Text) > budget {
		// A dyadic cover of an odd-length history can have no remaining sibling
		// pair. Keep every range and reduce only the displayed preview resolution.
		overhead := 0
		for _, n := range v.Nodes {
			overhead += len(fmt.Sprintf("%d+%d|\n", n.Start, n.Count))
		}
		perNode := (budget - overhead) / len(v.Nodes)
		if perNode < 32 {
			return v, errors.New("budget too small for complete tree coverage; increase max_bytes")
		}
		for i := range v.Nodes {
			v.Nodes[i].Text = preview(v.Nodes[i].Text, perNode)
		}
		v.Text = render(v.Nodes)
	}
	return v, nil
}
func (s Store) Zoom(id string, start, count int) (Zoom, error) {
	z := Zoom{Session: id, Start: start, Count: count}
	if start < 0 || count < 1 || count&(count-1) != 0 || start%count != 0 {
		return z, errors.New("invalid conversation node: require aligned power-of-two range")
	}
	a, err := s.load(id)
	if err != nil {
		return z, err
	}
	if start > len(a.Messages) || count > len(a.Messages)-start {
		return z, errors.New("conversation node outside archive")
	}
	if count == 1 {
		z.Message = &a.Messages[start]
		return z, nil
	}
	all := s.nodes(a)
	z.Nodes = []Node{all[key(start, count/2)], all[key(start+count/2, count/2)]}
	return z, nil
}
