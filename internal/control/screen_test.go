package control

import (
	"net/http"
	"testing"
)

func TestScreenRequiresInstanceKey(t *testing.T) {
	_, server, _ := instanceAuthFixture(t)
	if got := instanceRequest(server.Handler(), http.MethodGet, "/v1/screen", "", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("screen without key=%d", got)
	}
}
