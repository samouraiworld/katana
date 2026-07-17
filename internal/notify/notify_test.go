package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

func TestMessage(t *testing.T) {
	in := Info{
		Chain: "betanet", Moniker: "Sam", Address: "g1abc",
		GnowebURL: "https://betanet.example", ProposalID: "5",
		RejectedBackoff: 720 * time.Hour,
	}

	sent := Message(EventSent, in)
	if !strings.Contains(sent, "Sam (g1abc)") || !strings.Contains(sent, "has been sent to remove") {
		t.Errorf("unexpected sent message: %q", sent)
	}
	if !strings.Contains(sent, "https://betanet.example/r/gov/dao:5") {
		t.Errorf("sent message missing gnoweb link: %q", sent)
	}
	if !strings.Contains(Message(EventAccepted, in), "has been removed from the valset") {
		t.Errorf("unexpected accepted message")
	}
	if !strings.Contains(Message(EventRejected, in), "stays in the valset") {
		t.Errorf("unexpected rejected message")
	}

	// Dry-run messages are unmistakably labeled and say nothing went on-chain.
	dry := Message(EventDryRun, in)
	if !strings.HasPrefix(dry, "[DRY-RUN]") || !strings.Contains(dry, "Sam (g1abc)") ||
		!strings.Contains(dry, "nothing was broadcast") {
		t.Errorf("unexpected dry-run message: %q", dry)
	}

	// Moniker falls back to address when empty, and no link without gnoweb.
	bare := Message(EventSent, Info{Chain: "c", Address: "g1x"})
	if !strings.Contains(bare, "g1x (g1x)") || strings.Contains(bare, "/r/gov/dao:") {
		t.Errorf("unexpected bare message: %q", bare)
	}
}

func TestDispatchDiscord(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(config.Notifications{
		Enabled: true,
		Discord: config.WebhookChannel{Enabled: true, WebhookURL: srv.URL},
	})
	results := n.Dispatch(context.Background(), EventSent, Info{Chain: "c", Address: "g1x"})
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("expected 1 ok result, got %+v", results)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("webhook not called")
	}
}

func TestDispatchDisabled(t *testing.T) {
	n := New(config.Notifications{Enabled: false})
	if got := n.Dispatch(context.Background(), EventSent, Info{}); got != nil {
		t.Fatalf("expected nil results when disabled, got %+v", got)
	}
}
