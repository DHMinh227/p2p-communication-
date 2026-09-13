package main

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxSignalBody  = 64 << 10
	roomLifetime   = 12 * time.Hour
	eventRetention = 5 * time.Minute
	pollTimeout    = 20 * time.Second
)

//go:embed web/*
var webFiles embed.FS

type signalEvent struct {
	ID        int64           `json:"id"`
	From      string          `json:"from"`
	To        string          `json:"to"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	createdAt time.Time
}

type room struct {
	createdAt    time.Time
	lastActivity time.Time
	peers        map[string]peerInfo
	events       []signalEvent
	nextEventID  int64
	changed      chan struct{}
}

type peerInfo struct {
	supportsWebRTC bool
}

type chatServer struct {
	mu    sync.Mutex
	rooms map[string]*room
	now   func() time.Time
}

func newChatServer() *chatServer {
	return &chatServer{rooms: make(map[string]*room), now: time.Now}
}

func (s *chatServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/rooms", s.createRoom)
	mux.HandleFunc("POST /api/rooms/{code}/join", s.joinRoom)
	mux.HandleFunc("POST /api/rooms/{code}/signal", s.postSignal)
	mux.HandleFunc("GET /api/rooms/{code}/events", s.getEvents)
	mux.HandleFunc("POST /api/rooms/{code}/leave", s.leaveRoom)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(assets)))

	return securityHeaders(mux)
}

func (s *chatServer) createRoom(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredRoomsLocked()

	code, err := s.uniqueRoomCodeLocked()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create a room")
		return
	}
	peerID, err := randomToken(18)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create a peer identity")
		return
	}
	now := s.now()
	s.rooms[code] = &room{
		createdAt:    now,
		lastActivity: now,
		peers: map[string]peerInfo{
			peerID: {supportsWebRTC: r.URL.Query().Get("rtc") == "1"},
		},
		changed: make(chan struct{}),
	}
	writeJSON(w, http.StatusCreated, map[string]string{"code": code, "peerId": peerID})
}

func (s *chatServer) joinRoom(w http.ResponseWriter, r *http.Request) {
	code := normalizeCode(r.PathValue("code"))
	supportsWebRTC := r.URL.Query().Get("rtc") == "1"

	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredRoomsLocked()

	rm, ok := s.rooms[code]
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if len(rm.peers) >= 2 {
		writeError(w, http.StatusConflict, "this room already has two people")
		return
	}

	peerID, err := randomToken(18)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create a peer identity")
		return
	}
	var hostPeerID string
	var hostInfo peerInfo
	for id, info := range rm.peers {
		hostPeerID = id
		hostInfo = info
		break
	}
	rm.peers[peerID] = peerInfo{supportsWebRTC: supportsWebRTC}
	rm.lastActivity = s.now()
	joinPayload, _ := json.Marshal(map[string]bool{"supportsWebRTC": supportsWebRTC})
	s.appendEventLocked(rm, signalEvent{
		From:    peerID,
		To:      hostPeerID,
		Type:    "peer-joined",
		Payload: joinPayload,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"code":                code,
		"peerId":              peerID,
		"otherPeerId":         hostPeerID,
		"otherSupportsWebRTC": hostInfo.supportsWebRTC,
	})
}

func (s *chatServer) postSignal(w http.ResponseWriter, r *http.Request) {
	code := normalizeCode(r.PathValue("code"))
	from := r.URL.Query().Get("peer")
	var input struct {
		To      string          `json:"to"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		return
	}
	if !validSignalType(input.Type) || len(input.Payload) == 0 {
		writeError(w, http.StatusBadRequest, "invalid signal")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rm, ok := s.rooms[code]
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if _, ok := rm.peers[from]; !ok {
		writeError(w, http.StatusForbidden, "unknown peer")
		return
	}
	if input.To == from {
		writeError(w, http.StatusBadRequest, "cannot signal yourself")
		return
	}
	if _, ok := rm.peers[input.To]; !ok {
		writeError(w, http.StatusBadRequest, "the other peer is not in this room")
		return
	}

	rm.lastActivity = s.now()
	s.appendEventLocked(rm, signalEvent{
		From: from, To: input.To, Type: input.Type, Payload: input.Payload,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *chatServer) getEvents(w http.ResponseWriter, r *http.Request) {
	code := normalizeCode(r.PathValue("code"))
	peerID := r.URL.Query().Get("peer")
	var since int64
	if raw := r.URL.Query().Get("since"); raw != "" {
		if _, err := fmt.Sscan(raw, &since); err != nil || since < 0 {
			writeError(w, http.StatusBadRequest, "invalid event cursor")
			return
		}
	}

	timer := time.NewTimer(pollTimeout)
	defer timer.Stop()
	for {
		s.mu.Lock()
		rm, ok := s.rooms[code]
		if !ok {
			s.mu.Unlock()
			writeError(w, http.StatusNotFound, "room not found")
			return
		}
		if _, ok := rm.peers[peerID]; !ok {
			s.mu.Unlock()
			writeError(w, http.StatusForbidden, "unknown peer")
			return
		}
		events := eventsForPeer(rm.events, peerID, since)
		changed := rm.changed
		s.mu.Unlock()

		if len(events) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"events": events})
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			writeJSON(w, http.StatusOK, map[string]any{"events": []signalEvent{}})
			return
		case <-changed:
		}
	}
}

func (s *chatServer) leaveRoom(w http.ResponseWriter, r *http.Request) {
	code := normalizeCode(r.PathValue("code"))
	peerID := r.URL.Query().Get("peer")

	s.mu.Lock()
	defer s.mu.Unlock()
	rm, ok := s.rooms[code]
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, ok := rm.peers[peerID]; !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	delete(rm.peers, peerID)
	if len(rm.peers) == 0 {
		close(rm.changed)
		delete(s.rooms, code)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for otherID := range rm.peers {
		s.appendEventLocked(rm, signalEvent{
			From: peerID, To: otherID, Type: "peer-left", Payload: json.RawMessage(`{}`),
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *chatServer) appendEventLocked(rm *room, event signalEvent) {
	rm.nextEventID++
	event.ID = rm.nextEventID
	event.createdAt = s.now()
	rm.events = append(rm.events, event)
	cutoff := event.createdAt.Add(-eventRetention)
	firstCurrent := 0
	for firstCurrent < len(rm.events) && rm.events[firstCurrent].createdAt.Before(cutoff) {
		firstCurrent++
	}
	if firstCurrent > 0 {
		rm.events = rm.events[firstCurrent:]
	}
	if len(rm.events) > 256 {
		rm.events = rm.events[len(rm.events)-256:]
	}
	close(rm.changed)
	rm.changed = make(chan struct{})
}

func (s *chatServer) uniqueRoomCodeLocked() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for range 20 {
		bytes := make([]byte, 6)
		if _, err := rand.Read(bytes); err != nil {
			return "", err
		}
		for i := range bytes {
			bytes[i] = alphabet[int(bytes[i])%len(alphabet)]
		}
		code := string(bytes)
		if _, exists := s.rooms[code]; !exists {
			return code, nil
		}
	}
	return "", errors.New("could not allocate a unique room code")
}

func (s *chatServer) removeExpiredRoomsLocked() {
	cutoff := s.now().Add(-roomLifetime)
	for code, rm := range s.rooms {
		if rm.lastActivity.Before(cutoff) {
			close(rm.changed)
			delete(s.rooms, code)
		}
	}
}

func eventsForPeer(events []signalEvent, peerID string, since int64) []signalEvent {
	result := make([]signalEvent, 0)
	for _, event := range events {
		if event.To == peerID && event.ID > since {
			result = append(result, event)
		}
	}
	return result
}

func normalizeCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func validSignalType(value string) bool {
	switch value {
	case "offer", "answer", "candidate", "relay-hello", "relay-chat":
		return true
	default:
		return false
	}
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxSignalBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request")
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return errors.New("multiple JSON values")
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func main() {
	address := flag.String("addr", ":8080", "address to listen on")
	flag.Parse()

	server := &http.Server{
		Addr:              *address,
		Handler:           newChatServer().routes(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	localURL, lanURL := startupURLs(*address)
	log.Printf("LocalChat is running at %s", localURL)
	log.Printf("Other devices on this Wi-Fi can use %s", lanURL)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func startupURLs(address string) (string, string) {
	if strings.HasPrefix(address, ":") {
		return "http://localhost" + address, "http://<this-computer-ip>" + address
	}
	return "http://" + address, "http://" + address
}
