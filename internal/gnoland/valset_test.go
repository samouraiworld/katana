package gnoland

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The response shape is tm2's (see e2e/testdata/rpc_validators.json): the
// whole set at once, no pagination, no total.
func TestValidators(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/validators" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"","result":{"block_height":"77","validators":[
			{"address":"g1a","pub_key":{"@type":"/tm.PubKeyEd25519","value":"x"},"voting_power":"10","proposer_priority":"0"},
			{"address":"g1b","pub_key":{"@type":"/tm.PubKeyEd25519","value":"y"},"voting_power":"5","proposer_priority":"-5"}]}}`)
	}))
	defer srv.Close()

	got, err := New(srv.URL).Validators(context.Background())
	if err != nil {
		t.Fatalf("Validators: %v", err)
	}
	want := []Validator{{Address: "g1a", VotingPower: 10}, {Address: "g1b", VotingPower: 5}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Validators = %+v, want %+v", got, want)
	}
}

// An unreadable voting power or total must fail the call rather than read as
// 0: the VP-change targets and the min_valset_size guardrail are computed from
// these values.
func TestValidatorsFailsClosed(t *testing.T) {
	cases := map[string]string{
		"rpc error":         `{"error":{"message":"boom","data":"detail"}}`,
		"missing power":     `{"result":{"validators":[{"address":"g1a"}],"total":"1"}}`,
		"non-numeric power": `{"result":{"validators":[{"address":"g1a","voting_power":"ten"}],"total":"1"}}`,
		"negative power":    `{"result":{"validators":[{"address":"g1a","voting_power":"-1"}],"total":"1"}}`,
		"zero power":        `{"result":{"validators":[{"address":"g1a","voting_power":"0"}],"total":"1"}}`,
		"empty address":     `{"result":{"validators":[{"address":"","voting_power":"1"}],"total":"1"}}`,
		"duplicate address": `{"result":{"validators":[{"address":"g1a","voting_power":"1"},{"address":"G1A","voting_power":"1"}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer srv.Close()

			if got, err := New(srv.URL).Validators(context.Background()); err == nil {
				t.Fatalf("expected an error, got %+v", got)
			}
		})
	}
}
