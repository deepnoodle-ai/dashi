# 07 — Authenticated provider-hosted MCP: draft PRD

**Status:** draft proposal. **Size / difficulty:** M / high. **Research:** [authenticated hosted MCP findings](07-hosted-mcp-auth-research.md). **Existing design:** [resources and hosted tools](../2026-09-27-resources-and-hosted-tools.md), especially its credential boundary. This proposal is for a model provider connecting to a remote MCP server on the application's behalf; Dashi's separate [local MCP client](../remaining-work.md) item is not part of this feature.

## User problem

An application developer can expose a public MCP server to OpenAI Responses or Anthropic Messages today, but cannot use a private server without putting its access token in a tool definition or request body. Dashi records its exact request body as durable preparation evidence, so simply adding a token field would persist a secret. The developer also needs a predictable answer to “which tenant's connection is this?” and “who approves this tool call?”

## Goals and non-goals

**Goals:** one small, understandable way to bind a private MCP server to an app-owned credential; a recordable request snapshot that cannot disclose the token; exact dispatch bytes retained only in memory; clear failure when a binding is missing, expired, revoked or mismatched; explicit provider-hosted approval where the provider supports it. The authenticated path should retain existing URL, allowed-tool, origin and replay protections.

**Non-goals for the first slice:** OAuth browser flows, refresh-token storage, vault implementation, generic MCP client transport, arbitrary secret headers, approval policy engines and automatic approval. The host application already owns connection consent and token refresh. Start with bearer access tokens; add other provider-supported auth shapes when a real user requires them.

## Proposed smallest API and user flow

1. Keep `NewMCPTool` as the provider-specific, non-secret declaration. Add an opaque **credential binding** identifier to its options. It is recordable and never sent to the model provider. The binding is meaningful only to the host application; it should identify an authorized connection, not contain a token or mutable “current user” shortcut.
2. Configure a **credential resolver** on the provider client. At `Prepare`, Dashi gives it the binding, provider/API, server label and canonical HTTPS URL. The application verifies tenant, actor, grant, server and token lifetime, then returns a short-lived bearer access token. The resolver contract rejects an unscoped, revoked or mismatched credential; Dashi reports a typed, secret-free preparation error. There is no fallback to another credential.
3. `Prepare` builds a private exact wire payload for this request and a public `RequestSnapshot` with token-bearing fields redacted, a verification digest and named secret bindings. `Send` uses only the original private payload; it must not silently re-resolve or alter bytes. Public inspection, recorders, logs and `Request.Body()` must never yield the secret-bearing payload. Existing unauthenticated requests can retain their current exact snapshot behavior if the distinction stays obvious.
4. On recovery, a recorded snapshot is **not dispatchable**. The application rechecks authority and calls `Prepare` again with the recorded non-secret intent and binding; that is a new attempt. If an earlier attempt may have reached the provider, the host reconciles that uncertainty before retrying.
5. OpenAI Responses keeps `RequireApproval="always"` as the authenticated default and surfaces each provider `mcp_approval_request` through Dashi's existing waiting/settlement flow. An explicit opt-out can be evaluated after the first slice with concrete read-only use cases. Anthropic authenticated MCP should be enabled only with an explicit statement that this provider route lacks per-call application approval in Dashi.

Illustrative API shape, subject to naming review:

```go
mcp, _ := responses.NewMCPTool(responses.MCPToolOptions{
    ServerLabel: "docs",
    ServerURL: "https://docs.example/mcp",
    AllowedTools: []string{"search"},
    CredentialBinding: "connection:docs-for-tenant-42", // non-secret
    RequireApproval: "always",
})
client, _ := responses.New(responses.Config{
    MCPCredentials: func(ctx context.Context, request responses.MCPCredentialRequest) (string, error) {
        return connections.AccessToken(ctx, request.Binding, request.ServerURL)
    },
})
```

The exact Go names and resolver return type are open. The important split is that the tool stores a stable reference, while the app supplies the token from its existing connection system just before preparation.

## Acceptance criteria

1. A developer can use a private HTTPS MCP server with a bearer token and OpenAI Responses using the same hosted-tool and Runner flow as public MCP, plus one binding and resolver. A minimal example demonstrates a pending approval and explicit approve/deny.
2. A fixture proves the provider receives the token in the documented wire field and that `Send` transmits the exact private bytes built by `Prepare`. No credential re-resolution or mutation occurs between those operations.
3. No library-controlled path puts a token in `Input`, `RequestSnapshot` (including `Body`), `Request.Body()`, `Raw`, recorded events, trace/log fields or user-facing errors. Tests include resolver errors and a provider response that directly echoes the token; such a response fails closed before recording. A safe redacted body, named binding and verifiable digest remain available for audit. The digest mechanism must not enable practical guessing of low-entropy secrets.
4. Missing, expired, revoked, wrong-tenant and wrong-server bindings fail before provider dispatch with distinct, secret-free outcomes. No fallback credential is selected. A fresh `Prepare` after recovery rechecks the binding and is recorded as a new attempt.
5. OpenAI approval request, denial and approval continue to round-trip with pending-request identity and same-origin checks. Authenticated use cannot accidentally inherit an approval-free default. The Anthropic path documents its actual approval limit rather than promising parity.
6. Existing unauthenticated hosted MCP request snapshots and replay tests still pass; the new credential mechanism cannot be smuggled through arbitrary hosted options or URL fields.

## Risks and open decisions

- **Snapshot contract:** `Request.Body()` currently exposes exact bytes. Decide whether it becomes a safe inspection view or authenticated requests use a separate non-exportable request type. Avoid a public “unsafe body” accessor that recorders might call by habit. The redacted-body/digest format and integrity method need a short technical design before coding.
- **Provider behavior:** OpenAI accepts an OAuth `authorization` value and headers; Anthropic accepts `authorization_token`. Qualify both with fixtures and current API docs. OpenAI is the first delivery slice because Dashi already models its hosted approval. Decide whether Anthropic requires an explicit no-per-call-approval policy before use.
- **Secret echoes:** A provider or MCP server can return attacker-controlled text. Scrubbing known token values at all recording and display boundaries may fail with transformed encodings; define a fail-closed policy for direct echoes and document residual limits.
- **Credential lifetime:** A token may expire between `Prepare` and `Send`. Return an explicit send failure and require new preparation; never replace the token behind a recorded attempt. The host still owns retry, idempotency and grant revocation semantics.
