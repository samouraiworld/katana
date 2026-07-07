package valopers

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeRPC serves abci_query vm/qeval renders for the valopers realm.
func fakeRPC(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dataHex := strings.TrimPrefix(r.URL.Query().Get("data"), "0x")
		raw, _ := hex.DecodeString(dataHex)
		cmd := string(raw) // e.g. gno.land/r/gnops/valopers.Render("?page=1")

		var render string
		switch {
		case strings.Contains(cmd, `Render("?page=1")`):
			render = "Valopers\n\n- [bad-node](/r/gnops/valopers:g157x4vyop) power 1\n"
		case strings.Contains(cmd, `Render("?page=2")`):
			render = "no more valopers"
		case strings.Contains(cmd, `Render("g157x4vyop")`):
			render = "# bad-node\n\nOperator: g157x4vyop\nSigning Address: g1tzj6signing\n"
		default:
			render = ""
		}
		val := base64.StdEncoding.EncodeToString([]byte(render))
		w.Header().Set("Content-Type", "application/json")
		// gno's RPC abci_query nests the render under ResponseBase.Data (base64).
		_, _ = w.Write([]byte(`{"result":{"response":{"ResponseBase":{"Data":"` + val + `","Error":null}}}}`))
	}))
}

func TestOperatorFor(t *testing.T) {
	srv := fakeRPC(t)
	defer srv.Close()

	r := New(srv.URL, "gno.land/r/gnops/valopers", time.Minute)

	op, ok := r.OperatorFor(context.Background(), "g1tzj6signing")
	if !ok || op != "g157x4vyop" {
		t.Fatalf("OperatorFor(signing) = %q, %v; want g157x4vyop, true", op, ok)
	}

	// Unknown signing address has no mapping.
	if _, ok := r.OperatorFor(context.Background(), "g1unknown"); ok {
		t.Fatalf("expected no mapping for unknown signing address")
	}
}
