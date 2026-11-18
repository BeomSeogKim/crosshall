package wire

import (
	"encoding/json"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func TestOwnerEnvelopeOmitsMemberProof(t *testing.T) {
	request := Request{
		Version: Version,
		Attempt: "attempt",
		Verb:    "owner channel create",
		Args:    json.RawMessage(`{"name":"checkout"}`),
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["gen"]; ok {
		t.Fatal("owner request includes gen")
	}
	if _, ok := fields["kd"]; ok {
		t.Fatal("owner request includes kd")
	}
	if string(fields["args"]) != `{"name":"checkout"}` {
		t.Fatalf("wrong args: %s", fields["args"])
	}
}

func TestResponseEnvelope(t *testing.T) {
	response := Response{
		Version: Version,
		Attempt: "attempt",
		Result: core.Result{
			Status: core.ExitNotEnrolled,
			Code:   core.CodeKeyRetired,
			Data:   json.RawMessage(`{}`),
		},
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"v":1,"attempt":"attempt","status":4,"code":"key_retired","data":{}}`
	if string(data) != want {
		t.Fatalf("wrong envelope: %s", data)
	}
}
