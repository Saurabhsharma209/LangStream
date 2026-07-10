package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/exotel/langstream/pkg/tts"
)

func TestVSIPCallAdapter_PumpCallerAudio_PushesAllFramesUntilClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := newDemoSession(ctx)
	if err != nil {
		t.Fatalf("newDemoSession: %v", err)
	}
	defer sess.Close()

	adapter := NewVSIPCallAdapter(sess, 8000)

	pushed, err := adapter.PumpCallerAudio(ctx, fakeAudioSource(3))
	if err != nil {
		t.Fatalf("PumpCallerAudio: %v", err)
	}
	if pushed != 3 {
		t.Fatalf("pushed = %d, want 3", pushed)
	}
}

func TestVSIPCallAdapter_PumpAgentAudio_PushesAllFramesUntilClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := newDemoSession(ctx)
	if err != nil {
		t.Fatalf("newDemoSession: %v", err)
	}
	defer sess.Close()

	adapter := NewVSIPCallAdapter(sess, 8000)

	pushed, err := adapter.PumpAgentAudio(ctx, fakeAudioSource(2))
	if err != nil {
		t.Fatalf("PumpAgentAudio: %v", err)
	}
	if pushed != 2 {
		t.Fatalf("pushed = %d, want 2", pushed)
	}
}

func TestVSIPCallAdapter_PumpAudio_StopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := newDemoSession(ctx)
	if err != nil {
		t.Fatalf("newDemoSession: %v", err)
	}
	defer sess.Close()

	adapter := NewVSIPCallAdapter(sess, 8000)

	// A channel that's never closed and never sent on: PumpCallerAudio
	// must return once pumpCtx is cancelled rather than blocking forever.
	in := make(chan []byte)
	pumpCtx, pumpCancel := context.WithCancel(ctx)
	pumpCancel()

	pushed, err := adapter.PumpCallerAudio(pumpCtx, in)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if pushed != 0 {
		t.Fatalf("pushed = %d, want 0", pushed)
	}
}

func TestVSIPCallAdapter_Playback_DeliversUntilSessionCloses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := newDemoSession(ctx)
	if err != nil {
		t.Fatalf("newDemoSession: %v", err)
	}

	adapter := NewVSIPCallAdapter(sess, 8000)

	agentDone := make(chan int, 1)
	callerDone := make(chan int, 1)

	go func() {
		var delivered []tts.AudioChunk
		n, err := adapter.PumpAgentPlayback(ctx, func(chunk tts.AudioChunk) error {
			delivered = append(delivered, chunk)
			return nil
		})
		if err != nil {
			t.Errorf("PumpAgentPlayback: %v", err)
		}
		agentDone <- n
	}()
	go func() {
		var delivered []tts.AudioChunk
		n, err := adapter.PumpCallerPlayback(ctx, func(chunk tts.AudioChunk) error {
			delivered = append(delivered, chunk)
			return nil
		})
		if err != nil {
			t.Errorf("PumpCallerPlayback: %v", err)
		}
		callerDone <- n
	}()

	if _, err := adapter.PumpCallerAudio(ctx, fakeAudioSource(1)); err != nil {
		t.Fatalf("PumpCallerAudio: %v", err)
	}
	if _, err := adapter.PumpAgentAudio(ctx, fakeAudioSource(1)); err != nil {
		t.Fatalf("PumpAgentAudio: %v", err)
	}

	// Close flushes each leg's buffered utterance as a final transcript
	// (see langstream.Session.Close) and closes both outbound channels,
	// which is what makes the playback goroutines above return.
	if err := sess.Close(); err != nil {
		t.Fatalf("closing session: %v", err)
	}

	// The mock TTS synthesizer paces multiple chunks per utterance (see
	// pkg/tts/mock.go's mockWordsPerChunk), so at least one chunk (and
	// always exactly one IsFinal=true chunk) is what the contract
	// guarantees -- not a fixed count.
	select {
	case n := <-agentDone:
		if n < 1 {
			t.Fatalf("agent playback delivered %d chunk(s), want at least 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent playback pump did not return after session close")
	}

	select {
	case n := <-callerDone:
		if n < 1 {
			t.Fatalf("caller playback delivered %d chunk(s), want at least 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("caller playback pump did not return after session close")
	}
}

func TestVSIPCallAdapter_Playback_PropagatesOutError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := newDemoSession(ctx)
	if err != nil {
		t.Fatalf("newDemoSession: %v", err)
	}
	defer sess.Close()

	adapter := NewVSIPCallAdapter(sess, 8000)

	boom := errors.New("boom: pretend RTP write failed")
	done := make(chan error, 1)
	go func() {
		_, err := adapter.PumpAgentPlayback(ctx, func(chunk tts.AudioChunk) error {
			return boom
		})
		done <- err
	}()

	if _, err := adapter.PumpCallerAudio(ctx, fakeAudioSource(1)); err != nil {
		t.Fatalf("PumpCallerAudio: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("closing session: %v", err)
	}

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, boom) {
			t.Fatalf("err = %v, want wrapped %v", err, boom)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PumpAgentPlayback did not return after out returned an error")
	}
}
