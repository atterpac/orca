package supervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/pkg/orca"
	"github.com/atterpac/orca/pkg/orca/roletemplates"
)

type record struct {
	info     orca.AgentInfo
	session  orca.Session
	cancel   context.CancelFunc
	budget   budgetState
}

// budgetState tracks which budget events have already fired for an agent so
// we only emit Warn/Exceeded once and only apply the breach policy once.
type budgetState struct {
	warned   bool
	exceeded bool
}

// BudgetWarnThreshold — fraction of budget at which we emit BudgetWarn.
const BudgetWarnThreshold = 0.8

// DiscussionTouch is the optional callback supervisor invokes whenever a
// message flows that may belong to a conversation. Wired by the daemon
// to the discussions registry. Nil = no discussion tracking.
type DiscussionTouch func(senderBridge, agentID, correlationID string)

type Supervisor struct {
	mu       sync.RWMutex
	agents   map[string]*record
	runtimes map[string]orca.Runtime
	bus      bus.Bus
	events   *events.Bus

	// tag index: tag -> set of agent ids. Maintained on spawn/kill.
	byTag map[string]map[string]struct{}
	// round-robin counter per sorted-tag-set key for mode=any.
	rr map[string]uint64
	// task registry: task_id -> task. Populated by OpenTask.
	tasks map[string]*orca.Task

	// onDiscussionTouch fires on every delivered message whose sender or
	// recipient is a bridge agent and whose correlation_id is non-empty.
	OnDiscussionTouch DiscussionTouch
	// parent -> children, and child -> depth, for dynamic-spawn bookkeeping.
	children map[string]map[string]struct{}
	depth    map[string]int

	// Last-seen inbound correlation_id per agent. Used for auto-correlation:
	// when an agent sends a message without an explicit correlation_id,
	// supervisor fills in this value so the reply threads into the same
	// conversation. Updated every time a correlated message is delivered.
	lastInboundCorr sync.Map // agent_id → correlation_id

	// Per-agent ring buffers of recent events for debugging via
	// GET /agents/:id/transcript and `task trace <task_id>`.
	transcripts sync.Map // agent_id → *transcript

	// Limits caps dynamic spawning. Zero = unlimited. See SpawnLimits.
	Limits SpawnLimits
}

// SpawnLimits caps dynamic-spawn growth so a runaway coordinator cannot
// create an unbounded tree.
type SpawnLimits struct {
	// MaxAgents caps total concurrent agents. Zero = unlimited.
	MaxAgents int
	// MaxDepth caps the parent chain depth (root = 0). Zero = unlimited.
	MaxDepth int
}

func New(b bus.Bus, ev *events.Bus) *Supervisor {
	s := &Supervisor{
		agents:   make(map[string]*record),
		runtimes: make(map[string]orca.Runtime),
		bus:      b,
		events:   ev,
		byTag:    map[string]map[string]struct{}{},
		rr:       map[string]uint64{},
		tasks:    map[string]*orca.Task{},
		children: map[string]map[string]struct{}{},
		depth:    map[string]int{},
	}
	s.startTranscriptCollector(ev)
	return s
}

func (s *Supervisor) RegisterRuntime(rt orca.Runtime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtimes[rt.Name()] = rt
}

func (s *Supervisor) Runtimes() []orca.RuntimeCaps {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]orca.RuntimeCaps, 0, len(s.runtimes))
	for _, r := range s.runtimes {
		out = append(out, r.Capabilities())
	}
	return out
}

// RuntimeNames returns the names of all registered runtimes.
func (s *Supervisor) RuntimeNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.runtimes))
	for name := range s.runtimes {
		out = append(out, name)
	}
	return out
}

func (s *Supervisor) Spawn(ctx context.Context, spec orca.AgentSpec) (orca.AgentInfo, error) {
	if spec.ID == "" {
		return orca.AgentInfo{}, errors.New("agent id required")
	}
	if spec.Runtime == "" {
		spec.Runtime = "claude-code-local"
	}
	// Empty role_template defaults to the no-op "persona" template. Any
	// explicit value must match a known template.
	if spec.RoleTemplate == "" {
		spec.RoleTemplate = "persona"
	}
	if _, err := roletemplates.Load(spec.RoleTemplate); err != nil {
		return orca.AgentInfo{}, err
	}
	s.mu.Lock()
	if _, exists := s.agents[spec.ID]; exists {
		s.mu.Unlock()
		return orca.AgentInfo{}, fmt.Errorf("agent %s already exists", spec.ID)
	}
	if s.Limits.MaxAgents > 0 && len(s.agents) >= s.Limits.MaxAgents {
		s.mu.Unlock()
		return orca.AgentInfo{}, fmt.Errorf("max_agents limit reached (%d)", s.Limits.MaxAgents)
	}
	rt, ok := s.runtimes[spec.Runtime]
	if !ok {
		s.mu.Unlock()
		return orca.AgentInfo{}, fmt.Errorf("unknown runtime %s", spec.Runtime)
	}

	// Parent checks: must exist, must have can_spawn=true, must not exceed depth cap.
	spawnDepth := 0
	if spec.ParentID != "" {
		parent, ok := s.agents[spec.ParentID]
		if !ok {
			s.mu.Unlock()
			return orca.AgentInfo{}, fmt.Errorf("parent %s not found", spec.ParentID)
		}
		if !parent.info.Spec.CanSpawn {
			s.mu.Unlock()
			return orca.AgentInfo{}, fmt.Errorf("parent %s lacks can_spawn permission", spec.ParentID)
		}
		spawnDepth = s.depth[spec.ParentID] + 1
		if s.Limits.MaxDepth > 0 && spawnDepth > s.Limits.MaxDepth {
			s.mu.Unlock()
			return orca.AgentInfo{}, fmt.Errorf("max_depth limit reached (%d)", s.Limits.MaxDepth)
		}
	}

	// If the spec declares a task_id, look up the task and inherit its
	// worktree as workdir when the spec didn't pin one explicitly.
	if spec.TaskID != "" {
		t, ok := s.tasks[spec.TaskID]
		if !ok {
			s.mu.Unlock()
			return orca.AgentInfo{}, fmt.Errorf("task %s not found", spec.TaskID)
		}
		if spec.Workdir == "" {
			if t.WorktreePath != "" {
				spec.Workdir = t.WorktreePath
			} else {
				spec.Workdir = t.RepoRoot
			}
		}
	}
	s.mu.Unlock()

	sessCtx, cancel := context.WithCancel(context.Background())
	var sess orca.Session
	if rt.Capabilities().MultiSession {
		// Multi-session runtime: defer process spawn until first message;
		// sessions are per-correlation and reaped when idle. The wrapper
		// itself implements orca.Session so the rest of supervisor remains
		// session-shape-agnostic.
		sess = newMultiSession(rt, spec)
	} else {
		s0, err := rt.Start(sessCtx, spec)
		if err != nil {
			cancel()
			return orca.AgentInfo{}, fmt.Errorf("start: %w", err)
		}
		sess = s0
	}

	rec := &record{
		info: orca.AgentInfo{
			Spec:      spec,
			Status:    orca.StatusPending,
			StartedAt: time.Now(),
		},
		session: sess,
		cancel:  cancel,
	}
	s.mu.Lock()
	s.agents[spec.ID] = rec
	s.indexTags(spec.ID, spec.Tags)
	if spec.TaskID != "" {
		if t, ok := s.tasks[spec.TaskID]; ok && !slices.Contains(t.Agents, spec.ID) {
			t.Agents = append(t.Agents, spec.ID)
		}
	}
	s.depth[spec.ID] = spawnDepth
	if spec.ParentID != "" {
		set, ok := s.children[spec.ParentID]
		if !ok {
			set = map[string]struct{}{}
			s.children[spec.ParentID] = set
		}
		set[spec.ID] = struct{}{}
	}
	// Snapshot under lock so concurrent applyEvent writes don't race the
	// return value. Goroutines start only after the snapshot is taken.
	snapshot := rec.info
	s.mu.Unlock()

	go s.pump(rec)
	go s.deliverInbox(rec)
	return snapshot, nil
}

// indexTags adds the agent to the tag index. Must be called with s.mu held.
func (s *Supervisor) indexTags(id string, tags []string) {
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		set, ok := s.byTag[t]
		if !ok {
			set = map[string]struct{}{}
			s.byTag[t] = set
		}
		set[id] = struct{}{}
	}
}

// deindexTags removes the agent from the tag index. Must be called with s.mu held.
func (s *Supervisor) deindexTags(id string, tags []string) {
	for _, t := range tags {
		set := s.byTag[t]
		if set == nil {
			continue
		}
		delete(set, id)
		if len(set) == 0 {
			delete(s.byTag, t)
		}
	}
}

// FindByTags returns agent ids whose tags contain every tag in the input
// (AND semantics). Empty input returns all agent ids. status filter: only
// agents in StatusReady/StatusBusy are eligible for dispatch; pass
// eligibleOnly=false to include any status.
func (s *Supervisor) FindByTags(tags []string, eligibleOnly bool) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findByTagsLocked(tags, eligibleOnly)
}

func (s *Supervisor) findByTagsLocked(tags []string, eligibleOnly bool) []string {
	if len(tags) == 0 {
		out := make([]string, 0, len(s.agents))
		for id, rec := range s.agents {
			if !eligibleOnly || isEligible(rec.info.Status) {
				out = append(out, id)
			}
		}
		slices.Sort(out)
		return out
	}
	first := s.byTag[strings.TrimSpace(tags[0])]
	if first == nil {
		return nil
	}
	out := make([]string, 0, len(first))
	for id := range first {
		rec, ok := s.agents[id]
		if !ok {
			continue
		}
		specTags := rec.info.Spec.Tags
		matched := true
		for _, needed := range tags[1:] {
			needed = strings.TrimSpace(needed)
			if needed == "" {
				continue
			}
			if !slices.Contains(specTags, needed) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		if eligibleOnly && !isEligible(rec.info.Status) {
			continue
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func isEligible(st orca.AgentStatus) bool {
	switch st {
	case orca.StatusReady, orca.StatusBusy, orca.StatusPending:
		return true
	default:
		return false
	}
}

// DispatchTagged resolves a tag-targeted message to one or more concrete
// recipients, publishing a copy to each. Returns the recipient ids chosen.
// - mode=ModeAny: round-robin picks exactly one matching agent per call,
//   keyed by the sorted tag set so different tag queries rotate independently.
// - mode=ModeAll: delivers to every matching agent.
//
// Both sender-side and receiver-side ACLs are enforced — candidates that
// either party refuses are filtered out before dispatch. A MessageDropped
// event is emitted for each candidate dropped by ACL.
func (s *Supervisor) DispatchTagged(ctx context.Context, m orca.Message) ([]string, error) {
	if len(m.Tags) == 0 {
		return nil, errors.New("dispatch: no tags")
	}
	m = s.applyAutoCorrelation(m)
	mode := m.Mode
	if mode == "" {
		mode = orca.ModeAny
	}

	s.mu.Lock()
	candidates := s.findByTagsLocked(m.Tags, true)
	// remove self from candidates so an agent doesn't route a message to itself
	if m.From != "" {
		candidates = slices.DeleteFunc(candidates, func(id string) bool { return id == m.From })
	}

	// Apply ACL filter: drop candidates sender can't reach or that refuse
	// this sender. Collect blocked ones so we can emit events after unlock.
	senderTags := s.senderTagsLocked(m.From)
	var blocked []aclBlock
	allowed := candidates[:0]
	for _, id := range candidates {
		rec := s.agents[id]
		var reason string
		if !s.canSendLocked(m.From, id, rec.info.Spec.Tags) {
			reason = "sends_to"
		} else if !s.canReceiveLocked(m.From, senderTags, id) {
			reason = "accepts_from"
		}
		if reason != "" {
			blocked = append(blocked, aclBlock{id: id, reason: reason})
			continue
		}
		allowed = append(allowed, id)
	}
	candidates = allowed

	if len(candidates) == 0 {
		s.mu.Unlock()
		s.emitDroppedMany(m, blocked)
		return nil, fmt.Errorf("no agent matches tags %v (or all blocked by ACL)", m.Tags)
	}

	var targets []string
	switch mode {
	case orca.ModeAll:
		targets = candidates
	case orca.ModeAny:
		key := tagKey(m.Tags)
		idx := int(s.rr[key] % uint64(len(candidates)))
		s.rr[key]++
		targets = []string{candidates[idx]}
	default:
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown dispatch mode %q", mode)
	}
	s.mu.Unlock()

	// Emit MessageDropped for every ACL-blocked candidate.
	s.emitDroppedMany(m, blocked)

	// Clear the tag fields on the fan-out copies so the bus routes directly.
	for _, id := range targets {
		cp := m
		cp.To = id
		cp.Tags = nil
		cp.Mode = ""
		if err := s.bus.Publish(ctx, cp); err != nil {
			return targets, err
		}
	}
	return targets, nil
}

type aclBlock struct {
	id     string
	reason string
}

func (s *Supervisor) emitDroppedMany(m orca.Message, blocked []aclBlock) {
	for _, b := range blocked {
		s.events.Emit(orca.Event{
			Kind:    orca.EvtMessageDropped,
			AgentID: m.From,
			Payload: map[string]any{
				"from":   m.From,
				"to":     b.id,
				"reason": "acl:" + b.reason,
				"kind":   m.Kind,
				"tags":   m.Tags,
			},
		})
	}
}

// DispatchDirect publishes a direct (to=<id>) message after ACL checks.
// Returns error if blocked. Used by Registry's direct send endpoint so ACL is
// enforced there too.
func (s *Supervisor) DispatchDirect(ctx context.Context, m orca.Message) error {
	if m.To == "" {
		return errors.New("dispatch: no target id")
	}
	m = s.applyAutoCorrelation(m)

	s.mu.RLock()
	targetTags := s.tagsForLocked(m.To)
	senderTags := s.senderTagsLocked(m.From)
	sendOK := s.canSendLocked(m.From, m.To, targetTags)
	recvOK := s.canReceiveLocked(m.From, senderTags, m.To)
	s.mu.RUnlock()

	if !sendOK || !recvOK {
		reason := "acl:sends_to"
		if sendOK && !recvOK {
			reason = "acl:accepts_from"
		}
		s.events.Emit(orca.Event{
			Kind:    orca.EvtMessageDropped,
			AgentID: m.From,
			Payload: map[string]any{
				"from":   m.From,
				"to":     m.To,
				"reason": reason,
				"kind":   m.Kind,
			},
		})
		return fmt.Errorf("blocked by %s", reason)
	}
	return s.bus.Publish(ctx, m)
}

func (s *Supervisor) tagsForLocked(id string) []string {
	if rec, ok := s.agents[id]; ok {
		return rec.info.Spec.Tags
	}
	return nil
}

func tagKey(tags []string) string {
	cp := append([]string(nil), tags...)
	for i, t := range cp {
		cp[i] = strings.TrimSpace(t)
	}
	slices.Sort(cp)
	return strings.Join(cp, ",")
}

func (s *Supervisor) pump(rec *record) {
	ch, err := rec.session.Events(context.Background())
	if err != nil {
		return
	}
	for e := range ch {
		s.events.Emit(e)
		s.applyEvent(rec, e)
	}
	now := time.Now()
	s.mu.Lock()
	rec.info.Status = orca.StatusExited
	rec.info.ExitedAt = &now
	s.mu.Unlock()
}

func (s *Supervisor) applyEvent(rec *record, e orca.Event) {
	var actions []func()

	s.mu.Lock()
	switch e.Kind {
	case orca.EvtAgentReady:
		rec.info.Status = orca.StatusReady
		if p, ok := e.Payload.(map[string]any); ok {
			if sid, _ := p["session_id"].(string); sid != "" {
				rec.info.SessionID = sid
			}
		}
	case orca.EvtTurnCompleted, orca.EvtUsageSnapshot:
		rec.info.Usage = rec.session.Usage()
		actions = s.evaluateBudgetLocked(rec)
	case orca.EvtAgentExited:
		rec.info.Status = orca.StatusExited
	}
	s.mu.Unlock()

	// Run side effects (emitting events, interrupting sessions) outside the
	// lock so downstream subscribers can't re-enter and deadlock us.
	for _, a := range actions {
		a()
	}
}

// evaluateBudgetLocked inspects the agent's current cumulative usage against
// its Budget spec and returns a list of deferred side-effect closures:
// emitting BudgetWarn / BudgetExceeded events and applying the breach policy.
// Caller must hold s.mu.
func (s *Supervisor) evaluateBudgetLocked(rec *record) []func() {
	b := rec.info.Spec.Budget
	if b == nil {
		return nil
	}
	u := rec.info.Usage
	pct := budgetPct(b, u)
	var out []func()

	if pct >= 1.0 && !rec.budget.exceeded {
		rec.budget.exceeded = true
		policy := b.OnBreach
		if policy == "" {
			policy = "warn"
		}
		id := rec.info.Spec.ID
		usageCopy := u
		budgetCopy := *b

		// Apply policy effects synchronously under lock where safe.
		switch policy {
		case "soft_stop":
			rec.info.BudgetPaused = true
		}

		// Defer event emission + session interrupt — those may call back.
		out = append(out, func() {
			s.events.Emit(orca.Event{
				Kind:    orca.EvtBudgetExceeded,
				AgentID: id,
				Payload: map[string]any{
					"pct":    pct,
					"usage":  usageCopy,
					"budget": budgetCopy,
					"policy": policy,
				},
			})
		})

		if policy == "hard_interrupt" {
			sess := rec.session
			out = append(out, func() {
				_ = sess.Interrupt(context.Background())
				_ = s.Kill(id) // removes from registry + closes session
			})
		}
	} else if pct >= BudgetWarnThreshold && !rec.budget.warned {
		rec.budget.warned = true
		id := rec.info.Spec.ID
		usageCopy := u
		budgetCopy := *b
		out = append(out, func() {
			s.events.Emit(orca.Event{
				Kind:    orca.EvtBudgetWarn,
				AgentID: id,
				Payload: map[string]any{
					"pct":    pct,
					"usage":  usageCopy,
					"budget": budgetCopy,
				},
			})
		})
	}
	return out
}

// budgetPct returns the greatest fraction-of-budget consumed across any
// dimension set on b. A value ≥1.0 means budget is exhausted on at least one
// dimension; ≥0.8 means we're in the warning zone.
func budgetPct(b *orca.Budget, u orca.TokenUsage) float64 {
	var maxPct float64
	if b.MaxInputTokens > 0 {
		if p := float64(u.InputTokens) / float64(b.MaxInputTokens); p > maxPct {
			maxPct = p
		}
	}
	if b.MaxOutputTokens > 0 {
		if p := float64(u.OutputTokens) / float64(b.MaxOutputTokens); p > maxPct {
			maxPct = p
		}
	}
	if b.MaxCostUSD > 0 {
		if p := u.CostUSD / b.MaxCostUSD; p > maxPct {
			maxPct = p
		}
	}
	return maxPct
}

func (s *Supervisor) deliverInbox(rec *record) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = rec.session.Wait()
		cancel()
	}()
	ch, _ := s.bus.Subscribe(ctx, bus.Filter{AgentID: rec.info.Spec.ID})
	for m := range ch {
		// Respect soft-stop: pause delivery but don't drain the subscription.
		s.mu.RLock()
		paused := rec.info.BudgetPaused
		senderTags := s.senderTagsLocked(m.From)
		acceptsFromOK := s.canReceiveLocked(m.From, senderTags, rec.info.Spec.ID)
		s.mu.RUnlock()
		if paused {
			s.events.Emit(orca.Event{Kind: orca.EvtError, AgentID: rec.info.Spec.ID, Payload: map[string]any{
				"scope":  "budget",
				"msg":    "inbound message dropped — agent paused by budget policy (soft_stop)",
				"from":   m.From,
				"msg_id": m.ID,
			}})
			continue
		}
		if !acceptsFromOK {
			s.events.Emit(orca.Event{Kind: orca.EvtMessageDropped, AgentID: rec.info.Spec.ID, Payload: map[string]any{
				"from":   m.From,
				"to":     rec.info.Spec.ID,
				"reason": "acl:accepts_from",
				"kind":   m.Kind,
				"stage":  "delivery",
			}})
			continue
		}
		_ = rec.session.Send(ctx, m)
		// Record the correlation so the agent's next outbound auto-fills
		// with it unless overridden. Only non-empty correlations count —
		// we never "un-correlate" a session.
		if m.CorrelationID != "" {
			s.lastInboundCorr.Store(rec.info.Spec.ID, m.CorrelationID)
		}
		// Notify the discussion registry if either party is a bridge.
		if m.CorrelationID != "" && s.OnDiscussionTouch != nil {
			senderBridge := s.bridgeIDIfBridge(m.From)
			recvBridge := s.bridgeIDIfBridge(rec.info.Spec.ID)
			if senderBridge != "" {
				s.OnDiscussionTouch(senderBridge, rec.info.Spec.ID, m.CorrelationID)
			} else if recvBridge != "" {
				s.OnDiscussionTouch(recvBridge, m.From, m.CorrelationID)
			}
		}
		s.events.Emit(orca.Event{Kind: orca.EvtMessageDelivered, AgentID: rec.info.Spec.ID, Payload: map[string]any{
			"from":           m.From,
			"id":             m.ID,
			"correlation_id": m.CorrelationID,
		}})
	}
}

// bridgeIDIfBridge returns the agent's id when it's spawned under a
// runtime named "bridge", else empty. Used to detect bridge-originated
// or bridge-bound traffic without coupling supervisor to bridge types.
func (s *Supervisor) bridgeIDIfBridge(id string) string {
	if id == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.agents[id]
	if !ok {
		return ""
	}
	if r.info.Spec.Runtime == "bridge" {
		return id
	}
	return ""
}

// AutoCorrelationFor returns the last-seen inbound correlation_id for the
// given agent. Empty string if none recorded yet. Used by the outbound
// routing paths to fill in an omitted correlation_id.
func (s *Supervisor) AutoCorrelationFor(agentID string) string {
	if v, ok := s.lastInboundCorr.Load(agentID); ok {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// applyAutoCorrelation fills m.CorrelationID from the agent's last-seen
// inbound when the outgoing message doesn't carry one explicitly.
// Returns the (possibly modified) message.
func (s *Supervisor) applyAutoCorrelation(m orca.Message) orca.Message {
	if m.CorrelationID != "" || m.From == "" {
		return m
	}
	if auto := s.AutoCorrelationFor(m.From); auto != "" {
		m.CorrelationID = auto
	}
	return m
}

func (s *Supervisor) List() []orca.AgentInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]orca.AgentInfo, 0, len(s.agents))
	for _, r := range s.agents {
		info := r.info
		info.Usage = r.session.Usage()
		out = append(out, info)
	}
	return out
}

func (s *Supervisor) Get(id string) (orca.AgentInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.agents[id]
	if !ok {
		return orca.AgentInfo{}, false
	}
	info := r.info
	info.Usage = r.session.Usage()
	return info, true
}

func (s *Supervisor) Kill(id string) error {
	s.mu.Lock()
	r, ok := s.agents[id]
	var cascade []string
	if ok {
		delete(s.agents, id)
		s.deindexTags(id, r.info.Spec.Tags)

		// Collect children that opted into cascade kill before we drop the
		// parent → children edge, so we can terminate them after unlocking.
		for childID := range s.children[id] {
			child, childOK := s.agents[childID]
			if !childOK {
				continue
			}
			if child.info.Spec.OnParentExit == "kill" {
				cascade = append(cascade, childID)
			}
		}
		delete(s.children, id)
		delete(s.depth, id)
		// Detach this id from any parent's children set.
		if r.info.Spec.ParentID != "" {
			if pset, ok := s.children[r.info.Spec.ParentID]; ok {
				delete(pset, id)
				if len(pset) == 0 {
					delete(s.children, r.info.Spec.ParentID)
				}
			}
		}
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("agent %s not found", id)
	}
	r.cancel()
	err := r.session.Close()

	// Cascade kills run outside the lock. Each Kill() re-enters and acquires
	// the lock normally, and may emit additional cascades in turn.
	for _, childID := range cascade {
		_ = s.Kill(childID)
	}
	return err
}

// Children returns the direct children of parentID.
func (s *Supervisor) Children(parentID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := s.children[parentID]
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Depth returns the spawn depth of agentID (root = 0).
func (s *Supervisor) Depth(agentID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.depth[agentID]
}

func (s *Supervisor) AggregateUsage() orca.TokenUsage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total orca.TokenUsage
	for _, r := range s.agents {
		total = total.Add(r.session.Usage())
	}
	return total
}

func (s *Supervisor) SendTo(ctx context.Context, id string, m orca.Message) error {
	s.mu.RLock()
	r, ok := s.agents[id]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("agent %s not found", id)
	}
	return r.session.Send(ctx, m)
}

func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	rs := make([]*record, 0, len(s.agents))
	for _, r := range s.agents {
		rs = append(rs, r)
	}
	s.mu.Unlock()
	for _, r := range rs {
		r.cancel()
		_ = r.session.Close()
	}
}
