package gnoland

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidatorsPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/validators" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("page") {
		case "1":
			fmt.Fprint(w, `{"result":{"validators":[{"address":"g1a","voting_power":"10"}],"total":"2"}}`)
		case "2":
			fmt.Fprint(w, `{"result":{"validators":[{"address":"g1b","voting_power":"5"}],"total":"2"}}`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
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
		"missing total":     `{"result":{"validators":[{"address":"g1a","voting_power":"1"}]}}`,
		"non-numeric total": `{"result":{"validators":[{"address":"g1a","voting_power":"1"}],"total":"x"}}`,
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

// A total larger than what the pages return must not loop forever nor return
// a silently truncated set.
func TestValidatorsShortPagesFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, `{"result":{"validators":[{"address":"g1a","voting_power":"1"}],"total":"3"}}`)
			return
		}
		fmt.Fprint(w, `{"result":{"validators":[],"total":"3"}}`)
	}))
	defer srv.Close()

	if got, err := New(srv.URL).Validators(context.Background()); err == nil {
		t.Fatalf("expected an error for a truncated valset, got %+v", got)
	}
}
