package templates

import "testing"

func TestBuiltinTipsarrParsesAndCoversEveryEvent(t *testing.T) {
	s, err := Parse(Builtin("tipsarr"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"request.approved", "request.declined", "request.failed", "request.available", "request.created", "issue.created", "issue.comment", "issue.resolved"} {
		for _, lang := range []string{"en", "fr"} {
			txt, cat, ok := s.Lookup(ev, lang)
			if !ok || txt.Title == "" || txt.Body == "" || cat == "" {
				t.Errorf("%s/%s incomplete: %v %q %v", ev, lang, txt, cat, ok)
			}
		}
	}
}

func TestLookupFallbackAndRegion(t *testing.T) {
	s, _ := Parse(Builtin("tipsarr"))
	en, _, _ := s.Lookup("request.approved", "en")
	fr, _, _ := s.Lookup("request.approved", "fr")
	if en.Title == fr.Title {
		t.Fatal("en and fr identical")
	}
	if got, _, _ := s.Lookup("request.approved", "FR-ca"); got != fr {
		t.Fatalf("region: %v", got)
	}
	if got, _, _ := s.Lookup("request.approved", "de"); got != en {
		t.Fatalf("fallback: %v", got)
	}
	if _, _, ok := s.Lookup("nope", "en"); ok {
		t.Fatal("unknown event accepted")
	}
}

func TestParseValidation(t *testing.T) {
	bad := []string{
		``,
		"events: {}",
		"events:\n  a: { en: { title: x, body: y } }",                    // no category
		"events:\n  a: { category: c, fr: { title: x, body: y } }",       // no en
		"events:\n  a: { category: c, en: { title: x, body: y } }\nz: 1", // unknown field
	}
	for _, b := range bad {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
}
