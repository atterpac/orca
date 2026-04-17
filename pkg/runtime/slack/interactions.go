package slack

import (
	"context"
	"strconv"
	"strings"

	slackgo "github.com/slack-go/slack"

	"github.com/atterpac/orca/pkg/orca"
)

// handleInteraction processes a Block Kit button click. The action_id
// encodes the decision id and the selected option (or a cancel signal).
func (s *Session) handleInteraction(ctx context.Context, cb slackgo.InteractionCallback) {
	if cb.Type != slackgo.InteractionTypeBlockActions {
		return
	}
	if len(cb.ActionCallback.BlockActions) == 0 {
		return
	}
	action := cb.ActionCallback.BlockActions[0]
	decID, kind, arg, ok := parseDecisionActionID(action.ActionID)
	if !ok {
		s.log.Info("ignoring unknown action_id", "action_id", action.ActionID)
		return
	}

	responderID := cb.User.ID
	responderName := cb.User.Name
	if cb.User.Profile.DisplayName != "" {
		responderName = cb.User.Profile.DisplayName
	} else if cb.User.Profile.RealName != "" {
		responderName = cb.User.Profile.RealName
	}

	var ans orca.DecisionAnswer
	switch kind {
	case "opt":
		opt, err := strconv.Atoi(arg)
		if err != nil {
			return
		}
		ans = orca.DecisionAnswer{
			Type:          orca.AnswerOption,
			Option:        opt,
			ResponderID:   responderID,
			ResponderName: responderName,
		}
	case "cancel":
		ans = orca.DecisionAnswer{
			Type:          orca.AnswerCancel,
			ResponderID:   responderID,
			ResponderName: responderName,
		}
	default:
		return
	}

	s.log.Info("button click answered decision",
		"id", decID, "type", ans.Type, "option", ans.Option, "responder", responderName)

	if err := s.answerDecision(ctx, decID, ans); err != nil {
		s.log.Warn("answer decision failed (button)", "id", decID, "err", err)
		return
	}

	ct, ok := s.corr.byDecision(decID)
	if ok {
		s.postEphemeralReply(ct.Channel, ct.ThreadTS, ackLine(decID, ans))
	}
}

// parseDecisionActionID parses "orca-decision:<id>:opt:<N>" or
// "orca-decision:<id>:cancel".
func parseDecisionActionID(s string) (id, kind, arg string, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) < 3 || parts[0] != "orca-decision" {
		return "", "", "", false
	}
	id = parts[1]
	kind = parts[2]
	if len(parts) >= 4 {
		arg = parts[3]
	}
	return id, kind, arg, true
}
