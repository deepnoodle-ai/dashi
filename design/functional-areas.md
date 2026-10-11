# Functional areas

These are the functional areas Dashi's design must account for across the
foundation and its optional integrations. They establish coverage; public names,
package boundaries, priorities, and implementation sequence remain open.

## Areas

**1. Agent definition and configuration.** How agent behavior is described,
configured, reused, and changed.

**2. Model interaction.** How models are selected and called, and how provider
capabilities and responses are handled.

**3. Tool definition and execution.** How tools are described, made available,
invoked, and connected to their results.

**4. Execution lifecycle and control.** How work begins, progresses, waits,
stops, and continues.

**5. Context construction and management.** How information is selected,
organized, and adapted for each model call.

**6. Conversation and session continuity.** How related interactions share
history and state over time.

**7. Execution history and recovery.** How execution is recorded, persisted,
inspected, and recovered after interruption.

**8. Policy, authorization, and human interaction.** How behavior is governed
and people participate through permissions, questions, and decisions.

**9. Content and outputs.** How text, media, structured results, and artifacts
are represented and exchanged.

**10. Streaming and observability.** How callers follow live output, understand
progress, and diagnose behavior.

**11. Resource limits and accounting.** How resource consumption is measured,
attributed, and constrained.

**12. Delegation and background work.** How work is delegated, coordinated, and
completed beyond the immediate execution.

**13. Integration and interoperability.** How the library connects with
external tools, agents, clients, and protocols.

## Research basis

The [primary conceptual model][model] supplies the starting point; the
[companion analysis][analysis] broadens it. [Integrator feedback][feedback]
grounds the responsibilities in application needs. [Framework
comparisons][survey] and [ACP research][acp] inform integration boundaries.
These references point to the temporary local research snapshot.

Current area designs: [turns and their record](turn.md) and
[agent and loop](agent-loop.md).

Consumer review: [Swarm integration and adoption recommendations](swarm-integration/README.md)
explains where Dashi could reduce application code in Swarm.

[model]: ../tmp/research/design/2026-09-26-dive-v2-conceptual-model.md
[analysis]: ../tmp/research/design/2026-09-26-dive-v2-conceptual-design.md
[feedback]: ../tmp/research/design/2026-09-26-dive-v2-integrator-feedback.md
[survey]: ../tmp/research/comparison/README.md
[acp]: ../tmp/research/acpresearch/README.md
