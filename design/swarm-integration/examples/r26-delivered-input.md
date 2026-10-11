# R26: Acknowledge input saved into the transcript

[All examples](README.md) · [Review](../README.md)

A user sends “Use the new branch” while the agent runs. Swarm saves it as
pending input. When Dashi saves it into the transcript, Swarm must mark that input consumed
in the same transaction. This does not prove a model received it: a hook or
budget check can stop execution before sending.

## Before: current code

From [swarm/internal/runtime/dashirt/recording.go:165–182](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/recording.go#L165). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/recording.go 165 182 -->
```go
for _, key := range keys {
	seq := int64(0)
	for _, step := range turn.Steps {
		if step.Context != nil {
			for _, item := range step.Context.Items {
				if item.ID == key {
					seq = step.Seq
				}
			}
		}
	}
	if seq == 0 {
		return errors.New("steer acknowledgment has no recorded delivery")
	}
	if err = s.runtime.repo.ConsumeModelSteer(ctx, id, key, seq); err != nil {
		return err
	}
}
```

## After: Proposed API sketch

The proposed Dashi helper does not exist yet. Application storage methods
show the responsibilities that remain with the caller.

```go
return repo.Transaction(ctx, func(tx Tx) error {
    disposition, err := tx.CheckCommit(req) // Same new/duplicate contract as R09.
    if err != nil { return err }
    if disposition == CommitDuplicate { return nil }
    changes, err := dashi.ProjectCommit(turn, req.Step) // Proposed transcript receipts.
    if err != nil { return err }
    for _, receipt := range changes.DeliveredInputs {
        if err := tx.ConsumeInputOnce(turn.ID, receipt.ID, receipt.Seq); err != nil { return err }
    }
    return tx.AppendStepAndView(req, changes.View)
})
```

**Observable result:** The transcript record and consumed input would appear together, once. A later budget refusal can leave dispatch at zero; the next funded call still sees the saved message. A notification would wake the worker only after pending input is durably visible.

**Developer benefit:** Remove input-ID scanning across the whole saved Turn. Reuse Dashi's existing input deduplication. The still-required Steering.Delivered callback idempotently confirms the saved receipt; it does not consume or scan the whole Turn again.

**Application responsibility:** Own mail identity, urgency, transaction boundaries, and wake notifications. Acknowledgments confirm transcript persistence, not model dispatch. Start with a small Step-to-input-receipts helper; defer a mailbox/notification framework.

**Additional runnable evidence:** Example_savedInputBeforeDispatch in [the offline demos](demo/).
