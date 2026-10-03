package main

import (
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// A needle is an exact detail planted early in a session. Answer is the
// string a probe reply must contain; Question asks for it without saying
// it.
type needle struct {
	Kind     string // error, path, decision, constraint
	Answer   string
	Question string
}

// theme is the material of one session.
type theme struct {
	project string
	task    string
	latest  string
	// errBlock is the failing part of a test run; the run's other lines
	// are generated around it.
	errBlock   string
	errNeedle  needle
	grepQuery  string
	grepHit    string
	pathNeedle needle
	decision   string
	decNeedle  needle
	constraint string
	conNeedle  needle
	words      []string // package and file words for filler
}

var themes = []theme{
	{
		project: "ledgerd",
		task:    "The ledger tests started failing after the currency refactor. Find the cause and fix it.",
		latest:  "Now apply the fix, update the rounding overrides file, and rerun the failing test. Stick to what we decided about amounts.",
		errBlock: "--- FAIL: TestPostTransfer (0.01s)\n" +
			"    transfer_test.go:142: post: ledger: negative balance on acct_7Q2X (code LGR-4471)\n",
		errNeedle: needle{"error", "LGR-4471", "What error code did the first failing test run report?"},
		grepQuery: "rounding_mode",
		grepHit:   "config/currency/rounding_overrides.toml:7: rounding_mode = \"bankers\"",
		pathNeedle: needle{"path", "config/currency/rounding_overrides.toml",
			"Which file sets the rounding mode overrides?"},
		decision: "Decision: we will store amounts as integer minor units instead of decimal.Decimal, " +
			"because the reporting job serializes amounts through Avro, and our schema registry has no decimal logical type.",
		decNeedle: needle{"decision", "Avro",
			"Why did we decide to store amounts as integer minor units instead of decimal.Decimal?"},
		constraint: "One rule for this work: never edit anything under migrations/2024_q3/, because the data team replays those files.",
		conNeedle:  needle{"constraint", "migrations/2024_q3/", "Which directory did I say you must never edit?"},
		words:      []string{"ledger", "currency", "transfer", "account", "posting", "journal", "fx", "audit"},
	},
	{
		project: "exprkit",
		task:    "The parser tests fail after yesterday's merge. Please fix them.",
		latest:  "Go ahead: patch the generated keyword table, then rerun the parser tests. Follow the plan we agreed on for the generator.",
		errBlock: "--- FAIL: TestParseNestedCall (0.00s)\n" +
			"    parser_test.go:88: Parse(\"max(1, min(2, 3))\"): error E_PARSE_0x1F: unexpected token \")\" at offset 16\n",
		errNeedle: needle{"error", "E_PARSE_0x1F", "What error code did the failing parser test report?"},
		grepQuery: "kwTable",
		grepHit:   "internal/lexer/keywords_gen.go:12: var kwTable = [...]keyword{",
		pathNeedle: needle{"path", "internal/lexer/keywords_gen.go",
			"Which file defines the generated keyword table?"},
		decision: "Decision: we will patch the generated table by hand instead of upgrading the generator, " +
			"because generator v2 needs Go 1.27 and the CI image is pinned to golang:1.25-bookworm.",
		decNeedle: needle{"decision", "1.25-bookworm",
			"Why did we decide to patch the generated table instead of upgrading the generator? Name the CI image."},
		constraint: "Also: never rename ParseExprWithEnv. The plugin SDK looks it up by name.",
		conNeedle:  needle{"constraint", "ParseExprWithEnv", "Which function did I say must never be renamed?"},
		words:      []string{"parser", "lexer", "ast", "eval", "token", "scanner", "printer", "builtin"},
	},
	{
		project: "syncgate",
		task:    "Mobile clients see duplicate items after a sync. Find out why and fix it.",
		latest:  "Now implement the cursor change, adjust the canary autoscaling file, and run the sync tests again. Keep to the plan we chose.",
		errBlock: "--- FAIL: TestResumeAfterDisconnect (0.12s)\n" +
			"    resume_test.go:57: sync: replayed 3 items twice (code SYNC-E1093)\n",
		errNeedle: needle{"error", "SYNC-E1093", "What error code did the failing sync test report?"},
		grepQuery: "maxReplicas",
		grepHit:   "deploy/k8s/overlays/canary/hpa.yaml:9:   maxReplicas: 12",
		pathNeedle: needle{"path", "deploy/k8s/overlays/canary/hpa.yaml",
			"Which file sets the canary autoscaling limits?"},
		decision: "Decision: we will use per-device cursors instead of global sequence numbers, " +
			"because the replica lag alarm ALRM-77 fires whenever sequence reads go to the primary.",
		decNeedle: needle{"decision", "ALRM-77",
			"Why did we choose per-device cursors over global sequence numbers? Name the alarm."},
		constraint: "Important: never touch the feature flag sync_v1_fallback. Ops uses it as a kill switch.",
		conNeedle:  needle{"constraint", "sync_v1_fallback", "Which feature flag did I say you must never touch?"},
		words:      []string{"sync", "cursor", "device", "batch", "conflict", "queue", "push", "session"},
	},
	{
		project: "tidepool",
		task:    "The nightly dedupe job drops real events. Find the bug and fix it.",
		latest:  "Please change the dedupe window in the nightly job file and rerun the pipeline test. Use the window size we agreed on.",
		errBlock: "--- FAIL: TestNightlyDedupe (2.31s)\n" +
			"    dedupe_test.go:204: arrow: allocation failed in hash join (code ARROW-OOM-512)\n",
		errNeedle: needle{"error", "ARROW-OOM-512", "What error code did the failing pipeline test report?"},
		grepQuery: "dedupe_window",
		grepHit:   "jobs/nightly/dedupe_window.sql:3: -- dedupe_window: 24 hours",
		pathNeedle: needle{"path", "jobs/nightly/dedupe_window.sql",
			"Which file sets the nightly dedupe window?"},
		decision: "Decision: we will use a 36-hour dedupe window instead of 24 hours, " +
			"because the vendor feed from Kestrel Labs replays late events up to 30 hours after the fact.",
		decNeedle: needle{"decision", "Kestrel Labs",
			"Why did we choose a 36-hour dedupe window? Name the vendor."},
		constraint: "Note: do not rename the output table events_clean_v7; dashboards query it directly.",
		conNeedle:  needle{"constraint", "events_clean_v7", "Which output table did I say must not be renamed?"},
		words:      []string{"ingest", "dedupe", "window", "events", "arrow", "partition", "sink", "schema"},
	},
	{
		project: "keystone-auth",
		task:    "Logins fail intermittently with token validation errors. Investigate and fix it.",
		latest:  "Go ahead and change the JWKS cache, fix the provider quirks file, and rerun the auth tests. Keep the cache duration we decided on.",
		errBlock: "--- FAIL: TestValidateRotatedKey (5.00s)\n" +
			"    jwks_test.go:118: oidc: key fetch failed (code JWKS_FETCH_TIMEOUT_9)\n",
		errNeedle: needle{"error", "JWKS_FETCH_TIMEOUT_9", "What error code did the failing auth test report?"},
		grepQuery: "kidHeader",
		grepHit:   "pkg/oidc/providers/okta_quirks.go:31: const kidHeader = \"x-okta-kid\"",
		pathNeedle: needle{"path", "pkg/oidc/providers/okta_quirks.go",
			"Which file holds the identity provider quirks?"},
		decision: "Decision: we will cache JWKS for 10 minutes instead of 1 hour, " +
			"because Okta rotates the signing keys of the tenant acme-prod every 6 hours without notice.",
		decNeedle: needle{"decision", "acme-prod",
			"Why did we choose a 10-minute JWKS cache? Name the tenant."},
		constraint: "And never log the claim x_acme_ssn_hash, not even at debug level.",
		conNeedle:  needle{"constraint", "x_acme_ssn_hash", "Which claim did I say must never be logged?"},
		words:      []string{"oidc", "jwks", "token", "session", "claims", "issuer", "verify", "cache"},
	},
	{
		project: "lumen-index",
		task:    "Search results go stale after a reindex. Find the cause and fix it.",
		latest:  "Now follow the reindex runbook, switch the alias, and rerun the search tests. Use the reindex approach we picked.",
		errBlock: "--- FAIL: TestReindexKeepsAlias (8.40s)\n" +
			"    reindex_test.go:66: cluster: rebalance stuck (code SHARD_REBALANCE_E22)\n",
		errNeedle: needle{"error", "SHARD_REBALANCE_E22", "What error code did the failing reindex test report?"},
		grepQuery: "blue-green",
		grepHit:   "ops/runbooks/reindex_blue_green.md:1: # Reindex with blue-green indexes",
		pathNeedle: needle{"path", "ops/runbooks/reindex_blue_green.md",
			"Which file is the reindex runbook?"},
		decision: "Decision: we will reindex blue-green instead of in place, " +
			"because shard 14 sits on rack R3-B, which has the failing disk.",
		decNeedle: needle{"decision", "R3-B",
			"Why did we choose blue-green reindexing over in place? Name the rack."},
		constraint: "Keep the alias name products_live exactly; the storefront hardcodes it.",
		conNeedle:  needle{"constraint", "products_live", "Which alias name did I say must stay exactly as it is?"},
		words:      []string{"index", "shard", "alias", "mapping", "query", "ranker", "segment", "replica"},
	},
}

var followUps = []string{"Keep going.", "OK, continue.", "Fine, carry on."}

// vagueLatest is the latest request of the vague variant: it names no
// subject, so neither Decide nor a summarizer can learn from it what the
// next step needs.
const vagueLatest = "Keep going."

// variant changes how sessions are generated.
type variant struct {
	// vague replaces the theme's latest request, which names the needles'
	// subjects, with vagueLatest.
	vague bool
	// longTurn makes the last turn larger than selective's tail share
	// (0.10 of the window) but smaller than stock's (0.25), by reading a
	// large file after the latest request.
	longTurn bool
}

// session is one history to compact and the needles planted in it.
type session struct {
	name     string
	messages []llm.Message
	needles  []needle
	latest   string // the latest request
	lastRead string // the file read after the latest request
}

// builder writes a session's messages.
type builder struct {
	rng      *rand.Rand
	th       theme
	messages []llm.Message
	calls    int
}

func newSession(i int, seed uint64, v variant) session {
	th := themes[i%len(themes)]
	b := &builder{rng: rand.New(rand.NewPCG(seed, uint64(i))), th: th}
	b.add(llm.Message{Role: llm.RoleSystem, Content: []llm.Content{llm.Text(
		"You are a coding agent working in the " + th.project + " repository. " +
			"Use the tools to inspect and change the code. Be concise.")}})
	b.add(llm.UserMessage(llm.Text(th.task)))
	b.exchange("I will look at the repository first.", "ls", `{"path":"."}`, b.listing())
	b.exchange("", "read_file", `{"path":"README.md"}`, b.readme())
	b.exchange("", "git_log", `{"n":20}`, b.gitLog())
	b.exchange("Running the tests to see the failure.", "run_tests", `{"pkg":"./..."}`, b.testRun(th.errBlock))
	b.exchange("", "read_file", `{"path":"`+b.file()+`"}`, b.code(70))
	b.exchange("", "grep", `{"pattern":"`+th.grepQuery+`"}`, b.grep(th.grepHit))
	b.exchange("", "read_file", `{"path":"`+b.file()+`"}`, b.code(60))
	b.add(llm.AssistantMessage(llm.Text(th.decision)))
	b.add(llm.UserMessage(llm.Text(th.constraint)))
	b.add(llm.AssistantMessage(llm.Text("Understood.")))
	for k := range 12 {
		switch k % 4 {
		case 0:
			b.exchange("", "tail_log", `{"service":"`+th.project+`"}`, b.logTail(55))
		case 1:
			b.exchange("", "read_file", `{"path":"`+b.file()+`"}`, b.code(65))
		case 2:
			b.exchange("", "web_search", `{"q":"`+b.word()+` `+b.word()+`"}`, b.search())
		case 3:
			b.exchange("", "run_tests", `{"pkg":"./`+b.word()+`/..."}`, b.passing(30))
			// A short follow-up, as a user at a CLI types between turns.
			b.add(llm.AssistantMessage(llm.Text("Still looking; nothing conclusive yet.")))
			b.add(llm.UserMessage(llm.Text(followUps[k/4%len(followUps)])))
		}
	}
	latest := th.latest
	if v.vague {
		latest = vagueLatest
	}
	b.add(llm.UserMessage(llm.Text(latest)))
	lines := 20
	if v.longTurn {
		lines = 170 // about 10 KB: 2,500 tokens, over 0.10 of a 16,000-token window
	}
	lastRead := b.code(lines)
	b.exchange("", "read_file", `{"path":"`+b.file()+`"}`, lastRead)
	b.add(llm.AssistantMessage(llm.Text("I have what I need and will make the change now.")))
	return session{
		name:     th.project,
		messages: b.messages,
		needles:  []needle{th.errNeedle, th.pathNeedle, th.decNeedle, th.conNeedle},
		latest:   latest,
		lastRead: lastRead,
	}
}

func (b *builder) add(m llm.Message) { b.messages = append(b.messages, m) }

// exchange adds an assistant message with text (optional) and one tool
// call, and the call's result.
func (b *builder) exchange(text, name, args, output string) {
	b.calls++
	id := fmt.Sprintf("toolu_%02d", b.calls)
	var parts []llm.Content
	if text != "" {
		parts = append(parts, llm.Text(text))
	}
	parts = append(parts, llm.Content{
		Kind:     llm.ContentToolCall,
		ToolCall: &tool.Call{ID: id, Name: name, Args: []byte(args)},
	})
	b.add(llm.AssistantMessage(parts...))
	b.add(llm.UserMessage(llm.Content{
		Kind:       llm.ContentToolResult,
		ToolResult: &llm.ToolResult{CallID: id, Output: tool.TextOutput(output)},
	}))
}

func (b *builder) word() string { return b.th.words[b.rng.IntN(len(b.th.words))] }

func (b *builder) file() string {
	return fmt.Sprintf("internal/%s/%s_%s.go", b.word(), b.word(), b.word())
}

func (b *builder) hex(n int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, n)
	for i := range out {
		out[i] = digits[b.rng.IntN(16)]
	}
	return string(out)
}

func (b *builder) listing() string {
	var lines []string
	for range 45 {
		lines = append(lines, b.file())
	}
	return strings.Join(lines, "\n")
}

func (b *builder) readme() string {
	return "# " + b.th.project + "\n\n" + b.th.project + " is maintained by the platform team. " +
		"See docs/ for the design notes. Run make test before sending a pull request.\n\n" +
		"## Layout\n\n" + b.listing()[:600] + "\n"
}

func (b *builder) gitLog() string {
	var lines []string
	for range 20 {
		lines = append(lines, b.hex(7)+" "+strings.ToUpper(b.word()[:1])+b.word()[1:]+": tidy "+b.word()+" and "+b.word())
	}
	return strings.Join(lines, "\n")
}

func (b *builder) passing(n int) string {
	var lines []string
	for range n {
		lines = append(lines, fmt.Sprintf("ok  \tgithub.com/acme/%s/internal/%s/%s\t0.%03ds",
			b.th.project, b.word(), b.word(), b.rng.IntN(1000)))
	}
	return strings.Join(lines, "\n")
}

// testRun is a test run with block in its middle, so a short form that
// keeps only the first and last lines loses it.
func (b *builder) testRun(block string) string {
	return b.passing(18) + "\n" + block + b.passing(18) + "\nFAIL\nexit status 1"
}

func (b *builder) code(n int) string {
	var lines []string
	lines = append(lines, "package "+b.word(), "")
	for i := 0; len(lines) < n; i++ {
		fn := b.word() + strings.ToUpper(b.word()[:1]) + b.word()[1:]
		lines = append(lines,
			fmt.Sprintf("// %s handles the %s step for one %s.", fn, b.word(), b.word()),
			fmt.Sprintf("func %s(ctx context.Context, in *%sInput) (*%sOutput, error) {", fn, b.word(), b.word()),
			fmt.Sprintf("\tif err := in.Validate(); err != nil {"),
			fmt.Sprintf("\t\treturn nil, fmt.Errorf(\"%s: %%w\", err)", b.word()),
			"\t}",
			fmt.Sprintf("\treturn &%sOutput{ID: in.ID, Count: len(in.Items) + %d}, nil", b.word(), i),
			"}",
			"")
	}
	return strings.Join(lines[:n], "\n")
}

func (b *builder) grep(hit string) string {
	var lines []string
	for range 6 {
		lines = append(lines, fmt.Sprintf("%s:%d: // see %s for %s", b.file(), b.rng.IntN(200), b.word(), b.word()))
	}
	lines = append(lines, hit)
	for range 5 {
		lines = append(lines, fmt.Sprintf("%s:%d: %s := %s(%s)", b.file(), b.rng.IntN(200), b.word(), b.word(), b.word()))
	}
	return strings.Join(lines, "\n")
}

func (b *builder) logTail(n int) string {
	var lines []string
	for i := range n {
		lines = append(lines, fmt.Sprintf(
			"2026-09-30T12:%02d:%02dZ INFO %s request id=req_%s path=/v1/%s status=200 dur=%dms",
			i/60, i%60, b.th.project, b.hex(8), b.word(), b.rng.IntN(90)+3))
	}
	return strings.Join(lines, "\n")
}

func (b *builder) search() string {
	var lines []string
	for i := range 8 {
		lines = append(lines, fmt.Sprintf("%d. %s %s in Go: a practical guide (blog.example.com/%s-%s)\n   "+
			"How we handled %s and %s at scale, with benchmarks and a few pitfalls.",
			i+1, strings.ToUpper(b.word()[:1])+b.word()[1:], b.word(), b.word(), b.hex(4), b.word(), b.word()))
	}
	return strings.Join(lines, "\n")
}
