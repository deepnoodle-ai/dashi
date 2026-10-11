# Making the coding CLI and voice APIs easier to embed

See the [main recommendations](README.md). These are separate ways applications
use Dashi. The CLI has a clear simplification opportunity; the voice additions
need more proof of reuse.

## R27 — let a program send commands to the running CLI

**Problem.** [Swarm][cli] pastes prompts into the terminal, escapes `@` so it is
not treated as a mention, reverses that escaping in events, sends Escape to
interrupt, and clears the editor with several control-key sequences. Clearing
must avoid quitting or accidentally interrupting work.

**Change.** Add a local JSON command pipe or socket to the interactive CLI for
literal prompt submission, explicit attachments, additional input, interruption,
and readiness checks. Keep the terminal interface for people. The existing
one-shot `dashi run` does not control an already-running interactive session.

Use request IDs and return acceptance/rejection for the original request, even
when no Turn starts. Retrying an accepted request must not create duplicate
work. Restrict access to the intended local session and preserve approvals.
Choose the final transport after inspecting relevant existing protocols.

**Benefit:** delete terminal editing and mention escaping as program-control
mechanisms. Cost: Dashi would maintain a new control protocol.

## R28 — tell the caller which commands and events are supported

**Problem.** Swarm checks `--help` for `--turn-events` and parses hook-shaped
events. Dashi already has `--version` and turn events, but the caller still
makes assumptions about their meaning and compatibility.

**Change.** Add machine-readable capabilities: build version, protocol version,
modes, and supported commands/events. Provide a small Go client or shared schema
outside `package main` so adopters do not rewrite parsing.

Keep “the Turn ended” and “the CLI is ready for another prompt” explicit. Dashi's
current end event intentionally waits for safe input; preserve that guarantee.
New events could report queued/rejected input, approval/question waits, and
truncated text. Keep existing hook events for their consumers.

**Benefit:** remove help scraping and guesswork during upgrades. Similar-looking
hook records should not imply compatibility with another CLI's whole protocol.

## R29 — simplify packaging the CLI for remote machines

**Problem.** [Swarm][binary] cross-compiles and verifies its own Linux Dashi
bundle. It pins CLI 0.5.0 while its Go library uses 0.6.0. Separate versions may
be intentional, but each consumer maintains packaging and compatibility glue.

**Change.** Supply release binaries where appropriate, or a reproducible private
packaging command. Emit a manifest with version, platform, size/hash, and control
capabilities. Test the command protocol rather than requiring identical library
and CLI version numbers.

**Benefit:** easier deployment and upgrades. Swarm still pins/verifies binaries
and controls installation and caching. Public distribution is a business choice.

## R30 — reduce routine realtime connection bookkeeping

**Problem.** [Swarm's voice writer][voice] serializes sends, bounds queued audio,
tracks canceled tools, and decides when the provider needs another response.
It waits while the user speaks or previous audio is still playing. Dashi's
realtime Session supplies the lower-level transport.

**Change.** Consider an optional helper for serialized bounded writes, tool-result
matching, cancellation, and response readiness. Let the application report user
speech and playback completion. Provide examples for these transitions while
keeping the raw Session available.

**Benefit:** potentially remove reusable connection code. This is a candidate,
not a demonstrated large deletion. Swarm keeps microphones, playback, five-minute
limits, quotas, client ownership, message identity, agent permissions, and its
one-active-session rule. A provider finishing audio is different from a listener
finishing playback.

## R31 — expose the model and voice choices Dashi already knows

[Swarm's voice configuration][voices] duplicates provider model IDs and voice
lists. Publish those choices and capabilities beside Dashi's provider adapters,
with validation matching Connect. Include audio formats, tool support, response
behavior, and truncation support. Swarm can add labels and credential readiness.

This is a small convenience improvement. Also document realtime usage and
pricing coverage. Swarm accounts for voice separately today. A unified dollar
budget needs reliable pricing and spending bounds; usage events alone do not
provide that guarantee.

[cli]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/codingcontrol/cli/dashi.go#L3
[binary]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/codingartifacts/dashi.go#L3
[voice]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/manager/voice/service.go#L384
[voices]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/config/voice.go#L11
