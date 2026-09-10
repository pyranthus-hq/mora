package readable

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEmailSetsAsideQuotedHistorySignatureAndImageMarkup(t *testing.T) {
	raw := "Hi Sam,\r\n\r\nI hope you are doing well.\r\n\r\nThanks for the email. I received the approval. Working on the offer. We will release it later today or early tomorrow.\r\n\r\n\r\nBest Regards,\r\n\r\n[A0JjnJsIF8QYAAAAAElFTkSuQmCC]<https://www.example.test/>\r\n  Priya Example | Coordinator - Example Team\r\n  priya@example.test\r\n  www.example.test<http://www.example.test/>   | Example team signature\r\n  Example Services\r\n\r\n\r\n________________________________\r\nFrom: sam@example.test <sam@example.test>\r\nSent: Wednesday, September 9, 2026 5:40 PM\r\nTo: Priya Example <priya@example.test>\r\nSubject: Re: Next Steps\r\n\r\nHi Priya,\r\n\r\nI wanted to confirm there's nothing else you need from me."
	r := Email(raw)
	want := "Hi Sam,\n\nI hope you are doing well.\n\nThanks for the email. I received the approval. Working on the offer. We will release it later today or early tomorrow.\n\nBest Regards,\n\nPriya Example"
	if r.Text != want {
		t.Fatalf("text=%q", r.Text)
	}
	if !reflect.DeepEqual(r.Omitted, []string{OmittedQuotedHistory, OmittedImageMarkup, OmittedSignature}) {
		t.Fatalf("omitted=%v", r.Omitted)
	}
	if r.Changed != true {
		t.Fatal("expected change flag")
	}
}

func TestEmailKeepsNegationUncertaintyAndQuotationsInBody(t *testing.T) {
	raw := "The contract is not signed.\nWe might release it tomorrow, but that is unconfirmed.\nShe wrote: \"do not send the invoice yet\".\n\nThanks,\nMaria"
	r := Email(raw)
	if r.Text != raw || r.Changed || len(r.Omitted) != 0 {
		t.Fatalf("clean text altered: %+v", r)
	}
}

func TestEmailCutsWrappedGmailQuoteAndAngleQuotes(t *testing.T) {
	raw := "That time works for me.\n\nOn Tue, Sep 9, 2026 at 5:40 PM Priya Example <priya@example.test>\nwrote:\n\n> Can you meet tomorrow?\n> Thanks"
	r := Email(raw)
	if r.Text != "That time works for me." || !reflect.DeepEqual(r.Omitted, []string{OmittedQuotedHistory}) {
		t.Fatalf("%+v", r)
	}
	forwarded := "See below.\n\n---------- Forwarded message ---------\nFrom: X <x@example.test>\nSubject: Offer\n\nThe offer is attached."
	r = Email(forwarded)
	if !strings.Contains(r.Text, "The offer is attached.") {
		t.Fatalf("forwarded content is substance, not history: %q", r.Text)
	}
}

func TestEmailRemovesHiddenPreheaderPaddingAndTrackingLines(t *testing.T) {
	raw := "They won’t see your feedback until they write one too\r\n ͏  ͏  ͏  ͏ ­ ­ ­\r\n%opentrack%\r\n[image: Google]\r\nCheck activity\r\n<https://accounts.example.test/alert?x=1>\r\nTo make changes go to your account\r\n<https://accounts.example.test/x>"
	r := Email(raw)
	want := "They won’t see your feedback until they write one too\n\nCheck activity\n\nTo make changes go to your account"
	if r.Text != want {
		t.Fatalf("text=%q", r.Text)
	}
	if !reflect.DeepEqual(r.Omitted, []string{OmittedImageMarkup, OmittedHiddenCharacters, OmittedTrackingMarkup}) {
		t.Fatalf("omitted=%v", r.Omitted)
	}
}

func TestEmailNeverReturnsEmptyOrTruncatesContentBeforeClosing(t *testing.T) {
	if r := Email("[cid:image001.png@01DC]"); r.Text != "[cid:image001.png@01DC]" || r.Changed {
		t.Fatalf("empty readable must fall back to original: %+v", r)
	}
	if r := Email("   \r\n\r\n  "); r.Text != "" || r.Changed {
		t.Fatalf("%+v", r)
	}
	// A closing on the first line is a greeting, not a signature.
	raw := "Thanks,\nCould you send the invoice? It is not approved yet.\n\nBest,\nAdit"
	if r := Email(raw); r.Text != raw {
		t.Fatalf("%q", r.Text)
	}
	// Standard signature delimiter (exactly "-- ") and mobile footers.
	raw = "Running late, not cancelled.\n\nSent from my iPhone\n-- \nAdit Karode\n555-0100"
	if r := Email(raw); r.Text != "Running late, not cancelled." || !reflect.DeepEqual(r.Omitted, []string{OmittedSignature}) {
		t.Fatalf("%+v", r)
	}
}

func TestBoundKeepsRuneBoundaryAndFlagsTruncation(t *testing.T) {
	text, truncated := Bound("ééé", 2)
	if text != "éé" || !truncated {
		t.Fatal(text, truncated)
	}
	if text, truncated = Bound("ok", 5); text != "ok" || truncated {
		t.Fatal(text, truncated)
	}
}

func TestEmailKeepsSubstanceThatMerelyResemblesMarkupOrSignatures(t *testing.T) {
	cases := map[string]string{
		"bracketed words are not image residue":   "Status: [notapprovedyet] and [image] pending.\n",
		"sentences after a closing are content":   "Update:\nThanks,\nPlease review the draft.\nDo not sign.",
		"body dashes are not the delimiter":       "Options:\n--\nDo not sign yet.",
		"sentence footers stay":                   "Sent from my lawyer: do not sign.",
		"emoji joiners and direction marks stay":  "Family: \U0001F468\u200D\U0001F469\u200D\U0001F467 \u200Fلا\u200E",
		"forwarded content keeps its own history": "FYI.\n\nBegin forwarded message:\nFrom: X <x@example.test>\nDate: Monday\nSubject: Offer\n\nOn Monday X wrote:\n> Not approved.",
	}
	for name, raw := range cases {
		r := Email(raw)
		if r.Changed || len(r.Omitted) != 0 || r.Text != strings.TrimSpace(raw) {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	// Labeled image placeholders are markup wherever they appear.
	if r := Email("Logo: [image: logo.png]\nText continues."); r.Text != "Logo:\nText continues." || !reflect.DeepEqual(r.Omitted, []string{OmittedImageMarkup}) {
		t.Fatalf("%+v", r)
	}
}

func TestEmailFallbackAndWrappedWroteVariants(t *testing.T) {
	if r := Email("\u200d[cid:x]"); r.Text != "\u200d[cid:x]" || r.Changed || r.Omitted != nil {
		t.Fatalf("fallback must return the untouched source: %+v", r)
	}
	r := Email("Confirmed for 3pm.\n\nOn Tue, Sep 9, 2026 at 5:40 PM Alice Example\n<alice@example.test> wrote:\n> earlier")
	if r.Text != "Confirmed for 3pm." || !reflect.DeepEqual(r.Omitted, []string{OmittedQuotedHistory}) {
		t.Fatalf("%+v", r)
	}
	if r := Email("See [CID:logo.png] and <HTTPS://Example.test/x>.\nNot approved."); r.Text != "See  and .\nNot approved." || !reflect.DeepEqual(r.Omitted, []string{OmittedImageMarkup}) {
		t.Fatalf("%+v", r)
	}
}

func TestEmailProjectionGoldenCorpus(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "email-projection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Raw, Text string
		Omitted         []string
	}
	if err := json.Unmarshal(body, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 5 {
		t.Fatal("golden corpus unexpectedly small")
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := Email(tc.Raw)
			if got.Text != tc.Text || len(got.Omitted) != len(tc.Omitted) || (len(tc.Omitted) > 0 && !reflect.DeepEqual(got.Omitted, tc.Omitted)) {
				t.Fatalf("text=%q omitted=%v", got.Text, got.Omitted)
			}
		})
	}
}
