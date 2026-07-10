# Changelog

All notable changes to `llmux` are documented here.

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [Unreleased]

### Added
- **Sovereignty gate** — inference runs on your box by default; a default-deny
  egress policy (`core/sovereign`) gates every dispatch path (chat, streaming,
  embeddings, semantic-cache embedder, and all modality routes) before any
  network call. Off-box providers are blocked unless explicitly opted in per
  provider (`allow_egress`, `allow_brokered`, or `"tier": "sovereign"`). Fails
  closed; permitted off-box calls are logged with their tier; `/health`
  discloses the posture. Documented in README, `docs/architecture.md`,
  `docs/configuration.md`, and `SECURITY.md`.

### Security
- Closed a sovereignty-gate bypass: the semantic-cache embedder called the
  provider directly, so a remote embed model could silently egress prompt text
  on every request. It now enforces the same egress gate (a blocked embed model
  is a cache miss, never an off-box call). Regression tests added.

## [0.1.0] — 2026-06-28

Initial release.
