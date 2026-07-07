# latency_benchmark

Run it from the repo root with `go run ./tools/latency_benchmark [flags]`
(see `-h` for all flags: `-iterations`, `-pcm-bytes`, `-iteration-timeout`,
`-caller-lang`/`-agent-lang`, `-verbose`); it builds a real
`langstream.Session` against PE's real mock ASR/MT/TTS backends, pushes one
caller-leg audio frame per simulated call, waits for the resulting audio on
`AgentHearsAudio()`, and prints p50/p95/p99 latency (via
`pkg/observability.LatencyRecorder`) for session setup, session teardown,
and the full glass-to-glass round trip. Today every number it prints is
against instant, in-memory mocks and is therefore meaningless for real
latency planning (worse, `glass_to_glass_ms` currently reports zero
samples due to a known `Session.Close()` bug documented in
`langstream_integration_test.go` — see `TestSessionClose_DropsFinalUtteranceOnHangup`);
the value of this tool is that the harness itself — CLI, Session wiring,
recorder plumbing, percentile reporting — already exists and runs cleanly,
so once Week 2 swaps in real Deepgram/Sarvam ASR, GPT-4o translation, and
Cartesia TTS behind the same `asr.Recognizer`/`translate.Translator`/`tts.Synthesizer`
interfaces, this tool needs zero rewriting to start producing real,
trustworthy glass-to-glass latency numbers on day one.
