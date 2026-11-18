package domain

import (
	"encoding/json"
	"testing"
)

func TestDecodeRoundTrip(t *testing.T) {
	for _, cmd := range []Command{
		CreateChannel{Name: "checkout", Topic: "payment screen"},
		Join{Channel: "checkout", Alias: "form", Focus: "refunds"},
		Join{},
		Who{},
	} {
		raw, err := json.Marshal(cmd)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(cmd.Verb(), raw)
		if err != nil || got != cmd {
			t.Fatalf("%T: got %#v, %v", cmd, got, err)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	for _, tc := range []struct{ verb, args string }{
		{"join", `{"channel":"c","from":"@owner"}`},
		{"join", `[1]`},
		{"frobnicate", `{}`},
	} {
		if _, err := Decode(tc.verb, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s %s: accepted", tc.verb, tc.args)
		}
	}
}

func TestPayloadHashCoversEveryArgument(t *testing.T) {
	a, _ := payloadHash(Join{Channel: "c", Alias: "a"})
	b, _ := payloadHash(Join{Channel: "c", Alias: "a", Focus: "x"})
	c, _ := payloadHash(CreateChannel{Name: "c"})
	d, _ := payloadHash(CreateChannel{Name: "c", Topic: "t"})
	if a == b || c == d || a == c {
		t.Fatal("payload hash ignores an argument or the verb")
	}
}
