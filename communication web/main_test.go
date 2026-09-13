package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type roomResponse struct {
	Code                string `json:"code"`
	PeerID              string `json:"peerId"`
	OtherPeerID         string `json:"otherPeerId"`
	OtherSupportsWebRTC bool   `json:"otherSupportsWebRTC"`
}

func TestTwoPeersCanJoinAndExchangeSignals(t *testing.T) {
	server := httptest.NewServer(newChatServer().routes())
	defer server.Close()

	host := doJSON[roomResponse](t, http.MethodPost, server.URL+"/api/rooms?rtc=1", nil, http.StatusCreated)
	if len(host.Code) != 6 || host.PeerID == "" {
		t.Fatalf("unexpected host response: %+v", host)
	}

	guest := doJSON[roomResponse](t, http.MethodPost, server.URL+"/api/rooms/"+host.Code+"/join?rtc=1", nil, http.StatusOK)
	if guest.OtherPeerID != host.PeerID || guest.PeerID == "" {
		t.Fatalf("unexpected guest response: %+v", guest)
	}
	if !guest.OtherSupportsWebRTC {
		t.Fatal("guest was not told that the host supports WebRTC")
	}

	events := doJSON[struct {
		Events []signalEvent `json:"events"`
	}](t, http.MethodGet, server.URL+"/api/rooms/"+host.Code+"/events?peer="+host.PeerID, nil, http.StatusOK)
	if len(events.Events) != 1 || events.Events[0].Type != "peer-joined" || events.Events[0].From != guest.PeerID {
		t.Fatalf("host did not receive peer-joined event: %+v", events.Events)
	}

	offer := map[string]any{
		"to": guest.PeerID, "type": "offer", "payload": map[string]string{"type": "offer", "sdp": "test-sdp"},
	}
	doJSON[any](t, http.MethodPost, server.URL+"/api/rooms/"+host.Code+"/signal?peer="+host.PeerID, offer, http.StatusNoContent)

	guestEvents := doJSON[struct {
		Events []signalEvent `json:"events"`
	}](t, http.MethodGet, server.URL+"/api/rooms/"+host.Code+"/events?peer="+guest.PeerID, nil, http.StatusOK)
	if len(guestEvents.Events) != 1 || guestEvents.Events[0].Type != "offer" || guestEvents.Events[0].From != host.PeerID {
		t.Fatalf("guest did not receive offer: %+v", guestEvents.Events)
	}
}

func TestRoomRejectsThirdPeer(t *testing.T) {
	server := httptest.NewServer(newChatServer().routes())
	defer server.Close()

	host := doJSON[roomResponse](t, http.MethodPost, server.URL+"/api/rooms", nil, http.StatusCreated)
	doJSON[roomResponse](t, http.MethodPost, server.URL+"/api/rooms/"+host.Code+"/join", nil, http.StatusOK)
	doJSON[any](t, http.MethodPost, server.URL+"/api/rooms/"+host.Code+"/join", nil, http.StatusConflict)
}

func TestPeerCapabilityIsIncludedInJoinEvent(t *testing.T) {
	server := httptest.NewServer(newChatServer().routes())
	defer server.Close()

	host := doJSON[roomResponse](t, http.MethodPost, server.URL+"/api/rooms?rtc=1", nil, http.StatusCreated)
	guest := doJSON[roomResponse](t, http.MethodPost, server.URL+"/api/rooms/"+host.Code+"/join?rtc=0", nil, http.StatusOK)
	if !guest.OtherSupportsWebRTC {
		t.Fatal("expected the host WebRTC capability in the join response")
	}
	events := doJSON[struct {
		Events []signalEvent `json:"events"`
	}](t, http.MethodGet, server.URL+"/api/rooms/"+host.Code+"/events?peer="+host.PeerID, nil, http.StatusOK)
	var payload struct {
		SupportsWebRTC bool `json:"supportsWebRTC"`
	}
	if err := json.Unmarshal(events.Events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SupportsWebRTC {
		t.Fatal("guest capability should be false for an HTTP LAN peer")
	}
}

func TestUnknownPeerCannotSignal(t *testing.T) {
	server := httptest.NewServer(newChatServer().routes())
	defer server.Close()

	host := doJSON[roomResponse](t, http.MethodPost, server.URL+"/api/rooms", nil, http.StatusCreated)
	guest := doJSON[roomResponse](t, http.MethodPost, server.URL+"/api/rooms/"+host.Code+"/join", nil, http.StatusOK)
	input := map[string]any{"to": guest.PeerID, "type": "offer", "payload": map[string]string{"sdp": "nope"}}
	doJSON[any](t, http.MethodPost, server.URL+"/api/rooms/"+host.Code+"/signal?peer=unknown", input, http.StatusForbidden)
}

func TestPeersCanUseLocalRelay(t *testing.T) {
	server := httptest.NewServer(newChatServer().routes())
	defer server.Close()

	host := doJSON[roomResponse](t, http.MethodPost, server.URL+"/api/rooms?rtc=0", nil, http.StatusCreated)
	guest := doJSON[roomResponse](t, http.MethodPost, server.URL+"/api/rooms/"+host.Code+"/join?rtc=0", nil, http.StatusOK)

	chat := map[string]any{
		"to": guest.PeerID, "type": "relay-chat", "payload": map[string]string{
			"text": "hello through the local relay", "sentAt": "2026-09-13T12:00:00Z",
		},
	}
	doJSON[any](t, http.MethodPost, server.URL+"/api/rooms/"+host.Code+"/signal?peer="+host.PeerID, chat, http.StatusNoContent)

	guestEvents := doJSON[struct {
		Events []signalEvent `json:"events"`
	}](t, http.MethodGet, server.URL+"/api/rooms/"+host.Code+"/events?peer="+guest.PeerID, nil, http.StatusOK)
	if len(guestEvents.Events) != 1 || guestEvents.Events[0].Type != "relay-chat" {
		t.Fatalf("guest did not receive relayed chat: %+v", guestEvents.Events)
	}
}

func TestStartupURLs(t *testing.T) {
	local, lan := startupURLs(":8080")
	if local != "http://localhost:8080" || lan != "http://<this-computer-ip>:8080" {
		t.Fatalf("unexpected wildcard URLs: %q, %q", local, lan)
	}
	local, lan = startupURLs("127.0.0.1:9090")
	if local != "http://127.0.0.1:9090" || lan != local {
		t.Fatalf("unexpected explicit-host URLs: %q, %q", local, lan)
	}
}

func doJSON[T any](t *testing.T, method, url string, body any, wantStatus int) T {
	t.Helper()
	var requestBody *bytes.Reader
	if body == nil {
		requestBody = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, url, requestBody)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s: got %d, want %d", method, url, response.StatusCode, wantStatus)
	}
	var result T
	if response.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}
