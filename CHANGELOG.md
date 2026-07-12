# Changelog

## [0.7.0](https://github.com/yaad-index/yaad-grove/compare/v0.6.0...v0.7.0) (2026-07-12)


### Features

* **tools:** per-instance MCP tool allow/deny-list (closes [#87](https://github.com/yaad-index/yaad-grove/issues/87)) ([#89](https://github.com/yaad-index/yaad-grove/issues/89)) ([f6a4dd4](https://github.com/yaad-index/yaad-grove/commit/f6a4dd472254082b54127310bd450378283c9509))


### Bug Fixes

* **model:** parse native tool-call format so sentinels don't leak (closes [#88](https://github.com/yaad-index/yaad-grove/issues/88)) ([#90](https://github.com/yaad-index/yaad-grove/issues/90)) ([ce689bc](https://github.com/yaad-index/yaad-grove/commit/ce689bc6de82e2d12ff5f8b92f5675e4b5c9adce))

## [0.6.0](https://github.com/yaad-index/yaad-grove/compare/v0.5.0...v0.6.0) (2026-07-11)


### Features

* **transcript:** durable role-tagged conversation transcript (ADR 0015) ([#81](https://github.com/yaad-index/yaad-grove/issues/81)) ([c3e52fb](https://github.com/yaad-index/yaad-grove/commit/c3e52fb69a0cae4acb2f4ee1138fd0a46efb193b))


### Bug Fixes

* **memory:** language-agnostic follow-up detection (closes [#84](https://github.com/yaad-index/yaad-grove/issues/84)) ([#85](https://github.com/yaad-index/yaad-grove/issues/85)) ([6e4455e](https://github.com/yaad-index/yaad-grove/commit/6e4455e4e8849d7f514aa72ecc912300f82dcda4))

## [0.5.0](https://github.com/yaad-index/yaad-grove/compare/v0.4.0...v0.5.0) (2026-07-11)


### Features

* **cmd,retrieval:** wire semantic retrieval (ADR 0017 build 3/4) ([#79](https://github.com/yaad-index/yaad-grove/issues/79)) ([56c91c0](https://github.com/yaad-index/yaad-grove/commit/56c91c0b103bf1516c7bb4e2ffb98930b22db922))
* **embed:** OpenAI-compatible embeddings client (ADR 0017 build 1/4) ([#76](https://github.com/yaad-index/yaad-grove/issues/76)) ([64d56d1](https://github.com/yaad-index/yaad-grove/commit/64d56d162c9e2a0be1df8ad27e8ffe8bfce447e9))
* **retrieval:** semantic retriever + in-memory cosine index (ADR 0017 build 2/4) ([#78](https://github.com/yaad-index/yaad-grove/issues/78)) ([d4ad3c2](https://github.com/yaad-index/yaad-grove/commit/d4ad3c267f553592b73822d7bd7ce0ae787962a3))

## [0.4.0](https://github.com/yaad-index/yaad-grove/compare/v0.3.3...v0.4.0) (2026-07-11)


### Features

* **core,cmd:** externalize the grounding prompt into a template (ADR 0016) ([#71](https://github.com/yaad-index/yaad-grove/issues/71)) ([2c96ee4](https://github.com/yaad-index/yaad-grove/commit/2c96ee407f27907250cd9be9ece905bce1cb65a4))


### Bug Fixes

* **core:** log the prompt-template exec-error fallback (closes [#73](https://github.com/yaad-index/yaad-grove/issues/73)) ([#74](https://github.com/yaad-index/yaad-grove/issues/74)) ([3112020](https://github.com/yaad-index/yaad-grove/commit/3112020f203f22839a89b7e9514f1d96dfd87d63))

## [0.3.3](https://github.com/yaad-index/yaad-grove/compare/v0.3.2...v0.3.3) (2026-07-11)


### Bug Fixes

* **telegram:** strip the bot's own [@mention](https://github.com/mention) from the query ([#68](https://github.com/yaad-index/yaad-grove/issues/68)) ([e787c79](https://github.com/yaad-index/yaad-grove/commit/e787c7953ddc6131cda76295a4296021d0bc3e18))

## [0.3.2](https://github.com/yaad-index/yaad-grove/compare/v0.3.1...v0.3.2) (2026-07-11)


### Bug Fixes

* keep source grounding internal — don't surface vault refs as dead links ([#66](https://github.com/yaad-index/yaad-grove/issues/66)) ([b956571](https://github.com/yaad-index/yaad-grove/commit/b95657182ee63dd568092e0d0a2a6e3e55e704b0))

## [0.3.1](https://github.com/yaad-index/yaad-grove/compare/v0.3.0...v0.3.1) (2026-07-11)


### Bug Fixes

* **core:** meta follow-ups reach the model + persona-shaped refusals ([#63](https://github.com/yaad-index/yaad-grove/issues/63)) ([9675b1c](https://github.com/yaad-index/yaad-grove/commit/9675b1cd8a495f6a81f5f494ddd16402e58becb6))

## [0.3.0](https://github.com/yaad-index/yaad-grove/compare/v0.2.1...v0.3.0) (2026-07-11)


### Features

* **core,cmd:** persona layer — PERSONA.md injected into the prompt (ADR 0013) ([#58](https://github.com/yaad-index/yaad-grove/issues/58)) ([038bb74](https://github.com/yaad-index/yaad-grove/commit/038bb7462548c64360297c21463c7835f79e7a98))
* **core:** inject recent-conversation context into the prompt (ADR 0014 slice 2) ([#60](https://github.com/yaad-index/yaad-grove/issues/60)) ([68e1bcb](https://github.com/yaad-index/yaad-grove/commit/68e1bcb81b9f8221413f43a3dff89e6916497c38))
* **memory:** bounded per-conversation buffer + injection selection (ADR 0014 slice 1) ([#59](https://github.com/yaad-index/yaad-grove/issues/59)) ([0d87a2f](https://github.com/yaad-index/yaad-grove/commit/0d87a2f93fdb10cce50406ba8a9a577e6ec9282a))
* wire conversation memory end to end (ADR 0014 slice 3) ([#61](https://github.com/yaad-index/yaad-grove/issues/61)) ([35a1a60](https://github.com/yaad-index/yaad-grove/commit/35a1a601830e2299340f3923f2962f05572cb33b))


### Bug Fixes

* **telegram:** render Markdown as HTML so formatting shows ([#53](https://github.com/yaad-index/yaad-grove/issues/53)) ([#54](https://github.com/yaad-index/yaad-grove/issues/54)) ([ea00ebb](https://github.com/yaad-index/yaad-grove/commit/ea00ebba6499005a51e74c575eff7cd03be85ec2))

## [0.2.1](https://github.com/yaad-index/yaad-grove/compare/v0.2.0...v0.2.1) (2026-07-11)


### Bug Fixes

* **runtime:** route admin DM consent commands to the consent flow ([#50](https://github.com/yaad-index/yaad-grove/issues/50)) ([#51](https://github.com/yaad-index/yaad-grove/issues/51)) ([7edd7ae](https://github.com/yaad-index/yaad-grove/commit/7edd7ae485be9854f46977ad6f489f44d48eafd2))

## [0.2.0](https://github.com/yaad-index/yaad-grove/compare/v0.1.0...v0.2.0) (2026-07-11)


### Features

* **runtime,telegram:** reaction-mode consent nudge (ADR 0012 unit d-reaction) ([#49](https://github.com/yaad-index/yaad-grove/issues/49)) ([8bf86e4](https://github.com/yaad-index/yaad-grove/commit/8bf86e4d302d9230ee98d71723c080d5917bee13))
* **runtime:** /consent remove — self-withdrawal (ADR 0012 unit c) ([#43](https://github.com/yaad-index/yaad-grove/issues/43)) ([c312019](https://github.com/yaad-index/yaad-grove/commit/c3120194c9b1aeef4079bd6620b9064adda64c1a))
* **runtime:** DM consent UI — opt-in button + /start + /consent (ADR 0012 unit b) ([#42](https://github.com/yaad-index/yaad-grove/issues/42)) ([2f251f4](https://github.com/yaad-index/yaad-grove/commit/2f251f416ac5e02edb507a2ce17217d44b372fad))
* **runtime:** surface-split answering + directed-aware group gate (ADR 0012 unit d-core) ([#46](https://github.com/yaad-index/yaad-grove/issues/46)) ([4236dcf](https://github.com/yaad-index/yaad-grove/commit/4236dcf1ab01e1b32fe0468dfa8a33f9553d9027))
* **telegram:** directed-vs-ambient detection (ADR 0012 unit a) ([#41](https://github.com/yaad-index/yaad-grove/issues/41)) ([383a7ec](https://github.com/yaad-index/yaad-grove/commit/383a7ecd79fb55fc4d742f2bf3a56bb044681dc4))


### Bug Fixes

* **runtime:** quarantine log is group-only (BUG-3) ([#38](https://github.com/yaad-index/yaad-grove/issues/38)) ([15ae56a](https://github.com/yaad-index/yaad-grove/commit/15ae56ae26b817b444b9af7cd9d59dd74b1e9f81))
* **telegram:** drop the pre-online backlog on startup (BUG-2) ([#37](https://github.com/yaad-index/yaad-grove/issues/37)) ([2093111](https://github.com/yaad-index/yaad-grove/commit/20931112e6cede3d3029184de7b8c35ab720b589))

## 0.1.0 (2026-07-11)


### Features

* **acl:** consent gate flow + bbolt store ([#4](https://github.com/yaad-index/yaad-grove/issues/4)) ([716d5c8](https://github.com/yaad-index/yaad-grove/commit/716d5c835a1836b812d9daf1ef6d7b48ac5f8a63))
* **budget:** global spend ceiling — metered, fail-safe cost backstop ([#2](https://github.com/yaad-index/yaad-grove/issues/2)) ([93a66d2](https://github.com/yaad-index/yaad-grove/commit/93a66d22a86a30f1ccd2b78308af081028f2f8a5))
* **engine:** Answer wiring + metered-model decorator ([#11](https://github.com/yaad-index/yaad-grove/issues/11)) ([114e298](https://github.com/yaad-index/yaad-grove/commit/114e298223bb4547acac13f370f939d323dde7e8))
* **model:** OpenAI-compatible chat adapter surfacing token usage ([#7](https://github.com/yaad-index/yaad-grove/issues/7)) ([4166774](https://github.com/yaad-index/yaad-grove/commit/4166774121d479df243f685a87a0efbee9da0191))
* **retrieval:** full-text search over the curated vault ([#9](https://github.com/yaad-index/yaad-grove/issues/9)) ([87462b9](https://github.com/yaad-index/yaad-grove/commit/87462b942ce69e76aef29f5c01f33e271068e064))
* **runtime:** request handler composing the consent gate with the engine ([#13](https://github.com/yaad-index/yaad-grove/issues/13)) ([c0bf7e4](https://github.com/yaad-index/yaad-grove/commit/c0bf7e4028b1cfbab2634d1c0ec9b74c1b03af54))
* **transport:** swap Telegram to go-telegram/bot at text parity ([#18](https://github.com/yaad-index/yaad-grove/issues/18)) ([ed8305b](https://github.com/yaad-index/yaad-grove/commit/ed8305bc839c63517bac107ab5893d057902b36e))
