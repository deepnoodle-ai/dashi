# Gemini credential isolation

The adapter resolves API-key aliases in `New`, only for native Google routes.
The Google SDK treats an empty API key as permission to read native environment
credentials. That second lookup can send a Google key to a custom destination.

For the Gemini API backend, supply a private placeholder to SDK construction
when the resolved key is empty. This permits SDK serialization without an
ambient credential. Before real dispatch, `sdkGuard` replaces the API-key header
with the adapter's resolved key, or removes it for a keyless custom transport.
The placeholder never reaches a real transport. Native public calls without a
key retain their existing pre-dispatch `ErrNoAPIKey` failure.

Vertex keeps its separate authentication path. URL/body pinning and redirect
refusal remain in place. No new public configuration or credential source is
introduced. Do not temporarily change process environment variables: concurrent
clients may use different credentials.

Verification uses the actual SDK with a fixture transport for both native
variable names, empty and explicit custom keys, and streamed and ordinary
calls. Preparation must make no dispatch. Existing native, missing-key, and
Vertex fixtures remain required. Run Gemini formatter, vet, build, and race
tests, then an independent security review.
