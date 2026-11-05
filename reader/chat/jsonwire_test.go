// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package chat

import (
	"context"
	"encoding/json/v2"
	"testing"

	"latere.ai/x/lectio/reader"
)

// The request to a model leaves a list nobody set as null, which providers
// read as absent, and writes the schema it carries as it was given.
func TestTheRequestToAModelIsExact(t *testing.T) {
	got, err := json.Marshal(request{Model: "m"}, outboundJSON)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"model":"m","messages":null}`; string(got) != want {
		t.Errorf("%s, want %s", got, want)
	}
	if want := `{"additionalProperties":false,"properties":{"blocks":{"items":{"additionalProperties":false,"properties":{"box":{"items":{"type":"integer"},"type":"array"},"description":{"type":["string","null"]},"kind":{"enum":`; string(pageSchema[:len(want)]) != want {
		t.Errorf("the page schema starts %s", pageSchema)
	}
}

// A reply that repeats a name, writes it in capitals or holds bytes that
// are not UTF-8 is read.
func TestAReplyOfAnotherWriterIsRead(t *testing.T) {
	e := serve(t)
	e.body = "{\"MODEL\":\"m\",\"choices\":[{\"finish_reason\":\"stop\",\"finish_reason\":\"stop\",\"Message\":{\"content\":\"a\xffb\"}}]}"
	c, err := newClient(Config{Name: "n", Endpoint: e.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	done, err := c.complete(context.Background(), reader.NewCredential("k"), c.request(nil))
	if err != nil || done.content != "a�b" || done.model != "m" {
		t.Fatalf("%+v, %v", done, err)
	}
}
