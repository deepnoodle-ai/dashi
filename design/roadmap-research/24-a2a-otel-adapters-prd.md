# 24. A2A and OpenTelemetry adapters — draft PRD

**Status:** proposal for review; no API approved. **Size/difficulty:** M each / medium ([roadmap](../remaining-work.md)). **References:** [research](24-a2a-otel-adapters-research.md), [Turn record](../turn.md), [live delivery](../live-delivery.md), Dive's `a2a` and `otel` modules. Implement and qualify OTel first; A2A follows a confirmed host pilot.

## User problem and priority

Nvoken currently exports useful GenAI traces through Dive. A move to Dashi should preserve trace correlation and the default content-free boundary without making traces part of Turn recovery. Separately, an application may want to expose a Dashi agent through A2A so another agent can discover it, watch a task, supply requested input, or cancel it. Dive demonstrates the appeal of a small adapter, but Dashi's durable Turn must remain the source of task status after disconnects and worker replacement. Nvoken is the proposed A2A server pilot and Mobius a possible consuming client; the Mobius A2A reference is an **obsolete proposal**, not evidence of a current requirement. Confirm one actual cross-service use case before shipping the A2A public API.

## Goals and non-goals

- Keep A2A and OTel in separate optional Go modules, with no protocol SDK or OTel SDK dependency in Dashi's root module. Use the official A2A Go SDK for transport/card/task wire behavior rather than hand-writing the protocol.
- Treat committed Dashi steps as execution truth. A2A task state, output artifacts, pending input, and cancellation derive from `Replay`/`TurnView`; OTel spans describe observed attempts and may be incomplete.
- Make the common setup small: a host supplies its existing Runner, Recorder/task lookup, authorization, and A2A card; an OTel host supplies a tracer provider and installs one tracer. No generic adapter registry.
- Keep prompts, user messages, tool arguments/results, credentials, and raw Step JSON out of both exports by default. A2A emits only content the host explicitly authorizes for that caller.

Out of scope: A2A federation/discovery marketplace, generic agent identity or OAuth server, every A2A transport/version, a Dashi A2A client in the first slice, OTel as a journal or billing source, automatic prompt/tool payload capture, and an OTel exporter managed by Dashi. A client can use the official SDK for the pilot.

## Proposed smallest user flow and module boundaries

**1. `dashi/otel` first, with Nvoken as the host.** Port Dive's `invoke_agent`/`chat`/`execute_tool` shape and GenAI token/operation metrics, but attribute them to Dashi's actual Turn, attempt, model intent/settlement, tool start/settlement, and stop. Add only a narrow, optional core tracing seam needed to start/end spans and propagate the child `context.Context` into `llm.Client` and `tool.Tool` calls. `Runner.Observer` delivers committed steps but can drop previews and is asynchronous, so it cannot be the sole instrumentation path. The tracer must not authorize, commit, change a stop, or block recovery if export fails. Default attributes: model/provider, operation, counts, outcome, Turn ID, attempt ID, step sequence, and bounded error class. Payload capture, if ever added, needs a later explicit design. Keep the caller's active HTTP trace as parent for one `Runner.Run`; resumed runs receive new spans linked by Turn ID, not an invented continuous span. Nvoken can keep its current OTLP provider, attribute allowlist, and authorized trace-read APIs while swapping Dive's tracer factory for the Dashi adapter.

Illustrative API shape (names are not committed):

```go
runner := dashi.Runner{Agent: agent, Recorder: recorder,
    Tracer: dashiotel.New(dashiotel.Options{Tracer: provider.Tracer("dashi")})}
outcome, err := runner.Run(ctx, dashi.NewTurn(turnID), dashi.Start{Input: input})
```

**2. `dashi/a2a` server pilot after host validation, using merged live delivery.** Expose one Nvoken managed agent through a host-owned authenticated A2A endpoint and agent card. The adapter accepts the A2A SDK's task/context IDs, resolves an authorized Dashi Turn and host Runner, and persists the ID mapping alongside the Turn/task store. For a new request, durably establish identity before execution. For `GetTask`, load the Turn and project committed status/artifacts; on stream reconnect, read acknowledged steps through the host `StepReader` and Dashi `Follow` cursor; provisional text may appear as labeled progress but cannot advance durable task state. `waiting` maps to `input-required` with one identified pending input/approval; `completed`, `canceled`, and `failed` map to their matching task states. `limited` reports failure with the limit reason. `interrupted` remains resumable and must not be reported completed; the host decides whether to reschedule it or return a retriable service condition. A2A `CancelTask` reports canceled only after the Dashi cancellation is durably acknowledged. Require structured call-ID responses for multiple pending approvals; do not broadcast one text answer to them. The host owns authentication, tenant scoping, card visibility, and any cross-service billing.

Illustrative host flow, not a committed constructor:

```go
handler := dashia2a.NewServer(dashia2a.Options{
    Resolve: authorizedRunnerAndTurn, Tasks: durableTaskMapping,
    Card: approvedAgentCard,
}).Handler()
```

## Acceptance criteria — OTel

- A Nvoken integration fixture swaps Dive's tracer for Dashi's adapter and gets one `invoke_agent` span per live `Run`, child `chat` spans per dispatched model attempt, and `execute_tool` spans for locally executed calls. Parent context reaches instrumented provider HTTP and tool calls. A resumed Turn has stable `dashi.turn.id` correlation without claiming one span survived the gap.
- Attributes distinguish committed `model_requested` from `model_settled`, tool started/waiting/settled, interruption, and final stop. Usage/cost attributes come from settled Dashi facts and retain unknown/partial status; replay alone emits no new charge metrics or duplicate execution spans.
- Export disabled/unavailable/throwing does not change committed steps or outcome. Default exported spans contain no prompt, system instruction, message body, tool argument/result, secret, or raw Step payload. A test inspects exported attributes/events, not just configured flags. Nvoken's content-free exporter remains an independent defense at the egress boundary.
- The optional module can be omitted with no OTel dependencies in the root graph; a runnable local collector example and a short Nvoken migration note show provider lifecycle/shutdown and trace lookup.

## Acceptance criteria — A2A

- A small Nvoken pilot using the released A2A Go SDK serves an authenticated card and one selected agent. Create/get/stream task operations return IDs and status consistent with the durable Turn. A2A task ID, context ID, tenant, and Turn ID mapping survives process restart; mismatched IDs or unauthorized task access are rejected before loading private content.
- A completed output becomes one stable artifact; `waiting` identifies the pending call; a valid response settles exactly that call and continues once. Ambiguous plain text for multiple pending calls is rejected. A canceled task is reported canceled only after a committed Dashi cancel. Failed, limited, and interrupted outcomes never become completed artifacts solely because partial text exists.
- A second worker can load the Turn after disconnect, replay current task status, and resume streaming from a committed `Follow` cursor plus new live updates without re-executing prior effects. Duplicate messages and uncertain persistence results reconcile through the recorded identity/command rules; the A2A store never overrides a conflicting Turn fact.
- Protocol conformance tests use the selected SDK/version for card, send/get/stream/reconnect, input-required continuation, cancellation, and task/context mismatch. The first public docs state supported transport/version and content modes explicitly; unsupported artifact/data modes fail clearly.

## Risks and open decisions

OTel GenAI semantic conventions evolve; pin the adapter's emitted convention version and test its attribute mapping before upgrades. Stable Turn IDs and high-cardinality user/tenant attributes need an explicit Nvoken privacy and backend-cardinality review. A2A's task store and Dashi Recorder can drift unless one mapping owner is chosen and persistence is coordinated. Confirm Nvoken's desired public-agent exposure and a real Mobius/client task before the A2A slice; if neither host needs it, keep the research and defer implementation. The precise A2A treatment of an interrupted Turn depends on the host's durable rescheduler, so the adapter must not promise automatic progress without one.
