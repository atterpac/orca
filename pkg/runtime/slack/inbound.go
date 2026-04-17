package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/atterpac/orca/pkg/orca"
)

// eventsHTTPHandler wires the Slack Events webhook and a healthz endpoint.
// Used when running in HTTP Events API mode.
func (s *Session) eventsHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /slack/events", s.slackEvents)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// slackEvents handles the Slack Events API webhook.
func (s *Session) slackEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	verifier, err := slackgo.NewSecretsVerifier(r.Header, s.cfg.SigningSecret)
	if err != nil {
		http.Error(w, "signing verifier", http.StatusBadRequest)
		return
	}
	if _, err := verifier.Write(body); err != nil {
		http.Error(w, "verifier write", http.StatusBadRequest)
		return
	}
	if err := verifier.Ensure(); err != nil {
		http.Error(w, "signature mismatch", http.StatusUnauthorized)
		return
	}

	ev, err := slackevents.ParseEvent(body, slackevents.OptionNoVerifyToken())
	if err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}

	switch ev.Type {
	case slackevents.URLVerification:
		var cr slackevents.ChallengeResponse
		_ = json.Unmarshal(body, &cr)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(cr.Challenge))
		return
	case slackevents.CallbackEvent:
		w.WriteHeader(http.StatusOK)
		go s.handleCallback(ev.InnerEvent)
		return
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// handleCallback dispatches an inner event. We care about user-authored
// message events in configured channels and thread replies under known
// decisions.
func (s *Session) handleCallback(inner slackevents.EventsAPIInnerEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch e := inner.Data.(type) {
	case *slackevents.MessageEvent:
		s.log.Info("slack message event received",
			"channel", e.Channel,
			"user", e.User,
			"bot_id", e.BotID,
			"subtype", e.SubType,
			"thread_ts", e.ThreadTimeStamp,
			"ts", e.TimeStamp,
			"text_preview", textPreview(e.Text, 80),
		)
		if e.BotID != "" || e.SubType != "" || e.User == "" {
			s.log.Info("slack message skipped (bot/edit/delete)", "bot_id", e.BotID, "subtype", e.SubType)
			return
		}
		s.handleMessage(ctx, e)
	default:
		s.log.Info("slack callback: unhandled inner event type", "type", inner.Type)
	}
}

// handleMessage routes a Slack message into orca based on config.
func (s *Session) handleMessage(ctx context.Context, e *slackevents.MessageEvent) {
	responderID := e.User
	responderName := s.users.Name(responderID)

	if e.ThreadTimeStamp != "" {
		corrID, known := s.corr.byThread(e.Channel, e.ThreadTimeStamp)
		s.log.Info("thread reply seen", "thread_ts", e.ThreadTimeStamp, "matched", known, "corr_id", corrID)
		if known && isDecisionID(corrID) {
			ans := parseHumanReply(e.Text, s.lookupDecisionOptions(corrID))
			ans.ResponderID = responderID
			ans.ResponderName = responderName

			if ans.Type == orca.AnswerFreeform {
				s.log.Info("forwarding clarification to agent", "id", corrID, "responder", responderName)
				if err := s.clarifyDecision(ctx, corrID, ans.Text, responderID, responderName); err != nil {
					s.log.Warn("clarify failed", "id", corrID, "err", err)
					s.postEphemeralReply(e.Channel, e.ThreadTimeStamp, "orca: couldn't forward message — "+err.Error())
					return
				}
				s.postEphemeralReply(e.Channel, e.ThreadTimeStamp, ackLineClarify(corrID, ans))
				return
			}

			s.log.Info("finalizing decision", "id", corrID, "type", ans.Type, "option", ans.Option, "responder", responderName)
			if err := s.answerDecision(ctx, corrID, ans); err != nil {
				s.log.Warn("answer decision failed", "id", corrID, "err", err)
				s.postEphemeralReply(e.Channel, e.ThreadTimeStamp, "orca: couldn't record answer — "+err.Error())
				return
			}
			s.postEphemeralReply(e.Channel, e.ThreadTimeStamp, ackLine(corrID, ans))
			return
		}
	}

	route, ok := s.resolveRoute(e.Channel, e.ChannelType == "im")
	if !ok {
		channelName := s.channels.Name(e.Channel)
		s.log.Info("no route configured for slack message — dropping",
			"channel_id", e.Channel,
			"channel_name", channelName,
			"channel_type", e.ChannelType,
			"hint", "add this channel to channels: in slack.yaml (either '#"+channelName+"' or '"+e.Channel+"')",
		)
		return
	}
	s.log.Info("slack message routed",
		"channel_id", e.Channel,
		"channel_name", s.channels.Name(e.Channel),
		"route_to_id", route.RouteToID,
		"route_to_tags", route.RouteToTags,
		"responder", responderName,
	)

	body, _ := json.Marshal(map[string]any{
		"text":            e.Text,
		"slack_channel":   e.Channel,
		"slack_thread_ts": e.ThreadTimeStamp,
		"responder_id":    responderID,
		"responder_name":  responderName,
	})

	threadTS := e.ThreadTimeStamp
	if threadTS == "" && e.ChannelType != "im" {
		threadTS = e.TimeStamp
	}
	var convID string
	if threadTS != "" {
		if existing, ok := s.corr.byThread(e.Channel, threadTS); ok && !isDecisionID(existing) {
			convID = existing
		}
	}
	if convID == "" {
		convID = conversationID(e.Channel, e.ThreadTimeStamp, e.TimeStamp)
	}
	s.corr.set(convID, e.Channel, threadTS, nil)

	msg := s.buildInboundMessage(route, body)
	msg.CorrelationID = convID
	s.log.Info("publishing inbound slack message", "conv_id", convID, "to", msg.To, "tags", msg.Tags, "responder", responderName)
	if err := s.publishToOrca(ctx, msg); err != nil {
		s.log.Warn("publish inbound failed", "err", err)
	}
}
