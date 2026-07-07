package asr

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestMockRecognizer_SupportedLanguages(t *testing.T) {
	r := NewMockRecognizer()
	langs := r.SupportedLanguages()

	want := map[Language]bool{"en": false, "hi": false}
	for _, l := range langs {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for l, found := range want {
		if !found {
			t.Errorf("expected SupportedLanguages() to include %q, got %v", l, langs)
		}
	}
}

func TestMockRecognizer_StartStream_UnsupportedLanguage(t *testing.T) {
	r := NewMockRecognizer("en", "hi")
	if _, err := r.StartStream(context.Background(), "fr"); err == nil {
		t.Fatal("expected error for unsupported language, got nil")
	}
}

func TestMockRecognizer_EndToEnd(t *testing.T) {
	r := NewMockRecognizer()
	ctx := context.Background()

	sess, err := r.StartStream(ctx, "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	// Push enough audio to trigger at least one automatic (non-final)
	// flush: mockFlushBytes is 8000, so push 10000 bytes across frames.
	frame := AudioFrame{
		PCM:         make([]byte, 4000),
		SampleRate:  8000,
		TimestampMS: 0,
	}
	for i := 0; i < 3; i++ {
		if err := sess.PushAudio(ctx, frame); err != nil {
			t.Fatalf("PushAudio: %v", err)
		}
	}

	// Drain whatever was emitted so far (should be at least one partial).
	var got []Transcript
	drainTimeout := time.After(1 * time.Second)
loop:
	for {
		select {
		case tr, ok := <-sess.Transcripts():
			if !ok {
				break loop
			}
			got = append(got, tr)
			if len(got) >= 1 {
				// Non-blocking peek for any more that are already queued.
				select {
				case tr2, ok2 := <-sess.Transcripts():
					if ok2 {
						got = append(got, tr2)
					}
				default:
				}
				break loop
			}
		case <-drainTimeout:
			break loop
		}
	}

	if len(got) == 0 {
		t.Fatal("expected at least one transcript after pushing audio past the flush threshold")
	}
	for _, tr := range got {
		if tr.Text == "" {
			t.Error("expected non-empty transcript text")
		}
		if tr.Language != "en" {
			t.Errorf("expected language 'en', got %q", tr.Language)
		}
	}

	// Close should flush any remaining buffered audio as a final
	// transcript and then close the channel, without hanging or panicking.
	closeDone := make(chan error, 1)
	go func() { closeDone <- sess.Close() }()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() hung")
	}

	// Drain remaining transcripts (should include a final one) until the
	// channel closes.
	var sawFinal bool
	for tr := range sess.Transcripts() {
		if tr.IsFinal {
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Error("expected a final transcript to be emitted on Close")
	}

	// Calling Close again must not panic or hang.
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}

func TestMockRecognizer_HindiPhrase(t *testing.T) {
	r := NewMockRecognizer()
	ctx := context.Background()

	sess, err := r.StartStream(ctx, "hi")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	defer sess.Close()

	// Nothing pushed yet, so Close() flushing on empty session (seq==0)
	// should produce exactly one final transcript with the Hindi phrase.
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tr, ok := <-sess.Transcripts()
	if !ok {
		t.Fatal("expected a transcript on close-of-empty-session, channel was empty/closed")
	}
	if !tr.IsFinal {
		t.Error("expected final transcript")
	}
	if tr.Language != "hi" {
		t.Errorf("expected language 'hi', got %q", tr.Language)
	}
	if tr.Text == "" {
		t.Error("expected non-empty Hindi transcript text")
	}
}

// TestMockRecognizer_ConcurrentPushAndRead exercises PushAudio (writer) and
// Transcripts (reader) concurrently, plus a Close from a third goroutine,
// to be checked under `go test -race`.
func TestMockRecognizer_ConcurrentPushAndRead(t *testing.T) {
	r := NewMockRecognizer()
	ctx := context.Background()

	sess, err := r.StartStream(ctx, "en")
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}

	var readerWG sync.WaitGroup
	var received int
	var mu sync.Mutex

	// Reader goroutine: drains until the channel is closed by Close().
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for range sess.Transcripts() {
			mu.Lock()
			received++
			mu.Unlock()
		}
	}()

	// Writer goroutine: pushes audio concurrently with the reader above.
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		frame := AudioFrame{PCM: make([]byte, 1000), SampleRate: 8000}
		for i := 0; i < 50; i++ {
			_ = sess.PushAudio(ctx, frame)
		}
	}()
	writerWG.Wait()

	// Now close the session: this flushes any remainder and closes the
	// channel, which unblocks the reader goroutine's range loop.
	closeDone := make(chan error, 1)
	go func() { closeDone <- sess.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() hung")
	}

	select {
	case <-doneCh(&readerWG):
	case <-time.After(2 * time.Second):
		t.Fatal("reader goroutine did not finish after Close()")
	}

	mu.Lock()
	defer mu.Unlock()
	if received == 0 {
		t.Error("expected at least one transcript to have been received")
	}
}

// doneCh adapts a *sync.WaitGroup into a channel that is closed once the
// group's Wait() returns, so it can be used in a select with a timeout.
func doneCh(wg *sync.WaitGroup) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		wg.Wait()
		close(ch)
	}()
	return ch
}
