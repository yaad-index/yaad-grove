# ADR 0018 — Language packs (per-language behavior as data)

**Status:** Accepted (2026-07-13)
**Relates to:** ADR 0013 (per-instance persona), ADR 0014 (conversation memory / follow-up gate), ADR 0016 (externalized grounding prompt template), ADR 0008/0012 (answer + consent copy), issue #25 (i18n catalog + answer-language)

## Context

The bot should work in **any** language, and a native speaker should be able to add a language without touching Go. Today language-specific behavior is baked into engine code in three places:

- **Follow-up detection** carried English keyword lists (`followUpMeta`, `referentialPrefix`) plus an ad-hoc short-message heuristic — English-shaped, and it does not generalize (a Persian follow-up like «…بیشتر توضیح بده» reads as standalone).
- **Prompt scaffolding** is English and gives no per-language guidance (e.g. "answer in this language").
- **User-facing operational strings** (consent disclosure, nudge, rate-limit, refusal) are hardcoded English constants in `internal/runtime`.

Each new language means code changes. That does not scale, and it puts language content in the hands of Go authors rather than native speakers.

## Decision

Introduce a **language pack**: a self-contained data file, `langpacks/<code>.yaml`, authored per language and selected by config. A pack carries the language-specific bits — **prompt additions** now, **operational strings** later. **No language-specific content lives in engine code.** Follow-up detection stops being a per-language concern entirely: it is replaced by a **language-neutral recency signal** (below), so packs carry no follow-up terms.

Two packs ship as samples: **`en.yaml`** (the base / reference every pack mirrors) and **`fa.yaml`** (Persian, the second language).

## (a) Pack format + discovery / loading

A pack is YAML with a fixed, small shape:

```yaml
# langpacks/en.yaml — base / reference
code: en
name: English
# Language-specific system-prompt guidance, appended to the prompt. Empty for the
# base language (English needs none).
prompt: ""
# User-facing operational strings (later increment). Keyed by a stable id; a pack
# may omit any key and inherit the base (en) value.
strings: {}
```

```yaml
# langpacks/fa.yaml — Persian sample
code: fa
name: فارسی
prompt: |
  Answer in Persian (فارسی) unless the user writes in another language.
  Use natural, conversational Persian.
strings: {}   # populated in the strings increment
```

- **Embedded from the repo (primary).** The packs are first-class, source-controlled repo content: a `langpacks/` directory holding `en.yaml`, `fa.yaml`, and future packs, **embedded into the binary via `go:embed`** (the same pattern as the embedded default prompt, ADR 0016). So they ship *with* the engine — present in the distroless image by default, with **no external files required at runtime**. Adding a language is a **PR that adds `langpacks/<code>.yaml` to the repo** — the path by which native speakers contribute a language, reviewed like any change.
- **Selection:** `--language <code>` (default `en`) selects among the embedded built-ins.
- **Optional external override:** `--langpacks-dir <dir>` is an optional extension point — a deployment may point at an external directory to add a not-yet-upstreamed language or override an embedded pack for a code. Unset (the default), only the embedded built-ins are used. The built-in `en` / `fa` / future packs remain first-class repo content regardless.
- **Base + fallback (source-independent merge).** The effective pack for `--language X` is built by overlaying, **in order**: embedded `en` (the base) → embedded `X` (if present) → external `--langpacks-dir/X` (if present). Each layer overrides only the keys it sets; any key it omits inherits the layer below, down to `en`. The **per-key fallback is a property of the merge, not the file source** — so a pack author (embedded OR external) supplies only what differs and never re-states boilerplate (this matters most once `strings` ship: translate one string without re-declaring the rest). An external `fa.yaml` therefore still inherits missing keys from the embedded `fa` and ultimately `en`, and an operator can override a **single** key of the shipped `fa` without re-specifying the pack.

**What a pack may omit:** everything except `code` and `name`. `prompt` defaults to empty (no language guidance); every `strings` key falls through to the same-code embedded layer, then `en`. The minimal external pack is `code` + `name` + the one key being changed.
- **Fail-loud:** a `--language` naming a code with no embedded (or overridden) pack, or a malformed pack, is a startup error — an explicit `--language` is an explicit choice, like `--persona-file`.

## (b) Follow-up detection — the language-neutral recency signal

This replaces `IsFollowUp` and all keyword/short-message heuristics with a pure sender-presence + recency signal. It is language-agnostic **by construction**, so no pack carries follow-up terms — that category is gone. (Spec authored in review; folded in verbatim.)

`Buffer.Select` gains `senderID string` and `window time.Duration`:

```go
func (b *Buffer) Select(chatID, query, senderID string, isReply bool, injectN int, window time.Duration) []Turn
```

The follow-up gate inside `Select`, after the turns are loaded and before the recency-floor + relevance selection:

```go
if !isReply {
    deadline := time.Now().Add(-window)
    found := false
    for _, t := range turns { // turns already loaded from convos[chatID]
        if !t.Bot && t.SpeakerID == senderID && t.Time.After(deadline) {
            found = true
            break
        }
    }
    if !found {
        return nil
    }
}
```

The `isReply` short-circuit stays at the top (a reply is always a follow-up — ADR 0014 / #105).

**Delete** from `internal/memory/select.go`: `followUpMeta`, `referentialPrefix`, `shortMessageMaxTokens`, the short-message block, and `IsFollowUp` entirely (the gate now lives in `Select`). **Keep** `recencyFloor` and the recency-floor logic, and `terms()` / `score()` / `notWord()` (the token-overlap relevance scorer that fills the inject budget after the gate passes — already language-agnostic).

Call site (`internal/runtime/memory.go`) passes `in.User.ID` as `senderID` and threads a `window` from config; config adds `--followup-window` (default `30m`; `0` = replies-only).

**Behaviour:**
- `isReply=true` → always a follow-up (unchanged from #105).
- `isReply=false` + the sender has a non-bot turn in this chat within `window` → follow-up; `Select` runs as before (recency floor + token-overlap relevance).
- `isReply=false` + no such turn → standalone; `nil`.
- Empty `senderID` (should not happen) → no match → standalone (safe fallback).

## (c) Prompt additions, and (later) strings

**Prompt additions (this design):** the pack's `prompt` string is a language-guidance block injected into the system prompt through the existing template machinery (ADR 0016) — a new empty-safe slot, `{{.Language}}`, placed after the standing scope + grounding-contract instructions and immediately **before** the dynamic per-query content (asker). It is a standing instruction (like persona/scope), so it sits with the standing block, not amid the per-query tail; and it is opaque text to the engine, which injects whatever the pack provides with no knowledge of the language. Empty (the `en` default) renders **byte-identically** to today, so the golden prompt fixtures are unchanged and non-`en` deployments opt in.

**Operational strings (a later increment):** the consent disclosure, nudge, rate-limit, refusal, and capacity strings currently hardcoded in `internal/runtime` move into the pack's `strings:` catalog, loaded into a lookup the runtime consults by id (with en fallback). This is the vehicle for issue #25's message catalog + answer-language directive. Deferred to keep the first cut small; the pack shape reserves `strings` for it.

## (d) Engine code stays language-neutral

- **Engine / hot path:** language-**neutral** mechanisms only — the recency follow-up gate (no keywords), the token-overlap relevance scorer (already neutral), and multilingual embedding retrieval (ADR 0017). It injects pack-provided text (prompt additions, later strings) as opaque data. **No language-specific constant or keyword list remains in the answering, gating, or memory path.**
- **Pack (data):** all language-specific content — prompt additions and user-facing strings — authored by a native speaker in YAML, no Go.
- The base `en.yaml` is the reference shape; every other pack mirrors it and inherits its values for unspecified keys.

## Relationship to persona (0013) and the prompt template (0016)

These compose, they do not overlap:

- **Persona** (ADR 0013) is per-**instance** voice and manner (one bot's personality). It stays operator-authored per deployment.
- **A language pack** is per-**language** behavior (answer language, script/RTL, natural phrasing), reusable across every instance of that language.
- **The prompt template** (ADR 0016) is the overall scaffolding; the pack's prompt-addition is a new data slot within it. Render order: persona → scope → grounding contract → **language** → asker → reply-context → history → context — the language slot closes the standing-instruction block, just before the dynamic per-query content begins.

## Implementation increments (after this ADR is accepted)

1. **PR1 — recency follow-up** (§b): removes `IsFollowUp`, language-neutral gate + `--followup-window`. Independent; ships the follow-up win immediately.
2. **PR2 — pack loading + prompt-additions** (§a, §c-prompt): `langpacks/en.yaml` + `fa.yaml`, `--language` / `--langpacks-dir`, the empty-safe prompt slot.
3. **PR3 (later) — operational strings in packs** (§c-strings): move the runtime's user-facing constants into the pack catalog (issue #25).

## Gate decisions

The four design choices, resolved (each as recommended above, and reflected in the body):

1. **Prompt-addition placement** — a distinct `{{.Language}}` slot after scope + grounding, before the per-query content. (Folding it into scope would make scope files language-specific, defeating the architecture.)
2. **Fallback granularity** — per-key overlay onto `en`, as a property of the merge; a pack states only what differs.
3. **Answer-language** — carried inside the pack's `prompt` guidance, authored as the pack author wants (no separate auto-generated field).
4. **First-cut scope** — PR1 (recency) + PR2 (prompt-additions) first; strings (PR3 / #25) later.

## Deferred

- Operational-string localization (PR3 / #25).
- Consent/nudge string packs (part of PR3).
- Precise reply-chain walking + the bot's sent-message-id threading (unrelated ADR 0014 follow-on).
- Auto-detecting the user's language per message (v1 selects one pack per instance).
