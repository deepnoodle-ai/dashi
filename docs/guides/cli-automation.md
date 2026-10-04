# Run Dashi from scripts

`dashi run "prompt"` runs one request without a screen. It prints the last
parent reply to stdout and diagnostics to stderr. The session is saved.
Use `dashi run -c "what did you change?"` for a new request after prior work
permits it. Plain `dashi` with redirected stdin and no `--plain` also runs one
request.

Redirected stdin supplies one literal request, read to EOF, up to 16 MiB.
With a prompt, Dashi combines the exact prompt, two newlines, and exact stdin.
Blank lines, tabs, trailing newlines, `/quit`, `yes`, and `@file` remain data.
Only explicit prompt mentions and `--attach` select file snapshots.
Terminal stdin is never read in one-shot mode.

`dashi run --output-format text|json|jsonl` (`-o`) selects the output format with
any terminal arrangement; the default is `text`. JSON returns
`{"messages":[{"content":"reply"}]}`. JSONL emits one complete
`{"content":"reply"}` per line when each parent response stops.
Both include public parent request and task-notice replies reached during the
invocation. Empty replies produce no message. JSON returns `{"messages":[]}`
when none exists. Errors, attention targets, and saved-session guidance stay on
printable stderr. Useful partial replies can accompany a nonzero exit.

`dashi run -c` or `dashi run -r ID` with no request returns the relevant latest
saved reply without live setup when work is complete or needs resolution. Ready
interrupted work can continue through existing checks. Resume grants no approval,
repeats no effect, and raises no limit. A new payload cannot close unfinished
work implicitly.
Stdin never supplies approval. `--yes` retains its existing tool policy.

Exit codes are 0 for completion and written output, 1 for execution/setup/I/O
failure, 2 for invalid input or flags, 3 for required attention, and 4 for a
recorded limit, refusal, or cancellation. SIGINT/SIGTERM return 130/143.
Broken or stalled stdout fails without retrying effects or repeating output prefixes.

Keep the build's status when asking Dashi for a diagnosis:

```sh
build_status=0
go test ./... >build.log 2>&1 || build_status=$?
if [ "$build_status" -ne 0 ]; then
  dashi run "Explain this test failure" -o json <build.log >response.json || true
  jq -r '.messages[].content' response.json || true
fi
exit "$build_status"
```
