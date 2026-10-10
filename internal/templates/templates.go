// Package templates holds the localized alert texts. The relay builds every alert itself from
// event + language, so callers can never put free text on a user's lock screen.
package templates

import (
	_ "embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed tipsarr.yaml
var tipsarrYAML []byte

// Builtin returns the embedded template set for an app id, or nil.
func Builtin(app string) []byte {
	if app == "tipsarr" {
		return tipsarrYAML
	}
	return nil
}

type Text struct {
	Title string `yaml:"title"`
	Body  string `yaml:"body"`
}

type Event struct {
	Category string          `yaml:"category"`
	Texts    map[string]Text `yaml:",inline"`
}

type Set struct {
	Events map[string]Event `yaml:"events"`
}

func Parse(data []byte) (*Set, error) {
	var s Set
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	if len(s.Events) == 0 {
		return nil, fmt.Errorf("templates: no events")
	}
	for name, e := range s.Events {
		if e.Category == "" {
			return nil, fmt.Errorf("templates: event %s has no category", name)
		}
		if t, ok := e.Texts["en"]; !ok || t.Title == "" {
			return nil, fmt.Errorf("templates: event %s needs an en title", name)
		}
	}
	return &s, nil
}

// Lookup returns the text and category for an event. Unknown languages fall back to English.
func (s *Set) Lookup(event, lang string) (Text, string, bool) {
	e, ok := s.Events[event]
	if !ok {
		return Text{}, "", false
	}
	lang = strings.ToLower(lang)
	if len(lang) > 2 {
		lang = lang[:2]
	}
	if t, ok := e.Texts[lang]; ok && t.Title != "" {
		return t, e.Category, true
	}
	return e.Texts["en"], e.Category, true
}
