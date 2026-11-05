package api

import (
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Fields the server answers with null (or leaves out) must say so in the
// contract, or generated clients reject real answers.
func TestOpenAPINullableObjects(t *testing.T) {
	a := Mount(chi.NewRouter(), Options{})
	schemas := a.OpenAPI().Components.Schemas.Map()
	for _, name := range []string{"LastSend", "MessageFolder"} {
		s := schemas[name]
		if s == nil || !s.Nullable {
			t.Errorf("schema %s is not nullable", name)
		}
	}
	if m := schemas["Message"]; m == nil || slices.Contains(m.Required, "replyTo") {
		t.Error("Message.replyTo is required, but it is absent without a Reply-To header")
	}
}
