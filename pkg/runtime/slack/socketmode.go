package slack

import (
	"context"
	"errors"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// runSocketMode opens a Slack Socket Mode websocket with the app-level
// token and dispatches inbound events through handleCallback /
// handleInteraction. socketmode auto-reconnects on transient failures.
func (s *Session) runSocketMode(ctx context.Context) error {
	if s.cfg.AppToken == "" {
		return errors.New("socket mode requires SLACK_APP_TOKEN")
	}
	// Re-construct the slack client with BOTH tokens so socketmode can
	// pick up the app-level token.
	s.slack = slackgo.New(
		s.cfg.BotToken,
		slackgo.OptionAppLevelToken(s.cfg.AppToken),
	)
	s.users = newUserCache(s.slack)
	s.channels = newChannelCache(s.slack)

	sc := socketmode.New(s.slack)

	go func() {
		for evt := range sc.Events {
			switch evt.Type {
			case socketmode.EventTypeConnecting:
				s.log.Info("slack socket connecting")
			case socketmode.EventTypeConnected:
				s.log.Info("slack socket connected")
			case socketmode.EventTypeHello:
				// ignore
			case socketmode.EventTypeEventsAPI:
				eapi, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				if evt.Request != nil {
					sc.Ack(*evt.Request)
				}
				if eapi.Type == slackevents.CallbackEvent {
					_, cancel := context.WithTimeout(ctx, 15*time.Second)
					s.handleCallback(eapi.InnerEvent)
					cancel()
				}
			case socketmode.EventTypeSlashCommand:
				if evt.Request != nil {
					sc.Ack(*evt.Request)
				}
			case socketmode.EventTypeInteractive:
				callback, ok := evt.Data.(slackgo.InteractionCallback)
				if !ok {
					if evt.Request != nil {
						sc.Ack(*evt.Request)
					}
					continue
				}
				if evt.Request != nil {
					sc.Ack(*evt.Request)
				}
				hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				s.handleInteraction(hctx, callback)
				cancel()
			case socketmode.EventTypeDisconnect:
				s.log.Warn("slack socket disconnected")
			case socketmode.EventTypeErrorBadMessage, socketmode.EventTypeErrorWriteFailed:
				s.log.Warn("slack socket error", "type", evt.Type)
			}
		}
	}()

	if err := sc.RunContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
