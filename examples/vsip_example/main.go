package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/exotel/langstream/pkg/langstream"
	"github.com/exotel/langstream/pkg/tts"
)

// newDemoSession builds a langstream.Session using the always-available
// "mock" ASR/MT/TTS backends (see pkg/langstream/backends.go), the same
// way examples/backend_selection does. A real Exotel vSIP integration
// would select real vendor backends here instead (or via the same
// name-based registry, see cmd/langstream/main.go's --backend flag) —
// nothing about VSIPCallAdapter depends on which backends are behind the
// Session.
func newDemoSession(ctx context.Context) (*langstream.Session, error) {
	rec, err := langstream.NewASRBackend(langstream.BackendMock)
	if err != nil {
		return nil, fmt.Errorf("selecting ASR backend: %w", err)
	}
	tr, err := langstream.NewTranslatorBackend(langstream.BackendMock)
	if err != nil {
		return nil, fmt.Errorf("selecting MT backend: %w", err)
	}
	syn, err := langstream.NewTTSBackend(langstream.BackendMock)
	if err != nil {
		return nil, fmt.Errorf("selecting TTS backend: %w", err)
	}

	return langstream.NewSession(ctx, langstream.SessionConfig{
		CallerLanguage: "hi",
		AgentLanguage:  "en",
		ASR:            rec,
		Translator:     tr,
		TTS:            syn,
	})
}

// fakeAudioSource returns a channel of n small PCM frames simulating audio
// "just extracted" from an inbound vSIP RTP stream, closed once all frames
// have been sent. This is the one piece of this example that a real
// integration replaces outright: a real adapter would feed
// VSIPCallAdapter.PumpCallerAudio/PumpAgentAudio from an actual RTP
// receive loop instead. Frames here are sized under the mock ASR
// backend's internal flush threshold (see pkg/asr/mock.go), so, exactly
// as in examples/backend_selection, they surface as a final transcript
// only once the session is closed rather than immediately.
func fakeAudioSource(n int) <-chan []byte {
	out := make(chan []byte, n)
	for i := 0; i < n; i++ {
		out <- make([]byte, 320) // 20ms @ 8kHz, 16-bit mono, silence-shaped
	}
	close(out)
	return out
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := newDemoSession(ctx)
	if err != nil {
		log.Fatalf("creating session: %v", err)
	}

	adapter := NewVSIPCallAdapter(sess, 8000)

	// Playback pumps run concurrently with the rest of the call, printing
	// what a real integration would re-encode and write back out over the
	// vSIP trunk's outbound RTP streams. They return once Session.Close
	// (below) has fully drained and closed both outbound channels.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, err := adapter.PumpAgentPlayback(ctx, func(chunk tts.AudioChunk) error {
			fmt.Printf("[vsip-out -> agent]  %d bytes PCM @ %dHz (final=%v) -- would re-encode + write to the agent leg's outbound RTP stream\n",
				len(chunk.PCM), chunk.SampleRate, chunk.IsFinal)
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("agent playback pump ended with error: %v", err)
		}
		fmt.Printf("agent playback pump delivered %d chunk(s)\n", n)
	}()
	go func() {
		defer wg.Done()
		n, err := adapter.PumpCallerPlayback(ctx, func(chunk tts.AudioChunk) error {
			fmt.Printf("[vsip-out -> caller] %d bytes PCM @ %dHz (final=%v) -- would re-encode + write to the caller leg's outbound RTP stream\n",
				len(chunk.PCM), chunk.SampleRate, chunk.IsFinal)
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("caller playback pump ended with error: %v", err)
		}
		fmt.Printf("caller playback pump delivered %d chunk(s)\n", n)
	}()

	// Simulate one short inbound "utterance" from each side of the call,
	// as if just decoded from the vSIP trunk's inbound RTP streams (see
	// fakeAudioSource's doc comment on what a real integration replaces
	// here).
	pushedCaller, err := adapter.PumpCallerAudio(ctx, fakeAudioSource(1))
	if err != nil {
		log.Fatalf("pumping caller audio: %v", err)
	}
	fmt.Printf("pushed %d caller audio frame(s) into the session\n", pushedCaller)

	pushedAgent, err := adapter.PumpAgentAudio(ctx, fakeAudioSource(1))
	if err != nil {
		log.Fatalf("pumping agent audio: %v", err)
	}
	fmt.Printf("pushed %d agent audio frame(s) into the session\n", pushedAgent)

	// Closing the session flushes each leg's buffered utterance as a
	// final transcript (see langstream.Session.Close's doc comment) and
	// closes both outbound channels once fully drained -- which is what
	// lets the playback goroutines above observe channel closure and
	// return.
	if err := sess.Close(); err != nil {
		log.Fatalf("closing session: %v", err)
	}

	wg.Wait()

	fmt.Println("vsip_example (shape demo) complete: this demonstrated the langstream.Session integration contract/shape only -- no real socket was involved yet (see doc.go)")

	// Now demonstrate the same kind of call over a real rtp.DuplexSession
	// and real loopback UDP sockets (see real_rtp.go) -- the piece doc.go
	// originally flagged as not yet implemented here. Deliberately not
	// passed ctx above (see runRealRTPDemo's own doc comment on why it
	// owns an independent timeout instead of inheriting this one).
	if err := runRealRTPDemo(); err != nil {
		log.Fatalf("real RTP demo failed: %v", err)
	}
}
