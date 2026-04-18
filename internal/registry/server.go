package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/decisions"
	"github.com/atterpac/orca/internal/discussions"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/supervisor"
	"github.com/atterpac/orca/pkg/orca"
)

type Server struct {
	sup         *supervisor.Supervisor
	bus         bus.Bus
	events      *events.Bus
	mux         *http.ServeMux
	bridge      *bridgeEndpoint
	decisions   *decisions.Registry
	discussions *discussions.Registry
}

// SetDiscussions wires the discussion registry so /discussions endpoints
// light up.
func (s *Server) SetDiscussions(reg *discussions.Registry) {
	s.discussions = reg
	s.mux.HandleFunc("GET /discussions", s.listDiscussions)
	s.mux.HandleFunc("GET /discussions/{id}", s.getDiscussion)
	s.mux.HandleFunc("POST /discussions/{id}/close", s.closeDiscussion)
}

func (s *Server) listDiscussions(w http.ResponseWriter, r *http.Request) {
	if s.discussions == nil {
		http.Error(w, "discussions not configured", http.StatusNotImplemented)
		return
	}
	writeJSON(w, http.StatusOK, s.discussions.List())
}

func (s *Server) getDiscussion(w http.ResponseWriter, r *http.Request) {
	if s.discussions == nil {
		http.Error(w, "discussions not configured", http.StatusNotImplemented)
		return
	}
	id := r.PathValue("id")
	d, ok := s.discussions.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("discussion %s not found", id))
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) closeDiscussion(w http.ResponseWriter, r *http.Request) {
	if s.discussions == nil {
		http.Error(w, "discussions not configured", http.StatusNotImplemented)
		return
	}
	id := r.PathValue("id")
	if err := s.discussions.Close(id); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetDecisions wires in the decision registry so /decisions endpoints light up.
func (s *Server) SetDecisions(reg *decisions.Registry) {
	s.decisions = reg
	s.mux.HandleFunc("POST /decisions", s.createDecision)
	s.mux.HandleFunc("GET /decisions", s.listDecisions)
	s.mux.HandleFunc("GET /decisions/{id}", s.getDecision)
	s.mux.HandleFunc("POST /decisions/{id}/answer", s.answerDecision)
	s.mux.HandleFunc("POST /decisions/{id}/clarify", s.clarifyDecision)
}

func New(sup *supervisor.Supervisor, b bus.Bus, ev *events.Bus) *Server {
	s := &Server{sup: sup, bus: b, events: ev, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /agents", s.listAgents)
	s.mux.HandleFunc("POST /agents", s.spawnAgent)
	s.mux.HandleFunc("GET /agents/{id}", s.getAgent)
	s.mux.HandleFunc("DELETE /agents/{id}", s.killAgent)
	s.mux.HandleFunc("GET /agents/{id}/usage", s.agentUsage)
	s.mux.HandleFunc("POST /agents/{id}/message", s.sendMessage)
	s.mux.HandleFunc("GET /agents/{id}/events", s.agentEvents)
	s.mux.HandleFunc("GET /agents/{id}/transcript", s.agentTranscript)
	s.mux.HandleFunc("GET /tasks/{id}/timeline", s.taskTimeline)
	s.mux.HandleFunc("POST /messages", s.dispatchMessage)
	s.mux.HandleFunc("GET /tasks", s.listTasks)
	s.mux.HandleFunc("POST /tasks", s.openTask)
	s.mux.HandleFunc("GET /tasks/{id}", s.getTask)
	s.mux.HandleFunc("DELETE /tasks/{id}", s.closeTask)
	s.mux.HandleFunc("GET /usage", s.aggregateUsage)
	s.mux.HandleFunc("GET /runtimes", s.runtimes)
	s.mux.HandleFunc("GET /events", s.allEvents)
	s.mux.HandleFunc("POST /validate/agent-spec", s.validateAgentSpec)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
}

func (s *Server) validateAgentSpec(w http.ResponseWriter, r *http.Request) {
	var spec orca.AgentSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rts := s.sup.Runtimes()
	known := make([]string, 0, len(rts))
	for _, c := range rts {
		// RuntimeCaps doesn't carry the name; fall back to a small fixed set.
		_ = c
	}
	// Pull names from the supervisor itself.
	if names := s.sup.RuntimeNames(); len(names) > 0 {
		known = names
	}
	ctx := orca.ValidationContext{
		KnownRuntimes: known,
		AgentExists: func(id string) bool {
			_, ok := s.sup.Get(id)
			return ok
		},
		TaskExists: func(id string) bool {
			_, ok := s.sup.GetTask(id)
			return ok
		},
		StrictRoleTemplate: os.Getenv("ORCA_REQUIRE_ROLE_TEMPLATE") == "1",
	}
	errs := orca.ValidateSpec(spec, ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"errors":      errs,
		"fatal_count": orca.FatalCount(errs),
		"valid":       orca.FatalCount(errs) == 0,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	tagsParam := r.URL.Query().Get("tags")
	if tagsParam == "" {
		writeJSON(w, http.StatusOK, s.sup.List())
		return
	}
	var tags []string
	for t := range strings.SplitSeq(tagsParam, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			tags = append(tags, t)
		}
	}
	ids := s.sup.FindByTags(tags, false)
	out := make([]orca.AgentInfo, 0, len(ids))
	for _, id := range ids {
		if info, ok := s.sup.Get(id); ok {
			out = append(out, info)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) dispatchMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From          string            `json:"from"`
		To            string            `json:"to,omitempty"`
		Tags          []string          `json:"tags,omitempty"`
		Mode          orca.DispatchMode `json:"mode,omitempty"`
		Kind          orca.MessageKind  `json:"kind"`
		Body          json.RawMessage   `json:"body"`
		CorrelationID string            `json:"correlation_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.From == "" {
		body.From = "external"
	}
	if body.Kind == "" {
		body.Kind = orca.KindRequest
	}
	m := orca.Message{
		From: body.From, To: body.To, Tags: body.Tags, Mode: body.Mode,
		Kind: body.Kind, Body: body.Body, CorrelationID: body.CorrelationID,
		Timestamp: time.Now(),
	}

	if len(m.Tags) > 0 {
		targets, err := s.sup.DispatchTagged(r.Context(), m)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		s.events.Emit(orca.Event{Kind: orca.EvtMessageSent, AgentID: body.From, Payload: map[string]any{
			"tags": m.Tags, "mode": string(m.Mode), "targets": targets, "kind": body.Kind,
		}})
		writeJSON(w, http.StatusAccepted, map[string]any{"targets": targets})
		return
	}
	if m.To == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("either `to` or `tags` is required"))
		return
	}
	if err := s.sup.DispatchDirect(r.Context(), m); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	s.events.Emit(orca.Event{Kind: orca.EvtMessageSent, AgentID: body.From, Payload: map[string]any{
		"to": m.To, "kind": body.Kind,
	}})
	writeJSON(w, http.StatusAccepted, map[string]any{"targets": []string{m.To}})
}

func (s *Server) spawnAgent(w http.ResponseWriter, r *http.Request) {
	var spec orca.AgentSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	info, err := s.sup.Spawn(r.Context(), spec)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, ok := s.sup.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("agent %s not found", id))
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) killAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.sup.Kill(id); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, ok := s.sup.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("agent %s not found", id))
		return
	}
	writeJSON(w, http.StatusOK, info.Usage)
}

func (s *Server) aggregateUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"total":     s.sup.AggregateUsage(),
		"per_agent": s.sup.List(),
	})
}

func (s *Server) runtimes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sup.Runtimes())
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") == "true" || r.URL.Query().Get("include_closed") == "true" {
		writeJSON(w, http.StatusOK, s.sup.ListAllTasks())
		return
	}
	writeJSON(w, http.StatusOK, s.sup.ListTasks())
}

func (s *Server) openTask(w http.ResponseWriter, r *http.Request) {
	var req orca.OpenTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, err := s.sup.OpenTask(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := s.sup.GetTask(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("task %s not found", id))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) createDecision(w http.ResponseWriter, r *http.Request) {
	if s.decisions == nil {
		http.Error(w, "decisions not configured", http.StatusNotImplemented)
		return
	}
	var req orca.AskHumanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	d, err := s.decisions.Ask(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) listDecisions(w http.ResponseWriter, r *http.Request) {
	if s.decisions == nil {
		http.Error(w, "decisions not configured", http.StatusNotImplemented)
		return
	}
	writeJSON(w, http.StatusOK, s.decisions.Pending())
}

func (s *Server) getDecision(w http.ResponseWriter, r *http.Request) {
	if s.decisions == nil {
		http.Error(w, "decisions not configured", http.StatusNotImplemented)
		return
	}
	id := r.PathValue("id")
	d, ok := s.decisions.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("decision %s not found", id))
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) answerDecision(w http.ResponseWriter, r *http.Request) {
	if s.decisions == nil {
		http.Error(w, "decisions not configured", http.StatusNotImplemented)
		return
	}
	id := r.PathValue("id")
	var ans orca.DecisionAnswer
	if err := json.NewDecoder(r.Body).Decode(&ans); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.decisions.Answer(r.Context(), id, ans); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clarifyDecision forwards a human's free-form reply to the asking agent
// without closing the decision. The thread stays open for further turns.
func (s *Server) clarifyDecision(w http.ResponseWriter, r *http.Request) {
	if s.decisions == nil {
		http.Error(w, "decisions not configured", http.StatusNotImplemented)
		return
	}
	id := r.PathValue("id")
	var body struct {
		Text          string `json:"text"`
		ResponderID   string `json:"responder_id"`
		ResponderName string `json:"responder_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.decisions.Clarify(r.Context(), id, body.Text, body.ResponderID, body.ResponderName); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) closeTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	remove := r.URL.Query().Get("remove_worktree") != "false"
	t, err := s.sup.CloseTask(id, remove)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		From          string           `json:"from"`
		Body          json.RawMessage  `json:"body"`
		Kind          orca.MessageKind `json:"kind"`
		CorrelationID string           `json:"correlation_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.From == "" {
		body.From = "external"
	}
	if body.Kind == "" {
		body.Kind = orca.KindRequest
	}
	m := orca.Message{
		From:          body.From,
		To:            id,
		Kind:          body.Kind,
		Body:          body.Body,
		CorrelationID: body.CorrelationID,
		Timestamp:     time.Now(),
	}
	if err := s.sup.DispatchDirect(r.Context(), m); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	s.events.Emit(orca.Event{Kind: orca.EvtMessageSent, AgentID: body.From, Payload: map[string]any{
		"to":             id,
		"kind":           body.Kind,
		"correlation_id": body.CorrelationID,
	}})
	writeJSON(w, http.StatusAccepted, m)
}

func (s *Server) agentEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.streamEvents(w, r, events.Filter{AgentID: id})
}

// agentTranscript returns the per-agent ring buffer of recent events.
// Query params:
//   - limit: int (default 100, max 500)
//   - kinds: comma-separated EventKind filter
func (s *Server) agentTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	if limit > 500 {
		limit = 500
	}
	var kinds []orca.EventKind
	if v := r.URL.Query().Get("kinds"); v != "" {
		for k := range strings.SplitSeq(v, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				kinds = append(kinds, orca.EventKind(k))
			}
		}
	}
	writeJSON(w, http.StatusOK, s.sup.Transcript(id, limit, kinds))
}

// taskTimeline reconstructs all events touching a task, merged across
// participants and sorted chronologically.
func (s *Server) taskTimeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.sup.GetTask(id); !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("task %s not found", id))
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	writeJSON(w, http.StatusOK, s.sup.TaskTimeline(id, limit))
}

func (s *Server) allEvents(w http.ResponseWriter, r *http.Request) {
	kindsParam := r.URL.Query().Get("kinds")
	var kinds []orca.EventKind
	if kindsParam != "" {
		for k := range strings.SplitSeq(kindsParam, ",") {
			kinds = append(kinds, orca.EventKind(strings.TrimSpace(k)))
		}
	}
	s.streamEvents(w, r, events.Filter{Kinds: kinds})
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, f events.Filter) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ch, _ := s.events.Subscribe(ctx, f)

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				// Malformed event payload. Skip it rather than killing
				// the stream — other events will still flow.
				continue
			}
			// Build the SSE frame in one slice so a write failure is
			// detected once and aborts the whole frame atomically.
			frame := make([]byte, 0, len(data)+32+len(ev.Kind))
			frame = append(frame, "event: "...)
			frame = append(frame, ev.Kind...)
			frame = append(frame, "\ndata: "...)
			frame = append(frame, data...)
			frame = append(frame, "\n\n"...)
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
