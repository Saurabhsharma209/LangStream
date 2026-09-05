package main

import (
	"context"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"

	"github.com/exotel/langstream/pkg/webrtcgw"
)

func TestIceServerForURL_TURNGetsCredentialsWhenBothSet(t *testing.T) {
	got := iceServerForURL("turn:turn.example.com:3478", "alice", "s3cret")
	want := webrtc.ICEServer{
		URLs:       []string{"turn:turn.example.com:3478"},
		Username:   "alice",
		Credential: "s3cret",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestIceServerForURL_TURNSGetsCredentialsWhenBothSet(t *testing.T) {
	got := iceServerForURL("turns:turn.example.com:5349", "alice", "s3cret")
	if got.Username != "alice" || got.Credential != interface{}("s3cret") {
		t.Fatalf("expected turns: URL to get credentials, got %+v", got)
	}
}

func TestIceServerForURL_TURNUppercaseSchemeStillMatches(t *testing.T) {
	got := iceServerForURL("TURN:turn.example.com:3478", "alice", "s3cret")
	if got.Username != "alice" || got.Credential != interface{}("s3cret") {
		t.Fatalf("expected scheme match to be case-insensitive, got %+v", got)
	}
}

func TestIceServerForURL_STUNNeverGetsCredentials(t *testing.T) {
	got := iceServerForURL("stun:stun.l.google.com:19302", "alice", "s3cret")
	want := webrtc.ICEServer{URLs: []string{"stun:stun.l.google.com:19302"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stun: URL must never get credentials attached, got %+v, want %+v", got, want)
	}
}

func TestIceServerForURL_STUNSNeverGetsCredentials(t *testing.T) {
	got := iceServerForURL("stuns:stun.example.com:5349", "alice", "s3cret")
	if got.Username != "" || got.Credential != nil {
		t.Fatalf("stuns: URL must never get credentials attached, got %+v", got)
	}
}

func TestIceServerForURL_TURNWithoutCredentialsIsUnchanged(t *testing.T) {
	got := iceServerForURL("turn:turn.example.com:3478", "", "")
	want := webrtc.ICEServer{URLs: []string{"turn:turn.example.com:3478"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("turn: URL with no --turn-username/--turn-credential must be left anonymous, got %+v, want %+v", got, want)
	}
}

func TestIceServerForURL_TURNWithOnlyUsernameIsIgnored(t *testing.T) {
	// A half-supplied credential pair is treated as "not configured" --
	// a TURN server would reject a username with no credential anyway,
	// so silently sending one alone isn't useful and could be confusing.
	got := iceServerForURL("turn:turn.example.com:3478", "alice", "")
	if got.Username != "" || got.Credential != nil {
		t.Fatalf("expected no credentials attached when only username is set, got %+v", got)
	}
}

func TestIceServerForURL_TURNWithOnlyCredentialIsIgnored(t *testing.T) {
	got := iceServerForURL("turn:turn.example.com:3478", "", "s3cret")
	if got.Username != "" || got.Credential != nil {
		t.Fatalf("expected no credentials attached when only credential is set, got %+v", got)
	}
}

func TestBuildICEServers_MixedListAttachesCredentialsOnlyToTURN(t *testing.T) {
	got := buildICEServers("stun:stun.l.google.com:19302,turn:turn.example.com:3478,turns:turn.example.com:5349", "alice", "s3cret")
	if len(got) != 3 {
		t.Fatalf("expected 3 ICE servers, got %d: %+v", len(got), got)
	}
	if got[0].Username != "" || got[0].Credential != nil {
		t.Fatalf("stun: entry must not get credentials, got %+v", got[0])
	}
	if got[1].Username != "alice" || got[1].Credential != interface{}("s3cret") {
		t.Fatalf("turn: entry must get credentials, got %+v", got[1])
	}
	if got[2].Username != "alice" || got[2].Credential != interface{}("s3cret") {
		t.Fatalf("turns: entry must get credentials, got %+v", got[2])
	}
}

func TestBuildICEServers_DefaultSTUNOnlyBehaviorUnchangedWithNoTurnFlags(t *testing.T) {
	// Exactly today's pre-existing default: a bare STUN URL, no flags set
	// at all -- must produce the same single, anonymous ICEServer as
	// before --turn-username/--turn-credential existed.
	got := buildICEServers(defaultSTUNServer, "", "")
	want := []webrtc.ICEServer{{URLs: []string{defaultSTUNServer}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestBuildICEServers_EmptyStringDisablesICEEntirely(t *testing.T) {
	got := buildICEServers("", "alice", "s3cret")
	if len(got) != 0 {
		t.Fatalf("expected no ICE servers for an empty --stun value, got %+v", got)
	}
}

func TestBuildICEServers_WhitespaceAndEmptyEntriesAreSkipped(t *testing.T) {
	got := buildICEServers(" stun:a.example.com:19302 , , turn:b.example.com:3478 ", "u", "p")
	if len(got) != 2 {
		t.Fatalf("expected 2 ICE servers (blank entry skipped), got %d: %+v", len(got), got)
	}
	if got[0].URLs[0] != "stun:a.example.com:19302" {
		t.Fatalf("expected trimmed stun URL, got %q", got[0].URLs[0])
	}
	if got[1].URLs[0] != "turn:b.example.com:3478" || got[1].Username != "u" {
		t.Fatalf("expected trimmed turn URL with credentials, got %+v", got[1])
	}
}

// TestNewWebRTCServerSetsAllTimeouts is a regression test for a real gap
// SRE found (Sprint 32): the webrtc subcommand's signaling/static-client
// http.Server had no ReadTimeout/WriteTimeout/IdleTimeout/
// ReadHeaderTimeout set at all, unlike pkg/observability's
// NewDashboardServer which was hardened for the same resource-exhaustion
// class in Sprint 31. Fails if any of the four ever regresses back to the
// zero value ("no timeout").
func TestNewWebRTCServerSetsAllTimeouts(t *testing.T) {
	srv := newWebRTCServer("unused:0", http.NewServeMux())

	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want > 0", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 {
		t.Errorf("ReadTimeout = %v, want > 0 (unset means no timeout -- a slow-request-body client could hold a connection open indefinitely)", srv.ReadTimeout)
	}
	if srv.WriteTimeout <= 0 {
		t.Errorf("WriteTimeout = %v, want > 0 (unset means no timeout -- a slow reader on the response could hold a connection open indefinitely)", srv.WriteTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want > 0 (unset means no timeout -- an idle keep-alive connection could be held open forever)", srv.IdleTimeout)
	}
	if srv.Addr != "unused:0" {
		t.Errorf("Addr = %q, want %q", srv.Addr, "unused:0")
	}
}

// TestServeWebRTC_ServesAndShutsDownGracefully is serveWebRTC's analogue
// of main_test.go's TestServeDashboard_ServesAndShutsDownGracefully --
// before this test, serveWebRTC (the function runWebRTC actually blocks
// on, split out for exactly this kind of direct testing per its own doc
// comment) had zero coverage despite the `webrtc` subcommand's other
// pieces (iceServerForURL, buildICEServers, newWebRTCServer) all being
// well tested above. Confirms the signaling/static-client server actually
// accepts connections once serveWebRTC is running, and that it returns
// within a bounded time (no error) once ctx is cancelled.
func TestServeWebRTC_ServesAndShutsDownGracefully(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/", webrtcgw.StaticHandler())

	addr := freeAddr(t)
	srv := newWebRTCServer(addr, mux)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveWebRTC(ctx, srv)
	}()

	var resp *http.Response
	var getErr error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, getErr = http.Get("http://" + addr + "/")
		if getErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if getErr != nil {
		cancel()
		<-done
		t.Fatalf("GET / never succeeded: %v", getErr)
	}
	resp.Body.Close()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveWebRTC returned an error on graceful shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveWebRTC did not return within 3s of context cancellation")
	}
}

// TestServeWebRTC_ListenErrorSurfaced mirrors
// TestServeDashboard_ListenErrorSurfaced: if the signaling server's
// ListenAndServe fails immediately (address already in use), serveWebRTC
// must surface that error instead of blocking forever waiting on ctx.
func TestServeWebRTC_ListenErrorSurfaced(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	srv := newWebRTCServer(addr, http.NewServeMux())

	err = serveWebRTC(context.Background(), srv)
	if err == nil {
		t.Fatal("expected serveWebRTC to return an error when the address is already in use")
	}
}
