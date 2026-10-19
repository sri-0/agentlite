package refs

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genai"
)

type fakeResolver map[string]string

func (f fakeResolver) Resolve(_ context.Context, name string) (Ref, bool, error) {
	content, ok := f[name]
	return Ref{Kind: "skill", ID: name, Content: content}, ok, nil
}

// TestExpand: each reference that resolves, once and in order, ahead of the
// message as typed; anything else stays text.
func TestExpand(t *testing.T) {
	rs := Resolvers{'/': fakeResolver{"a": "A", "b": "B"}}
	var unknown string // maxRefs words looked up in vain
	for i := range maxRefs {
		unknown += fmt.Sprintf("/z%d ", i)
	}
	for _, tc := range []struct {
		name, text string
		parts      []string
		events     int
	}{
		{name: "plain", text: "hello", parts: []string{"hello"}},
		{name: "start and middle", text: "/b then /a", parts: []string{"B", "A", "/b then /a"}, events: 2},
		{name: "once each", text: "/a /a", parts: []string{"A", "/a /a"}, events: 1},
		{name: "unknown, a path, a lone sigil, another sigil", text: "/x /etc/hosts / @a", parts: []string{"/x /etc/hosts / @a"}},
		{name: "past the cap", text: unknown + "/a", parts: []string{unknown + "/a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, events, err := rs.Expand(context.Background(), "turn", tc.text)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, p := range c.Parts {
				got = append(got, p.Text)
			}
			if strings.Join(got, "|") != strings.Join(tc.parts, "|") || len(events) != tc.events {
				t.Errorf("parts %q, %d events; want %q, %d", got, len(events), tc.parts, tc.events)
			}
		})
	}
}

type partResolver struct{}

func (partResolver) Resolve(_ context.Context, name string) (Ref, bool, error) {
	return Ref{Kind: "file", ID: name, Content: "about " + name, Part: genai.NewPartFromBytes([]byte("img"), "image/png")}, true, nil
}

// TestExpandPart: a reference's part follows its content, ahead of the message.
func TestExpandPart(t *testing.T) {
	c, events, err := Resolvers{'#': partResolver{}}.Expand(context.Background(), "turn", "look at #f1")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Parts) != 3 || c.Parts[0].Text != "about f1" || c.Parts[1].InlineData == nil ||
		c.Parts[1].InlineData.MIMEType != "image/png" || c.Parts[2].Text != "look at #f1" || len(events) != 1 {
		t.Errorf("parts %+v, %d events", c.Parts, len(events))
	}
}
