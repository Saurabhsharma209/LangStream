package main

import (
	"reflect"
	"testing"

	"github.com/pion/webrtc/v3"
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
