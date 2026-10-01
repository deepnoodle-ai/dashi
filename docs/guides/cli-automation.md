# Run Dashi from scripts

`dashi -p "prompt"` runs one request without a screen. It prints the last
parent answer to stdout and diagnostics to stderr. The session is saved.
Use `dashi -c -p "what did you change?"` for a new request after prior work permits it.

Redirected stdin supplies one literal request, read to EOF, up to 16 MiB.
With `-p`, Dashi combines the exact prompt, two newlines, and exact stdin.
Blank lines, tabs, trailing newlines, `/quit`, `yes`, and `@file` remain data.
Only explicit prompt mentions and `--attach` select file snapshots.
Terminal stdin is never read in one-shot mode.

`--output-format text|json|jsonl` selects one-shot output with any terminal arrangement.
It conflicts with `--plain`, `--inline`, and `models`. JSON returns
`{"messages":[{"content":"answer"}]}`. JSONL emits one complete
`{"content":"answer"}` per line when each parent response stops.
Both include public parent request and task-notice answers reached during the invocation.
Empty answers produce no message. JSON returns `{"messages":[]}` when none exists.
Errors, attention targets, and saved-session guidance stay on printable stderr.
Useful partial answers can accompany a nonzero exit.

No-payload one-shot `--resume` returns the relevant latest saved answer without
live setup when work is complete or needs resolution. Ready interrupted work
can continue through existing checks. Resume grants no approval, repeats no effect,
and raises no limit. A new payload cannot close unfinished work implicitly.
Stdin never supplies approval. `--yes` retains its existing tool policy.

Exit codes are 0 for completion and delivered output, 1 for execution/setup/I/O
failure, 2 for invalid input or flags, 3 for required attention, and 4 for a
recorded limit, refusal, or cancellation. SIGINT/SIGTERM return 130/143.
Broken or stalled stdout fails without retrying effects or repeating output prefixes.

Keep the build's status when asking Dashi for a diagnosis:

```sh
build_status=0
go test ./... >build.log 2>&1 || build_status=$?
if [ "$build_status" -ne 0 ]; then
  dashi -p "Explain this test failure" --output-format json <build.log >response.json || true
  jq -r '.messages[].content' response.json || true
fi
exit "$build_status"
```
