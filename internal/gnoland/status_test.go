package gnoland

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"result":{"node_info":{"network":"test14"}}}`)
	}))
	defer srv.Close()

	got, err := New(srv.URL).Network(context.Background())
	if err != nil {
		t.Fatalf("Network: %v", err)
	}
	if got != "test14" {
		t.Fatalf("Network = %q, want test14", got)
	}
}

func TestNetworkErrors(t *testing.T) {
	cases := map[string]string{
		"rpc error":     `{"error":{"message":"boom","data":"detail"}}`,
		"empty network": `{"result":{"node_info":{"network":""}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer srv.Close()

			if _, err := New(srv.URL).Network(context.Background()); err == nil {
				t.Fatal("expected an error")
			} else if name == "rpc error" && !strings.Contains(err.Error(), "boom") {
				t.Fatalf("error should surface the RPC message, got: %v", err)
			}
		})
	}
}

func TestLatestHeight(t *testing.T) {
	cases := map[string]struct {
		body    string
		want    int64
		wantErr bool
	}{
		"ok":          {body: `{"result":{"sync_info":{"latest_block_height":"1234"}}}`, want: 1234},
		"missing":     {body: `{"result":{}}`, wantErr: true},
		"non-numeric": {body: `{"result":{"sync_info":{"latest_block_height":"x"}}}`, wantErr: true},
		"zero":        {body: `{"result":{"sync_info":{"latest_block_height":"0"}}}`, wantErr: true},
		"rpc error":   {body: `{"error":{"message":"boom","data":"detail"}}`, wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			got, err := New(srv.URL).LatestHeight(context.Background())
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("LatestHeight = %d, %v; want %d (err %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
