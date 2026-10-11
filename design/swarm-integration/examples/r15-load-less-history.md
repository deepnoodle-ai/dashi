# R15: Read only history needed after an applied summary

[All examples](README.md) · [Review](../README.md)

There are 100 saved Turns. An applied, previously validated summary at Turn 88
covers the earlier conversation. The next request needs that summary and Turns
89–100, while a resumed older request must retain its original history.

## Before: current code

From [swarm/internal/runtime/dashirt/compaction.go:85–89](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/compaction.go#L85). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/compaction.go 85 89 -->
```go
record, err := r.load(ctx, prior.ID)
if err != nil {
	return nil, err
}
records = append(records, record)
```

## After: Existing API

This uses current Dashi APIs. Application storage methods are illustrative;
the snippet is not a complete program.

```go
// Application query: include the latest activated, previously validated summary
// and every later conversation Turn, in order. Do not trust an unvalidated index.
turns, err := store.TurnsFromAppliedSummary(ctx, "support-7")
if err != nil { return err }
history, warnings := dashi.BuildHistory(turns) // Existing API accepts this suffix.
if len(warnings) != 0 { return refuseHistory(warnings) }
if err := store.SaveAdmittedHistory(ctx, requestID, history); err != nil { return err }
// A continuation uses its admission, even if a newer summary now exists.
original, err := store.AdmittedHistory(ctx, requestID)
```

**Observable result:** With a trustworthy applied summary, the query could load 13 Turns rather than all 100 using today's builder. A continuation would still retrieve its originally admitted history.

**Developer benefit:** Avoid reads and copies for covered history. The number illustrates this fixture; it is not a measured production speedup.

**Application responsibility:** Validate and apply summaries atomically. Without trustworthy summary validation, older covered records may still need reading. Retain records needed for recovery. Defer new lazy-loader/checkpoint APIs until measurement shows remaining duplication.
