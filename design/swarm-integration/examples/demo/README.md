# Offline Go demos

[Before-and-after index](../README.md)

Run from the repository root:

```sh
go test -v ./design/swarm-integration/examples/demo
```

The tests capture stdout and assert these transcripts. Passing tests normally
show their PASS status instead of the captured text. All requests use local
fixtures or a scripted client. No provider keys, network service, or paid call
is needed.

The existing APIs below demonstrate current behavior. The MCP envelope helper
is a narrow local prototype for one real PNG/text result. Proposed public APIs
appear only in the labeled Markdown sketches.

## [Example_saveBeforeCancel](current_test.go#L38)

```text
saved: interrupted, next: continue, model calls: 0
canceled: canceled, ended: true
```

## [Example_restartAndRetry](current_test.go#L52)

```text
retry added records: 0
reply: Order 42 is ready.
model calls: 1
```

## [Example_brokenHistory](current_test.go#L85)

```text
history still contains messages: true
warning: turn_unreadable
Swarm accepts history: false
```

## [Example_workerStatus](current_test.go#L99)

```text
saved view: interrupted, inferred: true
UI with worker status: running
```

## [Example_finishedCostCorrection](current_test.go#L124)

```text
call finished: true
charge final: false
Resolve accepts correction: false
recorded USD: 0.04
```

## [Example_pendingResult](current_test.go#L151)

```text
same command: true
command ID: result:call-42
```

## [Example_correctionRounding](current_test.go#L163)

```text
difference of rounded totals: 1
rounded difference: 1
smaller update, rounded totals: 0
smaller update, rounded difference: 1
```

## [Example_readerDisconnect](current_test.go#L180)

```text
reader cancellation ended worker context: false
worker result: Finished after the reader left.
```

## [Example_lateResultAfterCancel](current_test.go#L225)

```text
Turn remains: canceled
Follow stopped at cancel: true
later records through StepsAfter: 1
```

## [Example_activeCompaction](current_test.go#L282)

```text
summary model calls: 1
reply: Order 42 is ready.
```

## [Example_mcpImage](mcp_test.go#L77)

```text
Swarm today: 1 blocks, first kind: text
local envelope demo: 2 blocks, last kind: image
```

## [Example_previewGapThenReply](current_test.go#L329)

```text
preview gap: clear partial text and keep following
received saved finish: true
reply: Complete saved reply.
```

This reader joins mid-message. It proves keeping Follow open after a preview gap
reads reply text from the followed finish record; it does not simulate a transport buffer overflowing.

## [Example_savedInputBeforeDispatch](current_test.go#L407)

```text
saved input acknowledged: true
model dispatches before funding: 0
stop: limited
funded call includes saved input: true
```

This is an existing BeforeSend preflight refusal, not the proposed R10 database
race contract. Input acknowledgment proves transcript persistence.

## [Example_calculatedAndPostedCost](current_test.go#L441)

```text
model cost in nanos: 2
account postings in micros: 2
```

The account arithmetic is the Swarm per-call rounding policy. It is not a ledger
implementation or proof of atomic postings.
