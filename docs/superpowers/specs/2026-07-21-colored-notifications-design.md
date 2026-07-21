# Colored notifications (embed styling by action)

## Context

Today `internal/notify.Dispatch` sends every event (`sent`/`accepted`/
`rejected`/`would_eject`) as plain text on all three channels: Discord gets
`{"content": msg}`, Slack `{"text": msg}`, Telegram `sendMessage`/
`sendDocument` with a `text`/`caption` field. There is no visual distinction
between an `eject`, a `lower_vp` or a `raise_vp` notification beyond reading
the sentence — an operator skimming a busy channel can't tell at a glance
whether a validator gained or lost voting power, or was removed entirely.

The goal: add a colored, structured presentation — green for a voting-power
increase, orange for a reduction, red for an ejection — modeled on the
Discord-embed style already familiar from other ops bots (colored left
border, short title, message body), across all three notification channels
(Discord and Slack support real color; Telegram does not, so it gets a
colored-emoji equivalent).

## Goal

- Every notification carries a color/visual marker keyed on `Info.Action`
  (`eject` / `lower_vp` / `raise_vp`), consistent across `sent`/`accepted`/
  `rejected`/`would_eject` — the color says *what kind of action*, not *what
  stage it's at*.
- The title carries the *stage* (submitted/accepted/rejected/dry-run), so
  title + color together tell the full story at a glance without reading the
  body.
- The existing message body (`Message()` — `sentText`/`acceptedText`/
  `rejectedText`/would_eject text) is unchanged and becomes the
  description/body of the structured payload. No change to wording, no
  restructuring into separate fields.
- Discord and Slack render true color; Telegram substitutes a colored-emoji
  prefix since Bot API messages have no color concept.

## Mapping

| Action | Discord/Slack color | Telegram emoji | Title label |
|---|---|---|---|
| `raise_vp` | `#2ECC71` (green) | 🟢 | "Voting power increase" |
| `lower_vp` | `#E67E22` (orange) | 🟠 | "Voting power reduction" |
| `eject` | `#E74C3C` (red) | 🔴 | "Validator ejection" |
| unknown/other | `#95A5A6` (gray) | ⚪ | "katana event" (fallback, matches today's `default:` case in `Message`) |

Title = `"{label} {stage suffix} — {chain}"`:

- `EventSent` → *"submitted"*
- `EventAccepted` → *"accepted"*
- `EventRejected` → *"rejected"*
- `EventWouldEject` → *"(dry-run)"*

Example: `raise_vp` + `EventSent` on chain `gnoland-test` → title
`"Voting power increase submitted — gnoland-test"`, color `#2ECC71`.

## `internal/notify` changes

New pure functions alongside the existing `Message()`:

```go
func Title(event Event, in Info) string
func embedColor(action string) int        // decimal RGB, for Discord embeds
func slackColorHex(action string) string   // "#RRGGBB", for Slack attachments
func telegramEmoji(action string) string
```

`Title` and the color/emoji helpers switch on `in.Action` (and, for `Title`
only, also on `event`) using the mapping table above; unrecognized actions
fall through to the gray/⚪/"katana event" row rather than erroring, mirroring
`Message`'s existing `default:` branch.

`Dispatch` payload construction changes per channel (transport only — no
change to `Message()`, no change to `cycle.go`, no change to `Info`'s fields):

- **Discord**: `{"content": msg}` becomes
  `{"embeds": [{"title": Title(...), "description": msg, "color": embedColor(...)}]}`.
  The `attach` (would_eject with file) path keeps posting the file as a
  separate multipart part; `payload_json` carries the same `embeds` structure
  instead of bare `content`.
- **Slack**: `{"text": msg}` becomes
  `{"attachments": [{"color": slackColorHex(...), "title": Title(...), "text": msg, "fallback": msg}]}`.
  `fallback` is populated so clients that don't render attachments still show
  something.
- **Telegram**: `text`/`caption` becomes
  `"{emoji} {Title(...)}\n\n{msg}"`. No `parse_mode` is set (as today), so no
  markdown escaping is needed.

## Testing

- `internal/notify`: table-driven `TestTitle` covering all 4 events × 3
  actions (12 cases) plus the unknown-action fallback.
- Extend the existing Discord/Slack/Telegram httptest-server tests in
  `notify_test.go` to assert the posted JSON body contains the expected
  `embeds[0].color`/`attachments[0].color`/emoji-prefixed text for at least
  one case per action, plus the existing would_eject attachment case still
  carries the embed/attachment alongside the file.
- No change needed to `internal/daemon` tests — `Info.Action` is already
  populated by `cycle.go` today; this spec only changes how `notify` renders
  it.

## Out of scope

- Any change to the wording produced by `sentText`/`acceptedText`/
  `rejectedText`/would_eject text.
- Restructuring the message into separate Discord embed fields
  (Validator/Action/Power/Chain as distinct fields) — decided against in
  favor of keeping the existing sentence as a single description, to
  minimize risk to existing text-based tests and keep one source of truth
  for the message content.
- Per-event (rather than per-action) color variation (e.g. a rejected
  proposal turning gray/neutral) — color stays a pure function of the
  action throughout its lifecycle.
- Any new config field to customize colors/emoji — the mapping is a fixed,
  hardcoded table for this iteration.
