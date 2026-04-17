package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	slackgo "github.com/slack-go/slack"

	"github.com/atterpac/orca/pkg/orca"
)

// handleOutbound processes a message that orca delivered to us.
//   - KindDecision → structured human-in-loop prompt.
//   - KindUpdate   → structured progress post.
//   - KindEvent with body.type="task_opened" → styled task announcement,
//     stashes task_id as thread anchor, drops a pointer in the user's
//     original conversation thread.
//   - Anything else → plain message (threads into known correlation or
//     posts fresh to default_channel).
func (s *Session) handleOutbound(ctx context.Context, m orca.Message) {
	if m.Kind == orca.KindDecision {
		s.postDecision(ctx, m)
		return
	}
	if m.Kind == orca.KindUpdate {
		s.postUpdate(ctx, m)
		return
	}
	if m.Kind == orca.KindEvent && len(m.Body) > 0 {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(m.Body, &probe); err == nil && probe.Type == "task_opened" {
			s.postTaskOpened(ctx, m)
			return
		}
	}
	s.postPlainMessage(ctx, m)
}

// phaseEmoji picks a visual accent for an update phase.
func phaseEmoji(p orca.UpdatePhase) string {
	switch p {
	case orca.PhasePlanning:
		return "📋"
	case orca.PhaseImplementing:
		return "🔨"
	case orca.PhaseTesting:
		return "🧪"
	case orca.PhaseReviewing:
		return "🔍"
	case orca.PhaseInvestigating:
		return "🔎"
	case orca.PhaseDeploying:
		return "🚢"
	case orca.PhaseBlocked:
		return "⚠️"
	case orca.PhaseDone:
		return "✅"
	case orca.PhaseInfo:
		return "ℹ️"
	default:
		return "•"
	}
}

// severityPrefix returns a short leading marker when the severity
// warrants extra visual weight BEYOND what the phase emoji already conveys.
func severityPrefix(p orca.UpdatePhase, sev orca.UpdateSeverity) string {
	switch p {
	case orca.PhaseBlocked, orca.PhaseInvestigating:
		if sev == orca.UpdateWarn {
			return ""
		}
	case orca.PhaseDone:
		if sev == orca.UpdateSuccess {
			return ""
		}
	}
	switch sev {
	case orca.UpdateError:
		return "🔴 "
	case orca.UpdateWarn:
		return "⚠️ "
	default:
		return ""
	}
}

// postUpdate renders a structured Update into a threaded Slack message.
func (s *Session) postUpdate(ctx context.Context, m orca.Message) {
	var u orca.Update
	if err := json.Unmarshal(m.Body, &u); err != nil {
		s.log.Warn("update body parse failed", "err", err, "correlation_id", m.CorrelationID)
		return
	}

	channel := s.cfg.Routing.Outbound.DefaultChannel
	threadTS := ""
	haveThread := false
	if m.CorrelationID != "" {
		if ct, ok := s.corr.byDecision(m.CorrelationID); ok {
			channel = ct.Channel
			threadTS = ct.ThreadTS
			haveThread = true
		}
	}
	if channel == "" {
		channel = "#orca-activity"
	}

	blocks := renderUpdateBlocks(&u)
	fallback := fmt.Sprintf("%s%s %s",
		severityPrefix(u.Phase, u.Severity),
		phaseEmoji(u.Phase),
		capFirst(truncate(u.Title, 200)))

	opts := s.botIdentity([]slackgo.MsgOption{
		slackgo.MsgOptionText(fallback, false),
		slackgo.MsgOptionBlocks(blocks...),
		slackgo.MsgOptionDisableLinkUnfurl(),
	})
	if threadTS != "" {
		opts = append(opts, slackgo.MsgOptionTS(threadTS))
	}
	postedChannelID, postedTS, err := s.slack.PostMessage(channel, opts...)
	if err != nil {
		s.log.Warn("update post failed", "err", err, "correlation_id", m.CorrelationID, "phase", u.Phase)
		return
	}
	if !haveThread && m.CorrelationID != "" {
		s.corr.set(m.CorrelationID, postedChannelID, postedTS, nil)
	}
}

// renderUpdateBlocks produces a compact Block Kit layout for a progress post.
func renderUpdateBlocks(u *orca.Update) []slackgo.Block {
	var blocks []slackgo.Block

	title := capFirst(truncate(u.Title, 500))
	head := fmt.Sprintf("%s%s *%s*",
		severityPrefix(u.Phase, u.Severity),
		phaseEmoji(u.Phase),
		title)
	if u.AgentID != "" {
		head += "  ·  `" + u.AgentID + "`"
	}
	if badge := statusBadge(u.Status); badge != "" {
		head += "  " + badge
	}

	blocks = append(blocks, slackgo.NewSectionBlock(
		slackgo.NewTextBlockObject(slackgo.MarkdownType, head, false, false),
		nil, nil,
	))

	if len(u.Details) > 0 || len(u.Metrics) > 0 {
		var b strings.Builder
		for i, d := range u.Details {
			if i >= 5 {
				break
			}
			b.WriteString("• ")
			b.WriteString(truncate(d, 240))
			b.WriteString("\n")
		}
		if len(u.Metrics) > 0 {
			parts := make([]string, 0, len(u.Metrics))
			keys := make([]string, 0, len(u.Metrics))
			for k := range u.Metrics {
				keys = append(keys, k)
			}
			sortStrings(keys)
			for _, k := range keys {
				parts = append(parts, "*"+k+"*: `"+u.Metrics[k]+"`")
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(strings.Join(parts, "  ·  "))
		}
		if b.Len() > 0 {
			blocks = append(blocks, slackgo.NewSectionBlock(
				slackgo.NewTextBlockObject(slackgo.MarkdownType, b.String(), false, false),
				nil, nil,
			))
		}
	}

	if u.Link != "" {
		blocks = append(blocks, slackgo.NewContextBlock("update-link",
			slackgo.NewTextBlockObject(slackgo.MarkdownType, "🔗 <"+u.Link+"|link>", false, false),
		))
	}

	return blocks
}

// statusBadge returns a tiny pill for distinctive statuses.
func statusBadge(s orca.UpdateStatus) string {
	switch s {
	case "started":
		return "`Starting`"
	case "failed":
		return "`Failed`"
	default:
		return ""
	}
}

// capFirst upper-cases the first rune of s, leaving the rest untouched.
func capFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// sortStrings is a tiny in-place helper to avoid pulling in sort for one spot.
func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// postTaskOpened renders a task announcement as a styled top-level message
// in the configured channel, stashes task_id correlation, and drops a
// pointer in the user's original conversation thread if present.
func (s *Session) postTaskOpened(ctx context.Context, m orca.Message) {
	var body struct {
		Type              string `json:"type"`
		TaskID            string `json:"task_id"`
		Summary           string `json:"summary"`
		OpenedBy          string `json:"opened_by"`
		UserCorrelationID string `json:"user_correlation_id"`
		AnnounceChannel   string `json:"announce_channel"`
		WorktreePath      string `json:"worktree_path"`
		ArtifactDir       string `json:"artifact_dir"`
		Branch            string `json:"branch"`
	}
	if err := json.Unmarshal(m.Body, &body); err != nil {
		s.log.Warn("task_opened body parse failed", "err", err)
		return
	}

	channel := body.AnnounceChannel
	if channel == "" {
		channel = s.cfg.Routing.Outbound.DefaultChannel
	}
	if channel == "" {
		channel = "#orca-activity"
	}

	blocks := renderTaskOpenedBlocks(&body)
	fallback := fmt.Sprintf("🚀 Task %s opened: %s", body.TaskID, body.Summary)
	opts := s.botIdentity([]slackgo.MsgOption{
		slackgo.MsgOptionText(fallback, false),
		slackgo.MsgOptionBlocks(blocks...),
		slackgo.MsgOptionDisableLinkUnfurl(),
	})

	postedChannelID, ts, err := s.slack.PostMessage(channel, opts...)
	if err != nil {
		s.log.Warn("task announcement failed", "err", err, "task_id", body.TaskID)
		return
	}
	s.corr.set(body.TaskID, postedChannelID, ts, nil)
	link := s.slackMessageLink(ctx, postedChannelID, ts)
	s.log.Info("task announcement posted",
		"task_id", body.TaskID, "channel_id", postedChannelID, "ts", ts, "link", link)

	if body.UserCorrelationID != "" {
		ct, ok := s.corr.byDecision(body.UserCorrelationID)
		if ok {
			pointer := fmt.Sprintf("📎 Created task `%s` — follow progress here: %s", body.TaskID, link)
			if link == "" {
				pointer = fmt.Sprintf("📎 Created task `%s` — follow progress in #%s",
					body.TaskID, s.channels.Name(postedChannelID))
			}
			popts := s.botIdentity([]slackgo.MsgOption{
				slackgo.MsgOptionText(pointer, false),
				slackgo.MsgOptionTS(ct.ThreadTS),
				slackgo.MsgOptionDisableLinkUnfurl(),
			})
			if _, _, err := s.slack.PostMessage(ct.Channel, popts...); err != nil {
				s.log.Warn("pointer post failed", "err", err, "task_id", body.TaskID)
			}
		}
	}
}

// renderTaskOpenedBlocks — Block Kit layout for task announcements.
func renderTaskOpenedBlocks(d *struct {
	Type              string `json:"type"`
	TaskID            string `json:"task_id"`
	Summary           string `json:"summary"`
	OpenedBy          string `json:"opened_by"`
	UserCorrelationID string `json:"user_correlation_id"`
	AnnounceChannel   string `json:"announce_channel"`
	WorktreePath      string `json:"worktree_path"`
	ArtifactDir       string `json:"artifact_dir"`
	Branch            string `json:"branch"`
}) []slackgo.Block {
	var blocks []slackgo.Block

	blocks = append(blocks, slackgo.NewHeaderBlock(
		slackgo.NewTextBlockObject(slackgo.PlainTextType, "🚀  Task Opened", true, false),
	))

	meta := []*slackgo.TextBlockObject{
		slackgo.NewTextBlockObject(slackgo.MarkdownType, "*id:* `"+d.TaskID+"`", false, false),
	}
	if d.OpenedBy != "" {
		meta = append(meta, slackgo.NewTextBlockObject(slackgo.MarkdownType, "*opened by:* `"+d.OpenedBy+"`", false, false))
	}
	if d.Branch != "" {
		meta = append(meta, slackgo.NewTextBlockObject(slackgo.MarkdownType, "*branch:* `"+d.Branch+"`", false, false))
	}
	blocks = append(blocks, slackgo.NewContextBlock("task-meta", toContextElements(meta)...))

	blocks = append(blocks, slackgo.NewDividerBlock())

	summary := d.Summary
	if summary == "" {
		summary = "(no summary provided)"
	}
	blocks = append(blocks, slackgo.NewSectionBlock(
		slackgo.NewTextBlockObject(slackgo.MarkdownType, "*Request*\n"+truncate(summary, 2500), false, false),
		nil, nil,
	))

	if d.ArtifactDir != "" || d.WorktreePath != "" {
		var lines []string
		if d.ArtifactDir != "" {
			lines = append(lines, "• artifacts: `"+d.ArtifactDir+"`")
		}
		if d.WorktreePath != "" {
			lines = append(lines, "• worktree: `"+d.WorktreePath+"`")
		}
		blocks = append(blocks, slackgo.NewSectionBlock(
			slackgo.NewTextBlockObject(slackgo.MarkdownType, strings.Join(lines, "\n"), false, false),
			nil, nil,
		))
	}

	blocks = append(blocks, slackgo.NewContextBlock("task-footer",
		slackgo.NewTextBlockObject(slackgo.MarkdownType,
			"Progress updates will appear in this thread ↓", false, false),
	))

	return blocks
}

func (s *Session) postDecision(ctx context.Context, m orca.Message) {
	var d orca.Decision
	if err := json.Unmarshal(m.Body, &d); err != nil {
		s.log.Warn("decision body parse failed", "err", err, "id", m.CorrelationID)
		return
	}
	channel := d.Channel
	if channel == "" {
		channel = s.cfg.ResolvedChannelForDecisions()
	}

	blocks := renderDecisionBlocks(&d)
	fallback := renderDecision(&d)
	mention := ""
	if d.Severity == orca.SevCritical {
		mention = "<!channel> "
	}
	opts := s.botIdentity([]slackgo.MsgOption{
		slackgo.MsgOptionText(mention+fallback, false),
		slackgo.MsgOptionBlocks(blocks...),
		slackgo.MsgOptionDisableLinkUnfurl(),
	})
	postedChannelID, ts, err := s.slack.PostMessage(channel, opts...)
	if err != nil {
		s.log.Warn("decision post failed", "err", err, "id", d.ID)
		_ = s.answerDecision(ctx, d.ID, orca.DecisionAnswer{
			Type: orca.AnswerCancel,
			Note: "slack post failed: " + err.Error(),
		})
		return
	}

	s.corr.set(d.ID, postedChannelID, ts, d.Options)
	s.log.Info("decision posted", "id", d.ID, "channel_name", channel, "channel_id", postedChannelID, "thread_ts", ts)
}

// severityEmoji returns a visual signal for the human skimming the channel.
func severityEmoji(s orca.Severity) string {
	switch s {
	case orca.SevCritical:
		return "🚨"
	case orca.SevHigh:
		return "⚠️"
	case orca.SevMedium:
		return "🔔"
	case orca.SevLow:
		return "💬"
	default:
		return "🔔"
	}
}

// renderDecisionBlocks produces the Block Kit layout for a Decision post.
func renderDecisionBlocks(d *orca.Decision) []slackgo.Block {
	var blocks []slackgo.Block

	headerText := fmt.Sprintf("%s  Orca Decision", severityEmoji(d.Severity))
	blocks = append(blocks, slackgo.NewHeaderBlock(
		slackgo.NewTextBlockObject(slackgo.PlainTextType, headerText, true, false),
	))

	meta := []*slackgo.TextBlockObject{
		slackgo.NewTextBlockObject(slackgo.MarkdownType, "*severity:* "+strings.ToUpper(string(d.Severity)), false, false),
		slackgo.NewTextBlockObject(slackgo.MarkdownType, "*agent:* `"+d.AgentID+"`", false, false),
	}
	if d.TaskID != "" {
		meta = append(meta, slackgo.NewTextBlockObject(slackgo.MarkdownType, "*task:* `"+d.TaskID+"`", false, false))
	}
	meta = append(meta, slackgo.NewTextBlockObject(slackgo.MarkdownType, "*id:* `"+d.ID+"`", false, false))
	blocks = append(blocks, slackgo.NewContextBlock("meta", toContextElements(meta)...))

	blocks = append(blocks, slackgo.NewDividerBlock())

	blocks = append(blocks, slackgo.NewSectionBlock(
		slackgo.NewTextBlockObject(slackgo.MarkdownType,
			"*Question*\n"+truncate(d.Question, 2500), false, false),
		nil, nil,
	))

	if len(d.Context) > 0 {
		var b strings.Builder
		b.WriteString("*Context*\n")
		for i, c := range d.Context {
			if i >= 5 {
				break
			}
			b.WriteString("• ")
			b.WriteString(truncate(c, 200))
			b.WriteString("\n")
		}
		blocks = append(blocks, slackgo.NewSectionBlock(
			slackgo.NewTextBlockObject(slackgo.MarkdownType, b.String(), false, false),
			nil, nil,
		))
	}

	if len(d.Options) > 0 {
		var b strings.Builder
		b.WriteString("*Options*\n")
		for i, opt := range d.Options {
			fmt.Fprintf(&b, "*%d.* %s\n", i+1, truncate(opt, 250))
		}
		blocks = append(blocks, slackgo.NewSectionBlock(
			slackgo.NewTextBlockObject(slackgo.MarkdownType, b.String(), false, false),
			nil, nil,
		))

		btns := make([]slackgo.BlockElement, 0, len(d.Options)+1)
		for i := range d.Options {
			n := i + 1
			btns = append(btns, slackgo.NewButtonBlockElement(
				actionIDOption(d.ID, n),
				fmt.Sprintf("%d", n),
				slackgo.NewTextBlockObject(slackgo.PlainTextType, fmt.Sprintf("%d", n), false, false),
			))
		}
		cancelBtn := slackgo.NewButtonBlockElement(
			actionIDCancel(d.ID),
			"cancel",
			slackgo.NewTextBlockObject(slackgo.PlainTextType, "Cancel", false, false),
		)
		cancelBtn.Style = slackgo.StyleDanger
		btns = append(btns, cancelBtn)
		blocks = append(blocks, slackgo.NewActionBlock("opts-"+d.ID, btns...))
	}

	hintParts := []string{}
	if len(d.Options) > 0 {
		hintParts = append(hintParts, "Click a button, reply with a number, or type free-form in the thread.")
	} else {
		hintParts = append(hintParts, "Reply in thread with your instructions, or type `CANCEL`.")
	}
	if d.TimeoutSeconds > 0 {
		if d.DefaultOption > 0 {
			hintParts = append(hintParts,
				fmt.Sprintf("Timeout %ds → defaults to option %d.", d.TimeoutSeconds, d.DefaultOption))
		} else {
			hintParts = append(hintParts, fmt.Sprintf("Timeout %ds.", d.TimeoutSeconds))
		}
	}
	blocks = append(blocks, slackgo.NewContextBlock("footer",
		slackgo.NewTextBlockObject(slackgo.MarkdownType, strings.Join(hintParts, " "), false, false),
	))

	return blocks
}

func toContextElements(items []*slackgo.TextBlockObject) []slackgo.MixedElement {
	out := make([]slackgo.MixedElement, len(items))
	for i, it := range items {
		out[i] = it
	}
	return out
}

// action_id encoding keeps the decision id parseable without extra state.
func actionIDOption(decisionID string, n int) string {
	return fmt.Sprintf("orca-decision:%s:opt:%d", decisionID, n)
}
func actionIDCancel(decisionID string) string {
	return "orca-decision:" + decisionID + ":cancel"
}

// renderDecision produces the plain-text fallback body.
func renderDecision(d *orca.Decision) string {
	var b bytes.Buffer
	sev := strings.ToUpper(string(d.Severity))
	fmt.Fprintf(&b, "[ORCA-DECISION · %s]  severity=%s  agent=%s", d.ID, sev, d.AgentID)
	if d.TaskID != "" {
		fmt.Fprintf(&b, "  task=%s", d.TaskID)
	}
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "Q: %s\n", truncate(d.Question, 400))

	if len(d.Context) > 0 {
		b.WriteString("\nContext:\n")
		for i, c := range d.Context {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "• %s\n", truncate(c, 200))
		}
	}

	if len(d.Options) > 0 {
		b.WriteString("\nOptions:\n")
		for i, opt := range d.Options {
			fmt.Fprintf(&b, "%d. %s\n", i+1, truncate(opt, 200))
		}
	}

	b.WriteString("\n")
	if len(d.Options) > 0 {
		nums := make([]string, len(d.Options))
		for i := range d.Options {
			nums[i] = "`" + strconv.Itoa(i+1) + "`"
		}
		fmt.Fprintf(&b, "Reply in thread: %s, or just say what to do.\n", strings.Join(nums, ", "))
	} else {
		b.WriteString("Reply in thread with your instructions.\n")
	}

	if d.TimeoutSeconds > 0 {
		if d.DefaultOption > 0 {
			fmt.Fprintf(&b, "Timeout: %ds (defaults to option %d).", d.TimeoutSeconds, d.DefaultOption)
		} else {
			fmt.Fprintf(&b, "Timeout: %ds.", d.TimeoutSeconds)
		}
	}
	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// ackLine is the in-thread confirmation after a FINALIZED human answer.
func ackLine(decisionID string, ans orca.DecisionAnswer) string {
	prefix := "✅ [" + decisionID + "] "
	switch ans.Type {
	case orca.AnswerOption:
		if ans.Note != "" {
			return prefix + fmt.Sprintf("decision closed — option %d (`%s`). Agent proceeding.", ans.Option, truncate(ans.Note, 120))
		}
		return prefix + fmt.Sprintf("decision closed — option %d. Agent proceeding.", ans.Option)
	case orca.AnswerCancel:
		return prefix + "decision cancelled."
	case orca.AnswerTimeout:
		return prefix + "decision timed out."
	default:
		return prefix + "decision closed."
	}
}

// ackLineClarify is shown when a free-form reply was forwarded but not
// finalizing — decision stays open.
func ackLineClarify(decisionID string, ans orca.DecisionAnswer) string {
	preview := truncate(ans.Text, 120)
	return fmt.Sprintf("↩ [%s] forwarded to agent — waiting for reply. Click a button to finalize, or keep the conversation going.\n> %s", decisionID, preview)
}

// parseHumanReply turns a raw slack thread-reply into a DecisionAnswer.
func parseHumanReply(text string, options []string) orca.DecisionAnswer {
	trimmed := strings.TrimSpace(text)
	if strings.EqualFold(trimmed, "CANCEL") {
		return orca.DecisionAnswer{Type: orca.AnswerCancel}
	}

	if n := len(options); n > 0 {
		m := optionRE.FindStringSubmatch(trimmed)
		if m != nil {
			opt, err := strconv.Atoi(m[1])
			if err == nil && opt >= 1 && opt <= n {
				note := ""
				if len(m) > 2 {
					note = strings.TrimSpace(strings.TrimLeft(m[2], ":.-"))
				}
				return orca.DecisionAnswer{
					Type:   orca.AnswerOption,
					Option: opt,
					Note:   note,
				}
			}
		}
	}

	return orca.DecisionAnswer{
		Type: orca.AnswerFreeform,
		Text: trimmed,
	}
}

var optionRE = regexp.MustCompile(`^\s*(\d+)\s*([:.\-].*)?$`)

// postPlainMessage handles non-decision outbound messages. Threads into a
// known correlation or posts fresh to default_channel and stashes.
func (s *Session) postPlainMessage(ctx context.Context, m orca.Message) {
	channel := s.cfg.Routing.Outbound.DefaultChannel
	threadTS := ""
	haveThread := false
	if m.CorrelationID != "" {
		if ct, ok := s.corr.byDecision(m.CorrelationID); ok {
			channel = ct.Channel
			threadTS = ct.ThreadTS
			haveThread = true
		}
	}
	if channel == "" {
		channel = "#orca-activity"
	}

	text := formatPlainBody(m)
	opts := s.botIdentity([]slackgo.MsgOption{
		slackgo.MsgOptionText(text, false),
		slackgo.MsgOptionDisableLinkUnfurl(),
	})
	if threadTS != "" {
		opts = append(opts, slackgo.MsgOptionTS(threadTS))
	}
	postedChannelID, postedTS, err := s.slack.PostMessage(channel, opts...)
	if err != nil {
		s.log.Warn("plain post failed", "err", err, "from", m.From, "correlation_id", m.CorrelationID)
		return
	}

	if !haveThread && m.CorrelationID != "" {
		s.corr.set(m.CorrelationID, postedChannelID, postedTS, nil)
		s.log.Info("stashed new correlation thread",
			"correlation_id", m.CorrelationID,
			"channel_id", postedChannelID,
			"thread_ts", postedTS)
	}
}
