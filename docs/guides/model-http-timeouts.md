# Model HTTP timeouts

Set timeouts when constructing a supported HTTP model client. A response-start
bound prevents a call from waiting indefinitely for headers; a stream-idle
bound stops a body read when no bytes arrive; a total bound caps the whole
call, including an active stream.

```go
total := 20 * time.Minute
client, err := responses.New(responses.Config{
    Timeouts: llm.HTTPTimeouts{
        ResponseStart: 30 * time.Second,
        StreamIdle:    90 * time.Second,
        Total:         &total,
    },
})
```

| Model client | Timeout support |
| --- | --- |
| OpenAI Responses, Chat Completions, Anthropic Messages | Direct |
| Grok, Meta, DeepInfra, Mistral, OpenRouter, Ollama | Forwarded to their shared HTTP adapter |
| Gemini and Vertex AI | Not yet: model calls use the Google SDK path |
| Image/video clients and application-defined `llm.Client` | Not covered by this option |

`ResponseStart` runs from sending the request through response headers,
including connect, TLS, request upload, and provider wait; it cannot identify
which phase stalled. `StreamIdle` applies only to streaming model responses,
while blocked in a raw body read. SSE comments and heartbeat bytes reset it;
application processing time between reads does not consume it. `Total` spans
startup and response body. Zero phase durations disable those bounds, and
negative values fail construction.

With `Total == nil`, Dashi uses its existing five-minute default when
`Config.Client` is nil. A supplied `http.Client` keeps its own `Timeout`
instead. A positive `Total` adds a Dashi bound; an explicit zero removes the
Dashi default but cannot disable a supplied client's timeout. The earliest
caller context, Turn deadline, Dashi timer, or supplied-client timeout wins.
Only Dashi-owned expiries carry precise `llm.Error.Reason` values:
`ReasonResponseStartTimeout`, `ReasonStreamIdleTimeout`, and
`ReasonCallTimeout`. A supplied client's shorter timeout remains a transport
error without a guessed Dashi reason.

Custom `http.RoundTripper` implementations must return from sending and
unblock response-body reads when the request context is canceled. Dashi cannot
enforce a bound while a custom transport ignores cancellation in a blocked
operation.

On a failed stream, any received text, content, and usage remain provisional.
A `dashi.Runner` stores them on the finished model call and records the typed
reason in `ModelFact.ErrorReason`. The Turn may stop `result_unknown` because
a timeout cannot prove that a provider never received the request or ran a
provider tool. Dashi does not resend automatically. The application's
`Limits.Deadline` is a separate Turn limit; a host context cancellation keeps
its existing interruption or cancellation behavior.
