# R31: Share provider defaults and define voice metadata deliberately

[All examples](README.md) · [Review](../README.md)

The settings UI offers Gemini and Grok voices. Swarm hard-codes model IDs and its selected voice lists. Dashi exports default
model IDs today, but does not export a voice catalog or validate voice names
against one. A full catalog would be new maintained behavior.

## Before: current code

From [swarm/internal/config/voice.go:12–13](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/config/voice.go#L12). Surrounding definitions are omitted.

<!-- source: swarm internal/config/voice.go 12 13 -->
```go
gemini := manager.VoiceProvider{ID: "gemini", Label: "Gemini", Voices: []string{"Aoede", "Kore", "Puck", "Charon", "Fenrir"}, Model: "gemini-3.8-live"}
grok := manager.VoiceProvider{ID: "grok", Label: "Grok", Voices: []string{"ara", "rex", "sal", "eve", "leo"}, Model: "grok-voice-think-fast-2.0"}
```

## After: Existing API

This uses current Dashi APIs. Application storage methods are illustrative;
the snippet is not a complete program.

```go
// Existing provider constants remove the duplicated default model IDs now.
gemini.Model = geminilive.DefaultModel
grok.Model = grokvoice.DefaultModel
// Swarm's selected voice subset stays in application config today.
// Proposed future metadata: defaults/capabilities first; a catalog needs a policy
// shared by Connect and the UI, with unknown/new voices handled deliberately.
```

**Observable result:** Default model selection would follow Dashi today. Future voice metadata could inform the UI; a stale static list must not reject newly valid provider voices by accident.

**Developer benefit:** Remove duplicated default model strings now. Exporting adapter defaults/capabilities is a small addition; a full voice catalog needs maintenance and a clear support policy.

**Application responsibility:** Own UI labels, availability, and secrets. Realtime dollar caps require reliable pricing/spending bounds; usage events alone are insufficient.
