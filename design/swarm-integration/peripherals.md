# Making the coding CLI and voice APIs easier to embed

See the [main recommendations](README.md). These are separate ways applications
use Dashi. The CLI has a clear simplification opportunity; the voice additions
need more proof of reuse.

## R27 — let a program send commands to the running CLI

[Before and after](examples/r27-cli-commands.md).

**Problem.** [Swarm][cli] pastes prompts into the terminal, escapes `@` so it is
not treated as a mention, reverses that escaping in events, sends Escape to
interrupt, and clears the editor with several control-key sequences. Clearing
must avoid quitting or accidentally interrupting work.

**Change.** Add a local JSON command pipe or socket to the interactive CLI for
literal prompt submission, explicit attachments, additional input, interruption,
and readiness checks. Keep the terminal interface for people. The existing
one-shot `dashi run` does not control an already-running interactive session.

Use durable session/request identities. Acceptance follows persistence; lost
acknowledgment is unknown, not rejection. Query/retry the same ID and payload
after reconnect/restart; conflicting reuse fails. SubmitNewTurn, Steer and Enqueue
must be distinct operations; stale readiness cannot change their meaning. Restrict access to the intended local session and preserve approvals.
Compare the planned [ACP adapter](../roadmap-research/23-acp-adapter-prd.md) with
a small command port. Share underlying [session input operations](../session-input.md)
rather than introducing another submit interpretation.

**Benefit:** delete terminal editing and mention escaping as program-control
mechanisms. Cost: Dashi would maintain a new control protocol.

## R28 — tell the caller which commands and events are supported

[Before and after](examples/r28-cli-capabilities.md).

**Problem.** Swarm checks `--help` for `--turn-events` and parses hook-shaped
events. Dashi already has `--version` and turn events, but the caller still
makes assumptions about their meaning and compatibility.

**Change.** Add machine-readable capabilities: build version, protocol version,
modes, and supported commands/events. Provide a small Go client or shared schema
outside `package main` so adopters do not rewrite parsing.

Keep “the Turn ended” and “the CLI is ready for another prompt” explicit. Dashi's
current end event intentionally waits for safe input; preserve that guarantee.
New events could report queued/rejected input, approval/question waits, and
truncated text. Keep existing hook events. Reconnect needs a snapshot/event cursor and request
correlation. Readiness enables UI, while admission can still reject busy work.

**Benefit:** remove help scraping and guesswork during upgrades. Similar-looking
hook records should not imply compatibility with another CLI's whole protocol.

## R29 — simplify packaging the CLI for remote machines

[Before and after](examples/r29-cli-packaging.md).

**Problem.** [Swarm][binary] cross-compiles and verifies its own Linux Dashi
bundle. It pins CLI 0.5.0 while its Go library uses 0.6.0. Separate versions may
be intentional, but each consumer maintains packaging and compatibility glue.

**Change.** Supply release binaries where appropriate, or a reproducible private
release/dev packaging tool outside the runtime CLI. Emit a manifest with version, platform, size/hash, and control
capabilities. Test the command protocol rather than requiring identical library
and CLI version numbers.

**Benefit:** easier deployment and upgrades. Swarm still pins/verifies binaries
and controls installation and caching. Keep Swarm's artifact loader/verifier. Public distribution is a business choice;
byte reproducibility requires pinned tooling and separate verification.

## R30 — test a smaller voice response-readiness policy

[Before and after](examples/r30-voice-response.md).

[Swarm's voice writer][voice] tracks speech, playback, tools and pending response.
Existing Session.Send already serializes writes, validates pending tool IDs/names,
and blocks Respond during active responses/pending tools. The new policy is
waiting for user speech and actual playback completion.

**Defer NewController.** First document and test a small readiness decision in a
complete event loop. Keep one Receive owner, per-response playback correlation,
canceled-result exclusion, bounded queues and explicit overflow/shutdown behavior.
Use a session-lifetime send context: today's canceled Send context closes the
connection. Provider completion does not imply playback completion.

A pure decision helper may prove useful. It does not remove queue/error/lifetime
code. Swarm keeps devices, quotas, permissions and one-active-session policy.

## R31 — share defaults; define new voice metadata deliberately

[Before and after](examples/r31-voice-options.md).

[Swarm][voices] hard-codes model IDs and its selected voice lists. Dashi exports
DefaultModel constants today, so adopt those first. Its adapters do not currently
export a voice catalog or validate names against one.

**Small addition:** expose adapter defaults/capabilities. A complete catalog and
early selection validation need a maintained policy shared by Connect and the
UI; application subsets are not complete provider catalogs. Decide how to handle
new/unknown valid voices without accidental rejection by stale static metadata.

Realtime pricing and spend bounds need separate qualification. Usage events alone
do not guarantee an enforceable dollar cap.


[cli]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/codingcontrol/cli/dashi.go#L3
[binary]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/codingartifacts/dashi.go#L3
[voice]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/manager/voice/service.go#L384
[voices]: https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/config/voice.go#L11
