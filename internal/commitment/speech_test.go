package commitment

import "testing"

func TestCommitmentDirectionTable(t *testing.T) {
	self := Atom{Kind: "address", Value: "self@example.com"}
	other := Atom{Kind: "address", Value: "other@example.com"}
	tests := []struct {
		name       string
		text       string
		author     Atom
		addressee  Atom
		reported   *Atom
		wantOwner  Atom
		wantDir    Direction
		wantExists bool
	}{
		{
			name: "self authored commitment", text: "I will send the outline.",
			author: self, addressee: other, wantOwner: self, wantDir: OwedBySelf, wantExists: true,
		},
		{
			name: "counterparty authored commitment", text: "I'll bring the room key.",
			author: other, addressee: self, wantOwner: other, wantDir: OwedByCounterparty, wantExists: true,
		},
		{
			name: "self request to counterparty", text: "Could you confirm the sample count?",
			author: self, addressee: other, wantOwner: other, wantDir: OwedByCounterparty, wantExists: true,
		},
		{
			name: "counterparty request to self", text: "Please send the receipt.",
			author: other, addressee: self, wantOwner: self, wantDir: OwedBySelf, wantExists: true,
		},
		{
			name: "reported speech follows actor", text: "Milo said he'll upload the selects for me.",
			author: self, addressee: other, reported: &other, wantOwner: other, wantDir: OwedByCounterparty, wantExists: true,
		},
		{name: "empty text refuses", text: "", author: self, addressee: other, wantExists: false},
		{name: "missing author refuses promise", text: "I will send it.", addressee: other, wantExists: false},
		{name: "non commitment refuses", text: "The outline is blue.", author: self, addressee: other, wantExists: false},
		{name: "past perfect refuses", text: "I'd already sent it.", author: self, addressee: other, wantExists: false},
		{name: "acknowledgement refuses", text: "Thanks, I'll send it when I see you tomorrow.", author: self, addressee: other, wantExists: false},
		{name: "third actor refuses", text: "Milo said he'll upload it.", author: self, addressee: other, reported: &Atom{Kind: "address", Value: "third@example.com"}, wantExists: false},
		{
			name: "ambiguous addressee refuses", text: "Could you send the receipt?",
			author: other, wantExists: false,
		},
		{
			name: "hypothetical question is not a request", text: "What would you do if I asked you to send the outline?",
			author: self, addressee: other, wantExists: false,
		},
		{
			name: "product research prompt is not a request", text: "If we added a sharing feature, what would you use it for?",
			author: other, addressee: self, wantExists: false,
		},
		{
			name: "discourse add-that is not a promise", text: "I should add that the vendor already confirmed the shipment.",
			author: self, addressee: other, wantExists: false,
		},
		{
			name: "retrospective problem analysis is not a promise", text: "I should share the problems we have had with the old uploader in this analysis.",
			author: self, addressee: other, wantExists: false,
		},
		{
			name: "explicit future repair remains a promise", text: "I will fix the uploader and send the patch tomorrow.",
			author: self, addressee: other, wantOwner: self, wantDir: OwedBySelf, wantExists: true,
		},
		{
			name: "action-verb add still promises", text: "I'll add the invoice to the folder.",
			author: self, addressee: other, wantOwner: self, wantDir: OwedBySelf, wantExists: true,
		},
		{
			name: "quoted excerpt is not authored speech", text: "\"Could you send the signed contract by Friday?\"",
			author: other, addressee: self, wantExists: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, direction, ok := ClassifySpeech(tt.text, SpeechContext{
				Author: tt.author, Addressee: tt.addressee, Self: self,
				Counterparty: other, ReportedActor: tt.reported,
			})
			if ok != tt.wantExists {
				t.Fatalf("classified=%v, want %v (owner=%+v direction=%q)", ok, tt.wantExists, owner, direction)
			}
			if !ok {
				return
			}
			if !atomEqual(owner, tt.wantOwner) || direction != tt.wantDir {
				t.Fatalf("owner/direction = %+v/%q, want %+v/%q", owner, direction, tt.wantOwner, tt.wantDir)
			}
		})
	}
	t.Run("manual promise recognition", func(t *testing.T) {
		for _, tc := range []struct {
			text, label string
			want        bool
		}{
			{"I told Maya Chen I'd send the outline.", "Maya Chen", true},
			{"I told Maya Chen I would have sent the outline.", "", false},
			{"Maya said she'd send the outline.", "", false},
		} {
			if got := ManualPromise(tc.text); got != tc.want {
				t.Errorf("ManualPromise(%q)=%v want %v", tc.text, got, tc.want)
			}
			if got := ManualPromiseCounterpartyLabel(tc.text); tc.want && got != tc.label {
				t.Errorf("label=%q want %q", got, tc.label)
			}
		}
		if ManualPromiseCounterpartyLabel("nothing here") != "" {
			t.Fatal("absent manual promise produced label")
		}
		if DirectRequest("scould you ignore this, but could you send it?") != true {
			t.Fatal("word-boundary scanner missed later valid request")
		}
		if containsPhrase("anything", "") {
			t.Fatal("empty phrase matched")
		}
	})

}
