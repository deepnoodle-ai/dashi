# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

## [0.1.1] - 2026-10-01

### Changed

- **Installable CLI.** `go install github.com/deepnoodle-ai/dashi/cmd/dashi@latest`
  now works. A committed `go.work` replaces the sub-modules' `replace`
  directives, which `go install` refuses. (#163)
- **Release tooling.** `make release-prep`, `release-notes`, `tag-modules` and
  `release-publish` cut a release of all four modules together. (#162)

## [0.1.0] - 2026-10-01

### Added

- **Command identity and JSON codec.** `CommandIDOf`, `CommandKindOf`,
  `MarshalCommand` and `UnmarshalCommand` give stored commands a public
  identity and a lossless JSON format. (#157)
- **Turns without a model client.** `Cancel`, `Settle` and `Deliver` run
  without one, and an already-closed `WithPause` admits a Turn without running
  it. (#155)
- **`llm.ModelInfo.CanBoundCost`.** (#155)
- **Per-attempt spend.** `TurnView.Attempts` reports spend per attempt, and
  `AttemptSpendOf` settles it inside a recorder. (#156)
- **`ProjectHistory`** builds history across Turns. (#158)
- **A guide to running Turns without a `Session`.** (#160)

### Changed

- **One compaction path.** `Compactor.Start` is the only way to compact; the
  uncharged summarizer is removed. (#158)
- **CLI.** Quieter resource warnings and a readable muted text color. (#159)
