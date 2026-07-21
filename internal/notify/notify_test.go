package notify

import (
	"context"
	"io"
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

	// Moniker falls back to address when empty, and no link without gnoweb.
	bare := Message(EventSent, Info{Chain: "c", Address: "g1x"})
	if !strings.Contains(bare, "g1x (g1x)") || strings.Contains(bare, "/r/gov/dao:") {
		t.Errorf("unexpected bare message: %q", bare)
	}
}

func TestMessage_LowerVPSent(t *testing.T) {
	msg := Message(EventSent, Info{
		Chain: "test6", Moniker: "bad-node", Address: "g1bad",
		Action: config.ActionLowerVP, CurrentPower: 1000, TargetPower: 600, VPDeltaPercent: -2.0,
	})
	want := "A proposal for validator bad-node (g1bad) on test6 has been sent to lower its voting power from 1000 to 600 (-2.0% of total valset VP)."
	if msg != want {
		t.Errorf("Message = %q, want %q", msg, want)
	}
}

func TestMessage_RaiseVPAccepted(t *testing.T) {
	msg := Message(EventAccepted, Info{
		Chain: "test6", Moniker: "good-node", Address: "g1good",
		Action: config.ActionRaiseVP, CurrentPower: 1000, TargetPower: 1400,
	})
	want := "GovDAO accepted the proposal for validator good-node (g1good) on test6: its voting power has been raised from 1000 to 1400."
	if msg != want {
		t.Errorf("Message = %q, want %q", msg, want)
	}
}

func TestMessage_LowerVPRejected(t *testing.T) {
	msg := Message(EventRejected, Info{
		Chain: "test6", Moniker: "bad-node", Address: "g1bad",
		Action: config.ActionLowerVP, CurrentPower: 1000, RejectedBackoff: 720 * time.Hour,
	})
	want := "GovDAO rejected the proposal for validator bad-node (g1bad) on test6 to lower its voting power: it stays at 1000. A new proposal may be sent after 720h0m0s."
	if msg != want {
		t.Errorf("Message = %q, want %q", msg, want)
	}
}

func TestMessage_EjectUnchanged(t *testing.T) {
	msg := Message(EventSent, Info{Chain: "test6", Moniker: "bad-node", Address: "g1bad", Action: config.ActionEject})
	want := "A proposal for validator bad-node (g1bad) on test6 has been sent to remove it from the valset."
	if msg != want {
		t.Errorf("Message = %q, want %q (eject wording must not change)", msg, want)
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

func TestMessageWouldEjectIncludesCommand(t *testing.T) {
	msg := Message(EventWouldEject, Info{
		Chain:         "local",
		Moniker:       "val4",
		Address:       "g1abc",
		GnokeyCommand: "printf … | gnokey maketx run … ./eject-val4.gno",
	})
	if !strings.Contains(msg, "val4") || !strings.Contains(msg, "g1abc") {
		t.Errorf("missing validator identity: %q", msg)
	}
	if !strings.Contains(msg, "dry_run is enabled") {
		t.Errorf("would_eject message should reference dry_run: %q", msg)
	}
	if !strings.Contains(msg, "gnokey maketx run") {
		t.Errorf("would_eject message should embed the gnokey command: %q", msg)
	}
	// The wording must not unconditionally claim a file was attached — Slack
	// webhooks can't upload, and fall back to an inline block instead.
	if !strings.Contains(msg, "attached, or inline") {
		t.Errorf("would_eject message should describe both delivery paths: %q", msg)
	}
}

func TestDispatchWouldEjectAttachesFile(t *testing.T) {
	var gotContentType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := New(config.Notifications{
		Enabled: true,
		Discord: config.WebhookChannel{Enabled: true, WebhookURL: srv.URL},
	})
	res := n.Dispatch(context.Background(), EventWouldEject, Info{
		Chain: "local", Moniker: "val4", Address: "g1abc",
		ProposalFilename: "eject-val4.gno",
		ProposalCode:     "package main // eject",
		GnokeyCommand:    "gnokey maketx run ... ./eject-val4.gno",
	})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("expected 1 ok result, got %+v", res)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Errorf("expected multipart upload, got content-type %q", gotContentType)
	}
	if !strings.Contains(gotBody, "eject-val4.gno") || !strings.Contains(gotBody, "package main // eject") {
		t.Errorf("multipart body missing the attached script:\n%s", gotBody)
	}
	if !strings.Contains(gotBody, "gnokey maketx run") {
		t.Errorf("multipart body missing the command in payload_json:\n%s", gotBody)
	}
	if !strings.Contains(gotBody, `"embeds"`) {
		t.Errorf("would_eject multipart payload_json should still carry an embed:\n%s", gotBody)
	}
}

func TestDispatchDiscordEmbed(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(config.Notifications{
		Enabled: true,
		Discord: config.WebhookChannel{Enabled: true, WebhookURL: srv.URL},
	})
	n.Dispatch(context.Background(), EventSent, Info{
		Chain: "chain1", Address: "g1x", Action: config.ActionRaiseVP,
		CurrentPower: 100, TargetPower: 140,
	})
	if !strings.Contains(gotBody, `"color":3066993`) {
		t.Errorf("discord body missing expected color: %s", gotBody)
	}
	if !strings.Contains(gotBody, "Voting power increase submitted — chain1") {
		t.Errorf("discord body missing expected title: %s", gotBody)
	}
	if !strings.Contains(gotBody, "embeds") {
		t.Errorf("discord body should use an embeds array, not bare content: %s", gotBody)
	}
}

func TestDispatchSlackAttachment(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := New(config.Notifications{
		Enabled: true,
		Slack: config.WebhookChannel{Enabled: true, WebhookURL: srv.URL},
	})
	n.Dispatch(context.Background(), EventSent, Info{
		Chain: "chain1", Address: "g1x", Action: config.ActionLowerVP,
		CurrentPower: 1000, TargetPower: 600,
	})
	if !strings.Contains(gotBody, `"color":"#E67E22"`) {
		t.Errorf("slack body missing expected color: %s", gotBody)
	}
	if !strings.Contains(gotBody, "Voting power reduction submitted — chain1") {
		t.Errorf("slack body missing expected title: %s", gotBody)
	}
	if !strings.Contains(gotBody, "attachments") {
		t.Errorf("slack body should use an attachments array, not bare text: %s", gotBody)
	}
}

func TestTelegramText(t *testing.T) {
	in := Info{Chain: "chain1", Action: config.ActionRaiseVP, CurrentPower: 100, TargetPower: 140}
	msg := Message(EventSent, in)
	title := Title(EventSent, in)
	want := "🟢 " + title + "\n\n" + msg
	got := telegramText(EventSent, in)
	if got != want {
		t.Errorf("telegramText = %q, want %q", got, want)
	}
}

func TestTitle(t *testing.T) {
	cases := []struct {
		name   string
		event  Event
		action string
		want   string
	}{
		{"raise sent", EventSent, config.ActionRaiseVP, "Voting power increase submitted — chain1"},
		{"raise accepted", EventAccepted, config.ActionRaiseVP, "Voting power increase accepted — chain1"},
		{"raise rejected", EventRejected, config.ActionRaiseVP, "Voting power increase rejected — chain1"},
		{"raise dry-run", EventWouldEject, config.ActionRaiseVP, "Voting power increase (dry-run) — chain1"},
		{"lower sent", EventSent, config.ActionLowerVP, "Voting power reduction submitted — chain1"},
		{"lower accepted", EventAccepted, config.ActionLowerVP, "Voting power reduction accepted — chain1"},
		{"lower rejected", EventRejected, config.ActionLowerVP, "Voting power reduction rejected — chain1"},
		{"lower dry-run", EventWouldEject, config.ActionLowerVP, "Voting power reduction (dry-run) — chain1"},
		{"eject sent", EventSent, config.ActionEject, "Validator ejection submitted — chain1"},
		{"eject accepted", EventAccepted, config.ActionEject, "Validator ejection accepted — chain1"},
		{"eject rejected", EventRejected, config.ActionEject, "Validator ejection rejected — chain1"},
		{"eject dry-run", EventWouldEject, config.ActionEject, "Validator ejection (dry-run) — chain1"},
		{"unknown action", EventSent, "", "katana event submitted — chain1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Title(c.event, Info{Chain: "chain1", Action: c.action})
			if got != c.want {
				t.Errorf("Title(%s, action=%q) = %q, want %q", c.event, c.action, got, c.want)
			}
		})
	}
}

func TestEmbedColor(t *testing.T) {
	cases := map[string]int{
		config.ActionRaiseVP: 3066993,
		config.ActionLowerVP: 15105570,
		config.ActionEject:   15158332,
		"":                   9807270,
	}
	for action, want := range cases {
		if got := embedColor(action); got != want {
			t.Errorf("embedColor(%q) = %d, want %d", action, got, want)
		}
	}
}

func TestSlackColorHex(t *testing.T) {
	cases := map[string]string{
		config.ActionRaiseVP: "#2ECC71",
		config.ActionLowerVP: "#E67E22",
		config.ActionEject:   "#E74C3C",
		"":                   "#95A5A6",
	}
	for action, want := range cases {
		if got := slackColorHex(action); got != want {
			t.Errorf("slackColorHex(%q) = %q, want %q", action, got, want)
		}
	}
}

func TestTelegramEmoji(t *testing.T) {
	cases := map[string]string{
		config.ActionRaiseVP: "🟢",
		config.ActionLowerVP: "🟠",
		config.ActionEject:   "🔴",
		"":                   "⚪",
	}
	for action, want := range cases {
		if got := telegramEmoji(action); got != want {
			t.Errorf("telegramEmoji(%q) = %q, want %q", action, got, want)
		}
	}
}
