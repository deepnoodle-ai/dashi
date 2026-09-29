# PRD: child turns and a delegate tool

**Roadmap item 13 · Status: first slice approved for build (revised 2026-09-28).** Based on [source research](13-child-turns-research.md). Program deadline: demo on 2026-09-30. Ship the first slice by Tuesday night.

## User problem

A developer wants a parent agent to hand a focused task to a sub-agent, show that work live, and use the answer. Two people feel the gap.

- **The terminal user.** The agent must search a large codebase. Every file it reads fills the main context. The user wants an "explore" sub-agent that reads the files and returns a short answer.
- **The Dashi host author.** Dashi can run two agents today. The record does not say which parent tool call caused a child turn. After a crash the host cannot tell whether it started a second, billable child. It must rebuild child cost and decide what a cancel does.

Without this work, every host writes its own parent link, its own child ID rule, and its own cost sum. Each one gets a different answer.

## Goals and success measures

1. A parent turn can call one ready-made tool, `task`. The tool runs a child turn and returns the child's answer. It needs no host code beyond one line.
2. The record links each child to its parent turn and tool call. The link cannot change.
3. A retry after a crash never starts a second child.
4. The spend report shows parent, child, and inclusive totals. It never mixes child model cost into tool cost.
5. Cancel reaches a running child.

Measures (all are tests): parent link survives replay and rejects a change; a retry runs the child model zero extra times; cancel ends both turns; inclusive spend equals parent plus child, counted once.

Guardrail: turns without a parent behave and record exactly as before.

## Users

- **Primary:** the `dashi` CLI user (see the program plan). The CLI wires the tool in one call and shows nested steps.
- **Secondary:** any Dashi host that needs recoverable delegation.

## What the user meets

Terminal screen (the CLI draws it; the library supplies the events):

```
> find where config is parsed
  ▸ explore: find where config is parsed
      read  config/load.go
      grep  "func Parse"
  ◂ explore done (4 steps)
The parser is in config/load.go, function Parse.
total $0.41 (child agents $0.12)
```

Host code (public API):

```go
// package delegate
type Subagent struct {
    Name        string       // what the model passes as "subagent", e.g. "explore"
    Description string       // tells the model when to choose it
    Agent       dashi.Agent  // instructions, model, and limits for the child turn
    Tools       []string     // names allowed from the parent's toolset; empty = all except "task"
}

type Config struct {
    Subagents []Subagent   // empty = Explore and General
    Runner    dashi.Runner // child template: Client, Recorder, Hooks
    MaxAnswer int          // bytes returned to the parent; zero = 16 KiB, negative = no cap
    OnChild   func(ChildEvent) // live display; the only event path
}

func New(cfg Config, parentTools tool.Toolset) tool.Toolset // the "task" tool
func ChildID(parentTurnID, callID string) string
func Report(ctx context.Context, r dashi.Loader, parent dashi.TurnView) (SpendReport, error)

type ChildEvent struct {
    Parent   dashi.CallRef // parent turn and tool call
    Subagent string
    Event    dashi.Event   // the child's own event; Event.TurnID is the child
}
type SpendReport struct{ Parent, Children, Inclusive dashi.Spend }
```

Core additions (package `dashi`):

```go
type CallRef struct{ TurnID, CallID string } // one tool call in one turn

type Start struct { /* existing fields */ Parent *CallRef }
type StartFact struct { /* existing fields */ Parent *CallRef `json:"parent,omitempty"` }
type TurnView struct { /* existing fields */ Parent *CallRef }

// CurrentCall returns the turn and tool call that an executor is serving.
func CurrentCall(ctx context.Context) (CallRef, bool)

func (s Spend) Add(o Spend) Spend // sums usage, cost, tool cost, and counters
```

## Main flows

**Delegate.** The model calls `task{subagent, prompt, description}`. The executor reads `CurrentCall`. It computes `ChildID`. It loads that turn. If the turn does not exist, it starts one with `Start.Parent`. If the turn exists and has not finished, it continues it. If the turn finished, it returns the saved answer. Then it returns the child's answer as the tool result.

**Retry after a crash.** The `task` tool is marked idempotent, so the runner runs it again. The ID rule finds the same child. The child model calls that already settled do not run again.

**Cancel and interrupt.** The child runs under the tool's context. When the parent context ends under `ContextCancel`, the child records `StopCanceled`, and the tool settles the call as an error that says "canceled". Under `ContextInterrupt` (the default), the child records an interrupt and the tool does not settle the call. The parent call stays open and the parent stops as interrupted. On resume, the runner runs `task` again and the same child continues.

**Live display.** The tool sets the child's `Observer` itself and sends every child event to `OnChild`: committed steps, model deltas, and tool progress. There is no other path. If the template `Runner` has an `Observer`, the tool calls it too, after `OnChild`.

## Functional requirements

- FR-1: `Start.Parent` is optional. When set, both IDs must be nonempty valid UTF-8. Dashi records it in `StartFact` and shows it in `TurnView`.
- FR-2: A repeated `Start` with a different `Parent` is a command conflict. Replay shows the same link.
- FR-3: Dashi does not check that the parent call exists. The host owns that check.
- FR-4: The runner puts the current turn ID and call ID in the executor context. `CurrentCall` reads them.
- FR-5: `ChildID` is a pure function of its two arguments. Two calls with the same arguments return the same ID. Different pairs never collide.
- FR-6: The `task` tool schema has `subagent` (enum of catalog names), `prompt`, and `description` (short label for display).
- FR-7: The child sees only `prompt` and its own instructions. It never sees the parent transcript.
- FR-8: The child toolset is the parent toolset filtered by `Subagent.Tools`. It never includes `task`. A child cannot start a child.
- FR-9: The child uses `Config.Runner`. The host passes the same `Hooks` it gives the parent, so the host's inline approval prompt covers every child tool call. The hook sees the child's `TurnView`. `TurnView.Parent` is set, and the agent name identifies the subagent, so the prompt can say "explore wants to run: go test". Two parallel children can prompt at once; the host serializes its prompts.
- FR-10: The tool returns the child's final answer as text, cut to `MaxAnswer`. The tool says when it cut the text.
- FR-11: The tool settles the call as an error only when the child stops `failed`, `limited`, `refused`, or `canceled`. The error text names the stop reason. An interrupted child never settles the call (see Cancel and interrupt).
- FR-11a: If the child stops `waiting` for a deferred approval, the tool settles the call as an error that says: "subagent needs approval that was deferred; this host cannot answer it inline". The child stays resumable by its ID.
- FR-12: Parent calls to `task` can run in parallel. Each has its own child ID.
- FR-13: Zero `Config` works: `Explore` (only tools with `Annotations.ReadOnly` set, so MCP tools filter correctly) and `General` (all tools). `Subagent.Agent.Limits` bounds the child's work; no new timeout setting exists.
- FR-15: The tool needs a `Config.Runner.Recorder` that implements `dashi.Loader`. If it does not, `New` still works. It logs one warning, does not mark `task` idempotent, and documents the weaker guarantee: after a crash the runner does not re-run the call blindly, and a host can start a second child.
- FR-14: `Spend.Add` keeps `Cost`, `ToolCost`, `Estimated`, `CostUnknown`, `CostPartial`, and `UnpricedRequests` separate. `Report` never writes child cost into `ToolCost`.

## User stories and acceptance

- **US-1 Parent link.** As a host author, I read the parent link from any child record.
  - [ ] `Start{Parent}` persists. `Replay` and a recorder reload return it.
  - [ ] A second `Start` with a different parent returns a command conflict.
  - [ ] A root turn records no `parent` field.
- **US-2 Retry safety.** As a host author, I restart after a crash and pay once.
  - [ ] A test crashes before child start, after child start, and after child completion. Each retry leaves one child turn.
  - [ ] A retry after completion makes zero new model calls.
  - [ ] With a recorder that is not a `Loader`, `New` warns once and `task` is not idempotent.
- **US-3 Cancel.** As a CLI user, I press Ctrl-C and both agents stop.
  - [ ] A test cancels the parent context during a child model call under `ContextCancel`. Both records show `canceled`.
  - [ ] A test interrupts during a child model call under `ContextInterrupt`, then resumes the parent. The child continues. No second child exists. No settled child model call repeats. The parent records no failed `task` result.
- **US-4 Spend.** As a CLI user, I see what the child cost.
  - [ ] `Report` counts a resumed child once, with partial and unknown-price flags preserved.
  - [ ] Parent `TurnView.Spend` does not include the child.
- **US-5 Live view.** As a CLI user, I see the child's steps.
  - [ ] `OnChild` receives child steps, deltas, and progress, each with the parent link. A template `Observer` also receives them.
  - [ ] A child tool call passes through the host's `BeforeTool` hook, and the hook can read the subagent name.
  - [ ] A child answer over `MaxAnswer` is cut and marked.

## Out of scope (later work)

- **Durable external settle.** The parent records a pending call. A separate worker runs the child. The host settles the call. The link and ID rule in this slice already support it. Build it when a host needs it.
- **Family budget cap.** `Budget` stays per turn. A cap over parent plus child needs a host ledger (`Hooks.BeforeSend` with `SendDecision.Admission`). A later slice adds a reference ledger.
- **Child index.** The `Recorder` loads by ID only. A "list children of X" query waits for a proven need.
- **Subagent files.** Loading `explore`-style subagents from `.dashi/agents/*.md` belongs in the CLI or the skills track.
- **Background children, grandchildren, resume by task ID from the model, worktree isolation.**
- **Approval that waits.** A child that stops for a deferred approval fails the call (FR-11a). Durable approval inside children comes with the external-settle slice.
- **Host-started children in `Report`.** `Report` finds children only through `task` calls. A child that a host starts with its own ID does not appear. The guide states this.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Two turn records are not one transaction | A crash can leave a parent call open and a child finished | Child ID is stable. The tool is idempotent. The tests cover each crash point. |
| Idempotent mark hides a real repeat | A child tool with side effects can run twice after a crash | The child's own record stops repeated model calls. Non-idempotent child tools follow the runner's existing recovery rule. Document it in the guide. |
| Idempotent re-run without a `Loader` | Second paid child | FR-15 withholds the idempotent mark and warns. |
| Child cancel reports success too early | User thinks work stopped | Report `canceled` only when the child record shows a stop. |
| Answer cap hides needed detail | Parent lacks facts | Default 16 KiB. The tool marks a cut. The host can raise the cap. |

## Decisions

1. **Ship the synchronous `task` tool first, not the external-settle flow.** The CLI needs a blocking tool by Wednesday. The link and ID rule serve both paths.
2. **Child ID is derived from `(parent turn ID, call ID)`.** The host stores nothing. A host that wants its own ID can call `Start` itself.
3. **Mark `task` idempotent, only with a `Loader` recorder.** The re-run resumes the same child. Without a `Loader` the tool warns once and stays non-idempotent (review finding 2, accepted). Dashi warns and adjusts; it does not fail.
4. **An interrupted child does not settle the call.** The parent call stays open, the parent stops interrupted, and resume continues the same child. Only `failed`, `limited`, `refused`, and `canceled` children settle an error (finding 1, accepted).
5. **One type, `CallRef`, and `CurrentCall(ctx)`.** "Origin" already means who supplied content, so it is not reused (finding 4, accepted). `Start.Parent` is a `*CallRef`.
6. **The user-facing word is "subagent".** The type is `delegate.Subagent`. It wraps a `dashi.Agent`, so `Agent` keeps one meaning and each subagent gets its own model and limits at no cost (finding 3, accepted; per-agent model leaves "out of scope").
7. **The tool is named `task`.** Correction: Dive names its tool `Agent`, not `task`. `task` is the name coding-agent users know for launching a subagent, and models see it often. `agent` would also clash with the `dashi.Agent` type in prompts and docs. The parameter is `subagent` (finding 7, accepted).
8. **Child cost stays out of `Outcome.Cost`.** `Report` sums `Spend` and keeps every flag.
9. **No child inheritance.** The child gets a prompt, instructions, and a filtered toolset, and no transcript. Dive also hides its `Agent` tool from children.
10. **One event path: `OnChild`.** `delegate` owns the child `Observer`, chains the template's, and drops `tool.Report`. `OnChild` gets steps, deltas, and progress (finding 5, accepted).
11. **Child tool calls use the host's hooks.** The host passes the parent's `Hooks` to the child template. The CLI's inline prompt names the subagent. The CLI track must run its prompt inside `BeforeTool`, not as a deferred approval. A deferred approval in a child fails the call with a clear message (finding 6, accepted; the coordinator confirms the CLI side).
12. **`Explore` means tools with `Annotations.ReadOnly`.** MCP tools then filter correctly (finding 7, accepted).
13. **Host-started children stay out of `Report`; no child index; no timeout setting.** The guide states the limit. `Subagent.Agent.Limits` bounds the child (open decisions, accepted).
14. **Cancel follows the child runner's `OnContextDone`.** The CLI sets `ContextCancel`. Library hosts keep the default.

## Open questions

None block the first slice. Later: whether a child index belongs in `Recorder`, and whether a parent cancel should also cancel children that run outside the tool's context.

## Deliverables

Core fields and `CurrentCall`; `delegate` package; tests listed above; `examples/delegate`; `docs/guides/delegation.md`; update `design/remaining-work.md` and the packet index.
