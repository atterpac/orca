package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/atterpac/orca/pkg/orca"
)

// Session is one live connection to a Slack workspace. It implements
// orca.Session: the supervisor delivers messages to it via Send, and
// it streams inbound Slack events back to the bus via deps.Bus.
type Session struct {
	id       string
	cfg      Config
	deps     Deps
	log      *slog.Logger

	slack    *slackgo.Client
	users    *userCache
	channels *channelCache
	corr     *correlation

	events chan orca.Event
	done   chan struct{}
	closed atomic.Bool

	cancel context.CancelFunc

	workspaceMu  sync.RWMutex
	workspaceURL string
}

func newSession(cfg Config, spec orca.AgentSpec, deps Deps) (*Session, error) {
	if cfg.BotToken == "" {
		return nil, errors.New("bot token required")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	clientOpts := []slackgo.Option{}
	if cfg.UseSocketMode() {
		clientOpts = append(clientOpts, slackgo.OptionAppLevelToken(cfg.AppToken))
	}
	client := slackgo.New(cfg.BotToken, clientOpts...)
	return &Session{
		id:       spec.ID,
		cfg:      cfg,
		deps:     deps,
		log:      logger.With("component", "slack", "agent_id", spec.ID),
		slack:    client,
		users:    newUserCache(client),
		channels: newChannelCache(client),
		corr:     newCorrelation(),
		events:   make(chan orca.Event, 64),
		done:     make(chan struct{}),
	}, nil
}

// start kicks off the slack inbound pipe (Socket Mode or HTTP) and
// returns. The session runs until Close is called.
func (s *Session) start(ctx context.Context) error {
	sCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	// Emit AgentReady so the supervisor records us as live.
	s.emit(orca.Event{Kind: orca.EvtAgentReady, AgentID: s.id, Payload: map[string]any{
		"runtime":         "slack",
		"socket_mode":     s.cfg.UseSocketMode(),
		"default_inbound": s.cfg.Routing.DefaultInboundKind,
	}})

	if s.cfg.UseSocketMode() {
		s.log.Info("slack inbound mode: Socket Mode (no public URL needed)")
		go s.runSocketMode(sCtx)
	} else {
		s.log.Info("slack inbound mode: HTTP Events API", "addr", s.cfg.HTTPListen)
		srv := &http.Server{
			Addr:              s.cfg.HTTPListen,
			Handler:           s.eventsHTTPHandler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Error("http events server", "err", err)
			}
		}()
		go func() {
			<-sCtx.Done()
			sd, c := context.WithTimeout(context.Background(), 3*time.Second)
			defer c()
			_ = srv.Shutdown(sd)
		}()
	}
	return nil
}

func (s *Session) ID() string             { return s.id }
func (s *Session) Usage() orca.TokenUsage { return orca.TokenUsage{} }

// Send is invoked by the supervisor when a bus message is routed to
// this slack agent. We dispatch by Message kind (decision / update /
// task_opened event / plain) and post the rendered payload to Slack.
func (s *Session) Send(ctx context.Context, m orca.Message) error {
	if s.closed.Load() {
		return errors.New("slack session closed")
	}
	s.handleOutbound(ctx, m)
	return nil
}

func (s *Session) Events(ctx context.Context) (<-chan orca.Event, error) { return s.events, nil }

func (s *Session) Interrupt(ctx context.Context) error { return s.Close() }
func (s *Session) Wait() error                         { <-s.done; return nil }

func (s *Session) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	exit := orca.Event{
		Kind:      orca.EvtAgentExited,
		AgentID:   s.id,
		Timestamp: time.Now(),
		Payload:   map[string]any{"reason": "slack session closed"},
	}
	select {
	case s.events <- exit:
	default:
	}
	close(s.events)
	close(s.done)
	return nil
}

func (s *Session) emit(e orca.Event) {
	if s.closed.Load() {
		return
	}
	if e.AgentID == "" {
		e.AgentID = s.id
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	select {
	case s.events <- e:
	default:
	}
}

// publishToOrca routes an inbound slack-derived message onto the bus.
func (s *Session) publishToOrca(ctx context.Context, m orca.Message) error {
	if m.From == "" {
		m.From = s.id
	}
	return s.deps.Bus.Publish(ctx, m)
}

// answerDecision finalizes a decision via the daemon's decisions registry.
func (s *Session) answerDecision(ctx context.Context, decisionID string, ans orca.DecisionAnswer) error {
	if s.deps.Decisions == nil {
		return errors.New("no decisions registry wired")
	}
	return s.deps.Decisions.Answer(ctx, decisionID, ans)
}

// clarifyDecision forwards a free-form reply without finalizing.
func (s *Session) clarifyDecision(ctx context.Context, decisionID, text, responderID, responderName string) error {
	if s.deps.Decisions == nil {
		return errors.New("no decisions registry wired")
	}
	return s.deps.Decisions.Clarify(ctx, decisionID, text, responderID, responderName)
}

// botIdentity prepends Username + Icon options when configured.
func (s *Session) botIdentity(opts []slackgo.MsgOption) []slackgo.MsgOption {
	if s.cfg.Username != "" {
		opts = append(opts, slackgo.MsgOptionUsername(s.cfg.Username))
	}
	switch {
	case s.cfg.IconEmoji != "":
		opts = append(opts, slackgo.MsgOptionIconEmoji(s.cfg.IconEmoji))
	case s.cfg.IconURL != "":
		opts = append(opts, slackgo.MsgOptionIconURL(s.cfg.IconURL))
	}
	return opts
}

// workspaceBaseURL returns the workspace's URL for permalinks. Cached.
func (s *Session) workspaceBaseURL(ctx context.Context) string {
	s.workspaceMu.RLock()
	u := s.workspaceURL
	s.workspaceMu.RUnlock()
	if u != "" {
		return u
	}
	info, err := s.slack.GetTeamInfoContext(ctx)
	if err != nil || info == nil {
		return ""
	}
	if info.Domain == "" {
		return ""
	}
	u = "https://" + info.Domain + ".slack.com"
	s.workspaceMu.Lock()
	s.workspaceURL = u
	s.workspaceMu.Unlock()
	return u
}

// slackMessageLink builds a permalink from a channel id + message ts.
func (s *Session) slackMessageLink(ctx context.Context, channelID, ts string) string {
	base := s.workspaceBaseURL(ctx)
	if base == "" || channelID == "" || ts == "" {
		return ""
	}
	noDot := strings.ReplaceAll(ts, ".", "")
	return base + "/archives/" + channelID + "/p" + noDot
}

// resolveRoute matches a slack channel id (or name) against routing rules.
func (s *Session) resolveRoute(channelID string, isDM bool) (Route, bool) {
	if isDM && s.cfg.Routing.DMToBot != nil {
		return *s.cfg.Routing.DMToBot, true
	}
	if route, ok := s.cfg.Routing.Channels[channelID]; ok {
		return route, true
	}
	name := s.channels.Name(channelID)
	if name != "" {
		for _, key := range []string{"#" + name, name} {
			if route, ok := s.cfg.Routing.Channels[key]; ok {
				return route, true
			}
		}
	}
	return Route{}, false
}

// lookupDecisionOptions reads back the option list we stashed when we
// rendered a decision, so thread replies parse against a known count.
func (s *Session) lookupDecisionOptions(decisionID string) []string {
	s.corr.mu.RLock()
	defer s.corr.mu.RUnlock()
	return s.corr.optionsByID[decisionID]
}

func (s *Session) postEphemeralReply(channel, threadTS, text string) {
	opts := s.botIdentity([]slackgo.MsgOption{
		slackgo.MsgOptionText(text, false),
		slackgo.MsgOptionTS(threadTS),
	})
	_, _, _ = s.slack.PostMessage(channel, opts...)
}

// buildInboundMessage constructs the orca.Message to publish for a Slack
// event. Direct-id routing wins over tag routing when both are set.
func (s *Session) buildInboundMessage(route Route, body json.RawMessage) orca.Message {
	kind := orca.MessageKind(s.cfg.Routing.DefaultInboundKind)
	if kind == "" {
		kind = orca.KindRequest
	}
	m := orca.Message{
		From: s.id,
		Kind: kind,
		Body: body,
	}
	switch {
	case route.RouteToID != "":
		m.To = route.RouteToID
	case len(route.RouteToTags) > 0:
		m.Tags = route.RouteToTags
		mode := route.RouteMode
		if mode == "" {
			mode = "any"
		}
		m.Mode = orca.DispatchMode(mode)
	}
	return m
}

func textPreview(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// formatPlainBody unwraps a JSON-string body or returns the raw bytes.
func formatPlainBody(m orca.Message) string {
	var s string
	if err := json.Unmarshal(m.Body, &s); err == nil && s != "" {
		return s
	}
	if len(m.Body) > 0 {
		return string(m.Body)
	}
	return fmt.Sprintf("(empty message from %s)", m.From)
}
