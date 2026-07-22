# Colored Notifications Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every katana notification (Discord/Slack/Telegram) a visual marker keyed on the action (`eject`=red, `lower_vp`=orange, `raise_vp`=green), with a title that carries the lifecycle stage (submitted/accepted/rejected/dry-run), while keeping the existing message wording unchanged.

**Architecture:** Two new pure functions in `internal/notify/notify.go` (`Title`, plus unexported per-channel color/emoji helpers) sit next to the existing `Message()`. `Dispatch` changes its per-channel payload construction to use structured embeds/attachments (Discord, Slack) or an emoji-prefixed text (Telegram) instead of bare `content`/`text`. No changes outside `internal/notify` — `cycle.go` already populates `Info.Action`.

**Tech Stack:** Go, `net/http`/`encoding/json` (stdlib only, matching the existing package), `httptest` for tests.

## Global Constraints

- Message wording from `Message()` (`sentText`/`acceptedText`/`rejectedText`/would_eject text) must not change — it becomes the embed description/attachment text/telegram body verbatim.
- Color is a pure function of `Info.Action` only — it must not vary by `Event` (sent/accepted/rejected/would_eject all get the same color for the same action).
- Title varies by both `Info.Action` and `Event` (the lifecycle stage).
- Unknown/unset action falls back to gray `#95A5A6` / ⚪ / title label `"katana event"`, mirroring `Message`'s existing `default:` case — never error on an unrecognized action.
- Exact color mapping: `raise_vp` → `#2ECC71` (decimal `3066993`) green; `lower_vp` → `#E67E22` (decimal `15105570`) orange; `eject` → `#E74C3C` (decimal `15158332`) red.
- Telegram keeps no `parse_mode` (as today) — no markdown escaping needed.
- Spec: `docs/superpowers/specs/2026-07-21-colored-notifications-design.md`.

---

### Task 1: `Title` function + color/emoji helpers

**Files:**
- Modify: `internal/notify/notify.go` (add after `actionNoun`, ~line 152)
- Test: `internal/notify/notify_test.go` (add `TestTitle`)

**Interfaces:**
- Consumes: existing `Event` constants (`EventSent`, `EventAccepted`, `EventRejected`, `EventWouldEject`), `config.ActionEject`/`ActionLowerVP`/`ActionRaiseVP` (already imported in the package as `config.ActionX`), existing `isVPAction`/`vpVerb` are NOT reused (title labels are independent of the verb wording).
- Produces (used by Task 2):
  - `func Title(event Event, in Info) string`
  - `func embedColor(action string) int`
  - `func slackColorHex(action string) string`
  - `func telegramEmoji(action string) string`

- [ ] **Step 1: Write the failing test**

Add to `internal/notify/notify_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/notify/... -run 'TestTitle|TestEmbedColor|TestSlackColorHex|TestTelegramEmoji' -v`
Expected: FAIL — `undefined: Title` (and the other three helpers), since none exist yet.

- [ ] **Step 3: Implement `Title` and the color/emoji helpers**

Add to `internal/notify/notify.go`, right after the existing `actionNoun` function (~line 152):

```go
func actionLabel(action string) string {
	switch action {
	case config.ActionRaiseVP:
		return "Voting power increase"
	case config.ActionLowerVP:
		return "Voting power reduction"
	case config.ActionEject:
		return "Validator ejection"
	default:
		return "katana event"
	}
}

func stageSuffix(event Event) string {
	switch event {
	case EventSent:
		return "submitted"
	case EventAccepted:
		return "accepted"
	case EventRejected:
		return "rejected"
	case EventWouldEject:
		return "(dry-run)"
	default:
		return string(event)
	}
}

// Title renders the short embed/attachment title: the action label, the
// lifecycle stage, and the chain. Color (embedColor/slackColorHex/
// telegramEmoji) is a function of Action alone; Title additionally varies by
// Event so the stage is visible without reading the body.
func Title(event Event, in Info) string {
	return fmt.Sprintf("%s %s — %s", actionLabel(in.Action), stageSuffix(event), in.Chain)
}

// embedColor returns the Discord embed color (decimal RGB) for action.
func embedColor(action string) int {
	switch action {
	case config.ActionRaiseVP:
		return 3066993 // #2ECC71 green
	case config.ActionLowerVP:
		return 15105570 // #E67E22 orange
	case config.ActionEject:
		return 15158332 // #E74C3C red
	default:
		return 9807270 // #95A5A6 gray
	}
}

// slackColorHex returns the Slack attachment color (hex string) for action.
func slackColorHex(action string) string {
	switch action {
	case config.ActionRaiseVP:
		return "#2ECC71"
	case config.ActionLowerVP:
		return "#E67E22"
	case config.ActionEject:
		return "#E74C3C"
	default:
		return "#95A5A6"
	}
}

// telegramEmoji returns the colored-emoji prefix standing in for color on a
// channel with no native color concept.
func telegramEmoji(action string) string {
	switch action {
	case config.ActionRaiseVP:
		return "🟢"
	case config.ActionLowerVP:
		return "🟠"
	case config.ActionEject:
		return "🔴"
	default:
		return "⚪"
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/notify/... -run 'TestTitle|TestEmbedColor|TestSlackColorHex|TestTelegramEmoji' -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/notify/notify.go internal/notify/notify_test.go
git commit -m "feat(notify): add Title and per-action color/emoji helpers"
```

---

### Task 2: Wire structured payloads into `Dispatch`

**Files:**
- Modify: `internal/notify/notify.go:180-227` (the `Dispatch` method body)
- Test: `internal/notify/notify_test.go` (extend `TestDispatchDiscord`, add `TestDispatchDiscordEmbed`, `TestDispatchSlackAttachment`, `TestDispatchTelegramEmoji`, extend `TestDispatchWouldEjectAttachesFile`)

**Interfaces:**
- Consumes: `Title(event, in)`, `embedColor(action)`, `slackColorHex(action)`, `telegramEmoji(action)` from Task 1; existing `n.postJSON`, `n.postMultipart`, `msg := Message(event, in)`, `attach` bool — all unchanged signatures.
- Produces: no new exported symbols; `Result`/`Dispatch` signature unchanged, so `internal/daemon/cycle.go` needs no changes.

- [ ] **Step 1: Write the failing tests**

Add to `internal/notify/notify_test.go`:

```go
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
```

Note: Telegram's real endpoint is `api.telegram.org` (not overridable via
`WebhookURL`), so rather than an httptest round-trip, this test calls a small
extracted helper `telegramText(event, in) string` directly (introduced in
Step 3) rather than going through `Dispatch` — this keeps the test hermetic
without hitting the network.

Also extend the existing `TestDispatchWouldEjectAttachesFile` (in-place, same
file) — after the existing `payload_json` assertion, add:

```go
	if !strings.Contains(gotBody, `"embeds"`) {
		t.Errorf("would_eject multipart payload_json should still carry an embed:\n%s", gotBody)
	}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/notify/... -run 'TestDispatchDiscordEmbed|TestDispatchSlackAttachment|TestDispatchTelegramEmoji|TestDispatchWouldEjectAttachesFile' -v`
Expected: FAIL — `TestDispatchDiscordEmbed`/`TestDispatchSlackAttachment` fail on the missing `"color"`/`"embeds"`/`"attachments"` substrings (current payload is bare `content`/`text`); `TestDispatchTelegramEmoji` fails with `undefined: telegramText`; `TestDispatchWouldEjectAttachesFile` fails on the new `"embeds"` assertion.

- [ ] **Step 3: Implement the structured payloads**

Replace the body of `Dispatch` in `internal/notify/notify.go` (currently
lines ~180-227) with:

```go
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
```

Also update `postJSON`'s signature comment is unchanged (it already takes
`payload any`, so passing `map[string]any` instead of `map[string]string`
needs no signature change — verify this while editing: `func (n *Notifier)
postJSON(ctx context.Context, endpoint string, payload any) error`, already
generic).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/notify/... -v`
Expected: PASS — every test in the package, including the pre-existing
`TestDispatchDiscord`, `TestMessage*`, `TestDispatchWouldEjectAttachesFile`,
and the four new tests from this task.

- [ ] **Step 5: Commit**

```bash
git add internal/notify/notify.go internal/notify/notify_test.go
git commit -m "feat(notify): render colored embeds/attachments per action across Discord, Slack, Telegram"
```

---

### Task 3: Full package regression + docs mention

**Files:**
- Modify: `README.md` (notifications section, wherever Discord/Slack/Telegram payloads or examples are currently described — search for "Discord" first)
- No code changes in this task — verification + documentation only.

**Interfaces:**
- Consumes: nothing new.
- Produces: nothing new.

- [ ] **Step 1: Run the full test suite**

Run: `go test ./...`
Expected: PASS, no regressions in `internal/daemon` (which calls
`notify.Dispatch` via `cycle.go`'s `dispatch` helper — signature unchanged)
or any other package.

- [ ] **Step 2: `go vet`**

Run: `go vet ./...`
Expected: no warnings.

- [ ] **Step 3: Update README notifications section**

Find the section describing notification channels (grep `README.md` for
"Discord" to locate it) and add a short paragraph, e.g.:

```markdown
Notifications are color-coded by action: green for `raise_vp`, orange for
`lower_vp`, red for `eject` (Discord embed color / Slack attachment color;
Telegram uses a 🟢/🟠/🔴 emoji prefix since it has no native color concept).
The title states the lifecycle stage (submitted/accepted/rejected/dry-run).
```

Place it directly after the existing description of what triggers a
notification (sent/accepted/rejected), so it reads as an extension of that
paragraph rather than a new top-level topic.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs(readme): document per-action notification colors"
```
