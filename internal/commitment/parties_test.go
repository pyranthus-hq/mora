package commitment

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"reflect"
	"testing"
)

func TestCounterpartyPolicies(t *testing.T) {
	self := map[string]bool{"me@example.com": true, "z@example.com": true}
	if got := CanonicalSelf(self, " Preferred@EXAMPLE.COM "); got.Value != "preferred@example.com" {
		t.Fatalf("preferred=%+v", got)
	}
	if got := CanonicalSelf(self, ""); got.Value != "me@example.com" {
		t.Fatalf("sorted=%+v", got)
	}
	if got := CanonicalSelf(nil, ""); got.Kind != "self" || got.Value != "self" {
		t.Fatalf("fallback=%+v", got)
	}
	gmail := memory.Memory{Provider: "gmail", Meta: map[string]any{"from": []string{"sam@example.com"}, "to": []string{"me@example.com"}, "names": map[string]string{"sam@example.com": "Sam Rivera"}}}
	other, ok := Counterparty(gmail, self)
	if !ok || other.Kind != AtomAddress || other.Value != "sam@example.com" {
		t.Fatalf("gmail=%+v,%v", other, ok)
	}
	if keys := CounterpartyKeys(gmail, other); !reflect.DeepEqual(keys, []string{"address:sam@example.com", "given:sam", "name:sam rivera"}) {
		t.Fatalf("keys=%v", keys)
	}
	amb := gmail
	amb.Meta = map[string]any{"from": []string{"sam@example.com"}, "to": []string{"me@example.com", "bob@example.com"}}
	if _, ok := Counterparty(amb, self); ok {
		t.Fatal("ambiguous gmail accepted")
	}
	alias := gmail
	alias.Meta = map[string]any{"from": []string{"first.last+tag@gmail.com", "firstlast@gmail.com"}, "to": []string{"me@example.com"}}
	if _, ok := Counterparty(alias, self); !ok {
		t.Fatal("gmail mailbox alias split")
	}
	im := memory.Memory{Provider: "imessage", Meta: map[string]any{"participants": []map[string]string{{"handle": "+100", "name": "Me Example"}, {"handle": "+200", "name": "Sam Rivera"}}}}
	imSelf := map[string]bool{"me.example@example.com": true}
	cp, ok := Counterparty(im, imSelf)
	if !ok || cp.Provider != "imessage" || cp.Value != "+200" {
		t.Fatalf("imessage=%+v,%v", cp, ok)
	}
	if keys := CounterpartyKeys(im, cp); !reflect.DeepEqual(keys, []string{"given:sam", "handle:+200", "name:sam rivera"}) {
		t.Fatalf("im keys=%v", keys)
	}
	partial := im
	partial.Meta = map[string]any{"participants": []map[string]string{{"handle": "+100", "name": "Me Other"}, {"handle": "+200", "name": "Sam Rivera"}}}
	if _, ok := Counterparty(partial, imSelf); ok {
		t.Fatal("partial self name excluded")
	}
	if ParticipantNameIsSelf("", map[string]bool{}) || ParticipantNameIsSelf("X", map[string]bool{"x": true}) || ParticipantNameIsSelf("Me Other", map[string]bool{"me": true}) {
		t.Fatal("weak self name accepted")
	}
}
func TestGmailAddresseePolicy(t *testing.T) {
	self := Atom{Kind: AtomAddress, Value: "me@example.com"}
	other := Atom{Kind: AtomAddress, Value: "sam@example.com"}
	if got := GmailAddressee(other, []string{"me@example.com"}, nil, self, other, false); !EqualAtom(got, self) {
		t.Fatalf("inbound=%+v", got)
	}
	if got := GmailAddressee(self, []string{"sam@example.com"}, nil, self, other, false); !EqualAtom(got, other) {
		t.Fatalf("outbound=%+v", got)
	}
	for _, got := range []Atom{GmailAddressee(other, []string{"me@example.com", "bob@example.com"}, nil, self, other, false), GmailAddressee(self, []string{"sam@example.com", "bob@example.com"}, nil, self, other, false), GmailAddressee(Atom{}, nil, nil, self, other, false)} {
		if got.Kind != "" {
			t.Fatalf("ambiguous=%+v", got)
		}
	}
	if metaStrings(make(chan int)) != nil || metaStrings("bad") != nil || participantPairs(make(chan int)) != nil || participantPairs("bad") != nil {
		t.Fatal("malformed metadata accepted")
	}
	badNames := memory.Memory{Provider: "gmail", Meta: map[string]any{}}
	badNames.Meta["names"] = make(chan int)
	_ = CounterpartyKeys(badNames, other)

}
