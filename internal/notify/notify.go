// Package notify formats and dispatches katana notifications to Discord, Slack
// and Telegram. Messages are emitted around the on-chain proposal lifecycle
// (sent / accepted / rejected) and are English by design.
//
// Dispatch is best-effort: a channel failure is returned as a Result but never
// propagated as an error, so it can be logged/persisted without affecting the
// worker.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

// Event identifies which lifecycle transition triggered a notification.
type Event string

const (
	EventSent     Event = "sent"
	EventAccepted Event = "accepted"
	EventRejected Event = "rejected"
)

// Info carries the fields a message may reference.
type Info struct {
	Chain           string
	Moniker         string
	Address         string
	GnowebURL       string
	ProposalID      string
	RejectedBackoff time.Duration
}

func (i Info) display() string {
	if i.Moniker != "" {
		return i.Moniker
	}
	return i.Address
}

// proposalLink returns a gnoweb link to the GovDAO proposal, or "".
func (i Info) proposalLink() string {
	if i.GnowebURL == "" || i.ProposalID == "" {
		return ""
	}
	return fmt.Sprintf("%s/r/gov/dao:%s", i.GnowebURL, i.ProposalID)
}

// Message renders the English notification text for an event.
func Message(event Event, in Info) string {
	var msg string
	switch event {
	case EventSent:
		msg = fmt.Sprintf("A proposal for validator %s (%s) on %s has been sent to remove it from the valset.",
			in.display(), in.Address, in.Chain)
	case EventAccepted:
		msg = fmt.Sprintf("GovDAO accepted the proposal for validator %s (%s) on %s: it has been removed from the valset.",
			in.display(), in.Address, in.Chain)
	case EventRejected:
		msg = fmt.Sprintf("GovDAO rejected the proposal for validator %s (%s) on %s: it stays in the valset. A new proposal may be sent after %s.",
			in.display(), in.Address, in.Chain, in.RejectedBackoff)
	default:
		msg = fmt.Sprintf("katana event %q for validator %s (%s) on %s.", event, in.display(), in.Address, in.Chain)
	}
	if link := in.proposalLink(); link != "" {
		msg += " " + link
	}
	return msg
}

// Result is the per-channel outcome of a Dispatch.
type Result struct {
	Channel string // discord | slack | telegram
	Target  string // webhook host or chat id (for audit)
	OK      bool
	Err     string
}

// Notifier dispatches to the configured channels.
type Notifier struct {
	cfg  config.Notifications
	http *http.Client
}

// New builds a Notifier.
func New(cfg config.Notifications) *Notifier {
	return &Notifier{cfg: cfg, http: &http.Client{Timeout: 10 * time.Second}}
}

// Enabled reports whether any channel is active.
func (n *Notifier) Enabled() bool {
	return n.cfg.Enabled && (n.cfg.Discord.Enabled || n.cfg.Slack.Enabled || n.cfg.Telegram.Enabled)
}

// Dispatch sends the message for event to every enabled channel and returns one
// Result per channel. It never returns an error.
func (n *Notifier) Dispatch(ctx context.Context, event Event, in Info) []Result {
	if !n.cfg.Enabled {
		return nil
	}
	msg := Message(event, in)
	var results []Result

	if n.cfg.Discord.Enabled {
		err := n.postJSON(ctx, n.cfg.Discord.WebhookURL, map[string]string{"content": msg})
		results = append(results, result("discord", host(n.cfg.Discord.WebhookURL), err))
	}
	if n.cfg.Slack.Enabled {
		err := n.postJSON(ctx, n.cfg.Slack.WebhookURL, map[string]string{"text": msg})
		results = append(results, result("slack", host(n.cfg.Slack.WebhookURL), err))
	}
	if n.cfg.Telegram.Enabled {
		endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.Telegram.BotToken)
		err := n.postJSON(ctx, endpoint, map[string]string{
			"chat_id": n.cfg.Telegram.ChatID,
			"text":    msg,
		})
		results = append(results, result("telegram", n.cfg.Telegram.ChatID, err))
	}
	return results
}

func (n *Notifier) postJSON(ctx context.Context, endpoint string, payload any) error {
	if endpoint == "" {
		return fmt.Errorf("empty endpoint")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}

func result(channel, target string, err error) Result {
	r := Result{Channel: channel, Target: target, OK: err == nil}
	if err != nil {
		r.Err = err.Error()
	}
	return r
}

func host(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return ""
}
