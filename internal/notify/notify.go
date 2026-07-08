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
	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

// Event identifies which lifecycle transition triggered a notification.
type Event string

const (
	EventSent       Event = "sent"
	EventAccepted   Event = "accepted"
	EventRejected   Event = "rejected"
	EventWouldEject Event = "would_eject"
)

// Info carries the fields a message may reference.
type Info struct {
	Chain           string
	Moniker         string
	Address         string
	GnowebURL       string
	ProposalID      string
	RejectedBackoff time.Duration
	// Observe (would_eject) fields: the proposal script/expression, the filename
	// it is attached under, and the ready-to-run gnokey command to broadcast it.
	ProposalCode     string
	ProposalFilename string
	GnokeyCommand    string
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
	case EventWouldEject:
		msg = fmt.Sprintf("Validator %s (%s) on %s qualifies for ejection, but proposal sending is disabled — no on-chain proposal was submitted.",
			in.display(), in.Address, in.Chain)
		if in.GnokeyCommand != "" {
			msg += "\nTo broadcast it manually, download the attached script and run:\n```\n" + in.GnokeyCommand + "\n```"
		}
		return msg
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
	// Observe notifications ship the proposal as a downloadable file where the
	// channel supports it (Discord, Telegram); Slack webhooks cannot upload, so
	// they fall back to the code inline.
	attach := event == EventWouldEject && in.ProposalCode != "" && in.ProposalFilename != ""
	var results []Result

	if n.cfg.Discord.Enabled {
		var err error
		if attach {
			payload, _ := json.Marshal(map[string]string{"content": msg})
			err = n.postMultipart(ctx, n.cfg.Discord.WebhookURL,
				map[string]string{"payload_json": string(payload)},
				"files[0]", in.ProposalFilename, in.ProposalCode)
		} else {
			err = n.postJSON(ctx, n.cfg.Discord.WebhookURL, map[string]string{"content": msg})
		}
		results = append(results, result("discord", host(n.cfg.Discord.WebhookURL), err))
	}
	if n.cfg.Slack.Enabled {
		text := msg
		if attach {
			text += "\n```\n" + truncate(in.ProposalCode, 1500) + "\n```"
		}
		err := n.postJSON(ctx, n.cfg.Slack.WebhookURL, map[string]string{"text": text})
		results = append(results, result("slack", host(n.cfg.Slack.WebhookURL), err))
	}
	if n.cfg.Telegram.Enabled {
		var err error
		if attach {
			endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", n.cfg.Telegram.BotToken)
			err = n.postMultipart(ctx, endpoint,
				map[string]string{"chat_id": n.cfg.Telegram.ChatID, "caption": msg},
				"document", in.ProposalFilename, in.ProposalCode)
		} else {
			endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.Telegram.BotToken)
			err = n.postJSON(ctx, endpoint, map[string]string{
				"chat_id": n.cfg.Telegram.ChatID,
				"text":    msg,
			})
		}
		results = append(results, result("telegram", n.cfg.Telegram.ChatID, err))
	}
	return results
}

// postMultipart POSTs a multipart/form-data body: the string fields plus one
// file part (fileField/filename/content). Used for channel file uploads.
func (n *Notifier) postMultipart(ctx context.Context, endpoint string, fields map[string]string, fileField, filename, content string) error {
	if endpoint == "" {
		return fmt.Errorf("empty endpoint")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	fw, err := w.CreateFormFile(fileField, filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
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

// truncate caps s to n runes, appending an ellipsis marker when cut, so a long
// proposal script cannot overflow a channel's message-size limit (Discord 2000).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n… (truncated)"
}
