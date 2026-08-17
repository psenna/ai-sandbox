package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// modelState holds the stub model endpoint's canned reply text and request
// log.
type modelState struct {
	mu    sync.RWMutex
	reply string
	log   []RecordedRequest
}

func newModelState() *modelState {
	return &modelState{reply: "canned e2e model reply"}
}

func (s *modelState) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, RecordedRequest{Method: r.Method, Path: r.URL.Path, Time: time.Now().UTC()})
}

func (s *modelState) requests() []RecordedRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RecordedRequest, len(s.log))
	copy(out, s.log)
	return out
}

func (s *modelState) setReply(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply = text
}

func (s *modelState) getReply() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reply
}

type messagesResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Model      string           `json:"model"`
	Content    []messageContent `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      messagesUsage    `json:"usage"`
}

type messageContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messagesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// runModel starts the stub model endpoint and blocks until it exits. It
// speaks just enough of the Anthropic Messages API shape (POST
// /v1/messages, GET /v1/models) plus the Ollama tags shape (GET /api/tags)
// since internal/render/secret.go projects the class's Ollama base URL as
// ANTHROPIC_BASE_URL -- either client library could plausibly probe either
// shape.
func runModel() error {
	state := newModelState()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, messagesResponse{
			ID:         "msg_e2e",
			Type:       "message",
			Role:       "assistant",
			Model:      "e2e-stub",
			Content:    []messageContent{{Type: "text", Text: state.getReply()}},
			StopReason: "end_turn",
			Usage:      messagesUsage{InputTokens: 1, OutputTokens: 1},
		})
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]string{{"id": "e2e-stub", "type": "model"}},
		})
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"models": []map[string]string{{"name": "e2e-stub"}},
		})
	})
	mux.HandleFunc("PUT /_control/reply", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state.setReply(in.Text)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /_control/requests", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, state.requests())
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_control/requests" {
			state.record(r)
		}
		mux.ServeHTTP(w, r)
	})

	addr := ":8081"
	log.Printf("model: listening on %s", addr)
	return http.ListenAndServe(addr, handler)
}
