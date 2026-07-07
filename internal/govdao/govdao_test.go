package govdao

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStatusParsesResponseBaseData guards the RPC-parsing seam: gno's abci_query
// nests the render under result.response.ResponseBase.Data (base64), not a flat
// "value" field. Reading the wrong field yields an empty render → classify →
// Unknown, which silently keeps every proposal pending forever.
func TestStatusParsesResponseBaseData(t *testing.T) {
	render := "## Prop #1\n### Stats\n- **PROPOSAL HAS BEEN ACCEPTED**\n"
	data := base64.StdEncoding.EncodeToString([]byte(render))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"response":{"ResponseBase":{"Data":"` + data + `","Error":null}}}}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, "gno.land/r/gov/dao").Status(context.Background(), "1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != Accepted {
		t.Fatalf("Status = %q, want %q", got, Accepted)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		render string
		want   string
	}{
		{"Status: ACCEPTED", Accepted},
		{"Status: REJECTED", Rejected},
		{"Status: ACTIVE — open for votes", Pending},
		{"Vote YES / Vote NO", Pending},
		{"something entirely unexpected", Unknown},
	}
	for _, tt := range tests {
		if got := classify(tt.render); got != tt.want {
			t.Errorf("classify(%q) = %q, want %q", tt.render, got, tt.want)
		}
	}
}

func TestExtractProposalID(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "json events array",
			out:  `tx result EVENTS: [{"type":"ProposalCreated","attrs":[{"key":"id","value":"7"}]}] done`,
			want: "7",
		},
		{
			name: "text fallback",
			out:  "Event ProposalCreated fired with id 42 on gno.land/r/gov/dao",
			want: "42",
		},
		{
			name: "not present",
			out:  "some unrelated output",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractProposalID(tt.out); got != tt.want {
				t.Errorf("ExtractProposalID() = %q, want %q", got, tt.want)
			}
		})
	}
}
