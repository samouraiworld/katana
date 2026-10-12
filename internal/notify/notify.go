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
	EventSent     Event = "sent"
	EventAccepted Event = "accepted"
	EventRejected Event = "rejected"
	// EventExpired: the proposal stayed open past vote_tracking.pending_timeout.
	// It is still open on-chain; katana keeps watching it and the validator
	// stays gated.
	EventExpired Event = "expired"
	// EventUnconfirmed: gnokey failed after sending the tx, so the proposal
	// may or may not be on-chain. An operator has to check.
	EventUnconfirmed Event = "unconfirmed"
	// EventWouldEject announces a proposal a dry_run chain would have sent;
	// nothing was signed or broadcast.
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
	PendingTimeout  time.Duration
	// Action is the score-driven action this notification is about: eject |
	// lower_vp | raise_vp. CurrentPower/TargetPower are 0 for eject.
	// VPDeltaPercent is the signed % of the chain's total valset VP this
	// move represents (only used by the "sent" wording).
	Action         string
	CurrentPower   int64
	TargetPower    int64
	VPDeltaPercent float64
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
		msg = sentText(in)
	case EventAccepted:
		msg = acceptedText(in)
	case EventRejected:
		msg = rejectedText(in)
	case EventExpired:
		msg = fmt.Sprintf("The %s proposal for validator %s (%s) on %s expired: no supermajority after %s. It is still open on-chain and cannot be withdrawn: katana files no other proposal for this validator until it is decided, and keeps watching it.",
			actionNoun(in.Action), in.display(), in.Address, in.Chain, in.PendingTimeout)
	case EventUnconfirmed:
		msg = fmt.Sprintf("The %s proposal for validator %s (%s) on %s may or may not be on-chain: gnokey failed after sending it. katana files no other proposal for this validator until this is resolved; check the chain for a GovDAO proposal from katana's key.",
			actionNoun(in.Action), in.display(), in.Address, in.Chain)
	case EventWouldEject:
		msg = fmt.Sprintf("Validator %s (%s) on %s qualifies for %s, but dry_run is enabled — nothing was broadcast.",
			in.display(), in.Address, in.Chain, actionNoun(in.Action))
		if in.GnokeyCommand != "" {
			msg += "\nTo broadcast it manually, run:\n```\n" + in.GnokeyCommand + "\n```\nThe proposal script is included with this notification (attached, or inline below if your channel doesn't support attachments)."
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

func isVPAction(action string) bool {
	return action == config.ActionLowerVP || action == config.ActionRaiseVP
}

func vpVerb(action string, past bool) string {
	if action == config.ActionRaiseVP {
		if past {
			return "raised"
		}
		return "raise"
	}
	if past {
		return "lowered"
	}
	return "lower"
}

func sentText(in Info) string {
	if isVPAction(in.Action) {
		return fmt.Sprintf("A proposal for validator %s (%s) on %s has been sent to %s its voting power from %d to %d (%+.1f%% of total valset VP).",
			in.display(), in.Address, in.Chain, vpVerb(in.Action, false), in.CurrentPower, in.TargetPower, in.VPDeltaPercent)
	}
	return fmt.Sprintf("A proposal for validator %s (%s) on %s has been sent to remove it from the valset.",
		in.display(), in.Address, in.Chain)
}

func acceptedText(in Info) string {
	if isVPAction(in.Action) {
		return fmt.Sprintf("GovDAO accepted the proposal for validator %s (%s) on %s: its voting power has been %s from %d to %d.",
			in.display(), in.Address, in.Chain, vpVerb(in.Action, true), in.CurrentPower, in.TargetPower)
	}
	return fmt.Sprintf("GovDAO accepted the proposal for validator %s (%s) on %s: it has been removed from the valset.",
		in.display(), in.Address, in.Chain)
}

func rejectedText(in Info) string {
	if isVPAction(in.Action) {
		return fmt.Sprintf("GovDAO rejected the proposal for validator %s (%s) on %s to %s its voting power: it stays at %d. A new proposal may be sent after %s.",
			in.display(), in.Address, in.Chain, vpVerb(in.Action, false), in.CurrentPower, in.RejectedBackoff)
	}
	return fmt.Sprintf("GovDAO rejected the proposal for validator %s (%s) on %s: it stays in the valset. A new proposal may be sent after %s.",
		in.display(), in.Address, in.Chain, in.RejectedBackoff)
}

// actionMeta groups every per-action display property (used to render the
// message noun, the embed/attachment title, and each channel's color/emoji)
// in one place, so a future action only needs one new map entry instead of
// five parallel switches that can silently drift out of sync.
type actionMeta struct {
	noun  string
	label string
	rgb   int
	hex   string
	emoji string
}

var actionMetas = map[string]actionMeta{
	config.ActionEject:   {noun: "ejection", label: "Validator ejection", rgb: 15158332, hex: "#E74C3C", emoji: "🔴"},
	config.ActionLowerVP: {noun: "a voting-power reduction", label: "Voting power reduction", rgb: 15105570, hex: "#E67E22", emoji: "🟠"},
	config.ActionRaiseVP: {noun: "a voting-power increase", label: "Voting power increase", rgb: 3066993, hex: "#2ECC71", emoji: "🟢"},
}

// unknownActionMeta is the fallback for an empty/unrecognized action. The
// noun still reads as "ejection" (Message's EventWouldEject wording predates
// the Action field and always assumed eject when unset); colors/label render
// as a neutral, unbranded event instead.
var unknownActionMeta = actionMeta{noun: "ejection", label: "katana event", rgb: 9807270, hex: "#95A5A6", emoji: "⚪"}

func metaFor(action string) actionMeta {
	if m, ok := actionMetas[action]; ok {
		return m
	}
	return unknownActionMeta
}

func actionNoun(action string) string    { return metaFor(action).noun }
func actionLabel(action string) string   { return metaFor(action).label }
func embedColor(action string) int       { return metaFor(action).rgb }
func slackColorHex(action string) string { return metaFor(action).hex }
func telegramEmoji(action string) string { return metaFor(action).emoji }

func stageSuffix(event Event) string {
	switch event {
	case EventSent:
		return "submitted"
	case EventAccepted:
		return "accepted"
	case EventRejected:
		return "rejected"
	case EventExpired:
		return "expired"
	case EventUnconfirmed:
		return "unconfirmed"
	case EventWouldEject:
		return "(dry-run)"
	default:
		return string(event)
	}
}

// Title renders the short embed/attachment title: the action label, the
// lifecycle stage, the validator (moniker, falling back to address), and the
// chain. Color (embedColor/slackColorHex/telegramEmoji) is a function of
// Action alone; Title additionally varies by Event so the stage is visible
// without reading the body.
func Title(event Event, in Info) string {
	return fmt.Sprintf("%s %s — %s (%s)", actionLabel(in.Action), stageSuffix(event), in.display(), in.Chain)
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
	title := Title(event, in)
	// Observe notifications ship the proposal as a downloadable file where the
	// channel supports it (Discord, Telegram); Slack webhooks cannot upload, so
	// they fall back to the code inline.
	attach := event == EventWouldEject && in.ProposalCode != "" && in.ProposalFilename != ""
	var results []Result

	if n.cfg.Discord.Enabled {
		var err error
		embed := map[string]any{"title": title, "description": msg, "color": embedColor(in.Action)}
		if attach {
			payload, _ := json.Marshal(map[string]any{"embeds": []any{embed}})
			err = n.postMultipart(ctx, n.cfg.Discord.WebhookURL,
				map[string]string{"payload_json": string(payload)},
				"files[0]", in.ProposalFilename, in.ProposalCode)
		} else {
			err = n.postJSON(ctx, n.cfg.Discord.WebhookURL, map[string]any{"embeds": []any{embed}})
		}
		results = append(results, result("discord", host(n.cfg.Discord.WebhookURL), err))
	}
	if n.cfg.Slack.Enabled {
		text := msg
		if attach {
			text += "\n```\n" + truncate(in.ProposalCode, 1500) + "\n```"
		}
		attachment := map[string]any{
			"color": slackColorHex(in.Action), "title": title, "text": text, "fallback": text,
		}
		err := n.postJSON(ctx, n.cfg.Slack.WebhookURL, map[string]any{"attachments": []any{attachment}})
		results = append(results, result("slack", host(n.cfg.Slack.WebhookURL), err))
	}
	if n.cfg.Telegram.Enabled {
		text := telegramText(event, in)
		var err error
		if attach {
			endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", n.cfg.Telegram.BotToken)
			err = n.postMultipart(ctx, endpoint,
				map[string]string{"chat_id": n.cfg.Telegram.ChatID, "caption": text},
				"document", in.ProposalFilename, in.ProposalCode)
		} else {
			endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.Telegram.BotToken)
			err = n.postJSON(ctx, endpoint, map[string]string{
				"chat_id": n.cfg.Telegram.ChatID,
				"text":    text,
			})
		}
		results = append(results, result("telegram", n.cfg.Telegram.ChatID, err))
	}
	return results
}

// telegramText prefixes the message with a colored-emoji marker and the
// title, since Telegram's Bot API has no color concept for plain messages.
func telegramText(event Event, in Info) string {
	return fmt.Sprintf("%s %s\n\n%s", telegramEmoji(in.Action), Title(event, in), Message(event, in))
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
