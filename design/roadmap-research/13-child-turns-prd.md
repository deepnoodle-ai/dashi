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
type Agent struct {
    Name         string   // what the model passes as "agent", e.g. "explore"
    Description  string   // tells the model when to choose this agent
    Instructions string
    Tools        []string // names allowed from the parent's toolset; empty = all except "task"
}

type Config struct {
    Agents   []Agent      // empty = Explore and General
    Runner   dashi.Runner // child template: Client, Recorder, Hooks, Observer
    MaxAnswer int         // bytes returned to the parent; zero = 16 KiB, negative = no cap
    OnChild  func(ChildEvent) // live display; optional
}

func New(cfg Config, parentTools tool.Toolset) tool.Toolset // the "task" tool
func ChildID(parentTurnID, callID string) string
func Report(ctx context.Context, r dashi.Loader, parent dashi.TurnView) (SpendReport, error)

type ChildEvent struct {
    Parent dashi.ParentCall // parent turn and tool call
    Agent  string
    Event  dashi.Event      // the child's own event; Event.TurnID is the child
}
type SpendReport struct{ Parent, Children, Inclusive dashi.Spend }
```

Core additions (package `dashi`):

```go
type ParentCall struct{ TurnID, CallID string }

type Start struct { /* existing fields */ Parent *ParentCall }
type StartFact struct { /* existing fields */ Parent *ParentCall `json:"parent,omitempty"` }
type TurnView struct { /* existing fields */ Parent *ParentCall }

// CallOrigin returns the turn and call that an executor is serving.
func CallOrigin(ctx context.Context) (ParentCall, bool)

func (s Spend) Add(o Spend) Spend // sums usage, cost, tool cost, and counters
```

## Main flows

**Delegate.** The model calls `task{agent, prompt, description}`. The executor reads `CallOrigin`. It computes `ChildID`. It loads that turn. If the turn does not exist, it starts one with `Start.Parent`. If the turn exists and has not finished, it continues it. If the turn finished, it returns the saved answer. Then it returns the child's answer as the tool result.

**Retry after a crash.** The `task` tool is marked idempotent, so the runner runs it again. The ID rule finds the same child. The child model calls that already settled do not run again.

**Cancel.** The child runs under the tool's context. Cancel of the parent context ends the child run. If the child runner uses `ContextCancel`, the child records `StopCanceled`. The tool then returns a failed outcome that says "canceled". With `ContextInterrupt`, the child stays resumable.

**Live display.** The tool sends each child event to `OnChild`. It also calls `tool.Report` with a short status line, so a host without `OnChild` still sees progress.

**Cost.** `Report` loads each child of the parent's `task` calls by derived ID. It counts each child once. The `task` outcome carries no `Cost`.

## Functional requirements

- FR-1: `Start.Parent` is optional. When set, both IDs must be nonempty valid UTF-8. Dashi records it in `StartFact` and shows it in `TurnView`.
- FR-2: A repeated `Start` with a different `Parent` is a command conflict. Replay shows the same link.
- FR-3: Dashi does not check that the parent call exists. The host owns that check.
- FR-4: The runner puts the current turn ID and call ID in the executor context. `CallOrigin` reads them.
- FR-5: `ChildID` is a pure function of its two arguments. Two calls with the same arguments return the same ID. Different pairs never collide.
- FR-6: The `task` tool schema has `agent` (enum of catalog names), `prompt`, and `description` (short label for display).
- FR-7: The child sees only `prompt` and its own instructions. It never sees the parent transcript.
- FR-8: The child toolset is the parent toolset filtered by `Agent.Tools`. It never includes `task`. A child cannot start a child.
- FR-9: The child uses `Config.Runner`. Its `Hooks` apply, so the host's approval prompt covers child tool calls.
- FR-10: The tool returns the child's final answer as text, cut to `MaxAnswer`. The tool says when it cut the text.
- FR-11: If the child stops for any reason other than completed, the tool returns a failed outcome with the stop reason. The model sees the reason and can retry or continue alone.
- FR-12: Parent calls to `task` can run in parallel. Each has its own child ID.
- FR-13: Zero `Config` works: `Explore` (read-only tools, search and read) and `General` (all tools).
- FR-14: `Spend.Add` keeps `Cost`, `ToolCost`, `Estimated`, `CostUnknown`, `CostPartial`, and `UnpricedRequests` separate. `Report` never writes child cost into `ToolCost`.

## User stories and acceptance

- **US-1 Parent link.** As a host author, I read the parent link from any child record.
  - [ ] `Start{Parent}` persists. `Replay` and a recorder reload return it.
  - [ ] A second `Start` with a different parent returns a command conflict.
  - [ ] A root turn records no `parent` field.
- **US-2 Retry safety.** As a host author, I restart after a crash and pay once.
  - [ ] A test crashes before child start, after child start, and after child completion. Each retry leaves one child turn.
  - [ ] A retry after completion makes zero new model calls.
- **US-3 Cancel.** As a CLI user, I press Ctrl-C and both agents stop.
  - [ ] A test cancels the parent context during a child model call. Both records show a stop. Neither shows `completed`.
- **US-4 Spend.** As a CLI user, I see what the child cost.
  - [ ] `Report` counts a resumed child once, with partial and unknown-price flags preserved.
  - [ ] Parent `TurnView.Spend` does not include the child.
- **US-5 Live view.** As a CLI user, I see the child's steps.
  - [ ] `OnChild` receives child steps in order, each with the parent link.
  - [ ] A child answer over `MaxAnswer` is cut and marked.

## Out of scope (later work)

- **Durable external settle.** The parent records a pending call. A separate worker runs the child. The host settles the call. The link and ID rule in this slice already support it. Build it when a host needs it.
- **Family budget cap.** `Budget` stays per turn. A cap over parent plus child needs a host ledger (`Hooks.BeforeSend` with `SendDecision.Admission`). A later slice adds a reference ledger.
- **Child index.** The `Recorder` loads by ID only. A "list children of X" query waits for a proven need.
- **Agent files.** Loading `explore`-style agents from `.dashi/agents/*.md` belongs in the CLI or the skills track.
- **Per-agent model choice, background children, grandchildren, resume by task ID from the model, worktree isolation.**
- **Approval that waits.** If a child stops waiting for a deferred approval, the sync tool fails the call. The child stays resumable by its ID.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Two turn records are not one transaction | A crash can leave a parent call open and a child finished | Child ID is stable. The tool is idempotent. The tests cover each crash point. |
| Idempotent mark hides a real repeat | A child tool with side effects can run twice after a crash | The child's own record stops repeated model calls. Non-idempotent child tools follow the runner's existing recovery rule. Document it in the guide. |
| Child cancel reports success too early | User thinks work stopped | Report `canceled` only when the child record shows a stop. |
| Answer cap hides needed detail | Parent lacks facts | Default 16 KiB. The tool marks a cut. The host can raise the cap. |

## Decisions

1. **Ship the synchronous `task` tool first, not the external-settle flow.** The CLI needs a blocking tool by Wednesday. The link and ID rule serve both paths, so the durable path stays open.
2. **Child ID is derived from `(parent turn ID, call ID)`.** The host stores nothing. Retry finds the child by rule. A host that wants its own ID can call `Start` itself.
3. **Mark `task` idempotent.** The runner then re-runs it after a crash. The tool resumes the same child, so the re-run is safe.
4. **Add `CallOrigin(ctx)` to the core.** Any tool can then start a linked child. The delegate package is not special.
5. **Child cost stays out of `Outcome.Cost`.** Token counts and price flags survive only in the child's own `Spend`. `Report` sums them.
6. **No child inheritance.** The child gets a prompt, instructions, and a filtered toolset. It gets no transcript. This matches Dive's sub-agents, which also hide the `Agent` tool from children.
7. **Cancel follows the child runner's `OnContextDone`.** The CLI sets `ContextCancel`. Library hosts keep the default (interrupt, resumable).
8. **Live events go through `OnChild` plus `tool.Report`.** Progress is preview-only. The child's record stays the truth.
9. **Naming.** The tool is `task`, the term Dive users know. The package is `delegate`. The link is `ParentCall`.
10. **Roadmap 13 is complete at the first slice.** The remaining items above become roadmap follow-ups.

## Open questions

None block the first slice. Later: whether a child index belongs in `Recorder`, and whether a parent cancel should also cancel children that run outside the tool's context.

## Deliverables

Core fields and `CallOrigin`; `delegate` package; tests listed above; `examples/delegate`; `docs/guides/delegation.md`; update `design/remaining-work.md` and the packet index.
