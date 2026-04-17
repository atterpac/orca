// Package supervisor is orca's agent lifecycle and routing brain. It is
// the largest internal package and owns the most state.
//
// # Responsibilities
//
//   - Agent lifecycle: spawn, kill, restart-on-failure (planned),
//     parent/child tracking, cascade-kill via OnParentExit, dynamic
//     spawn limits (MaxAgents, MaxDepth).
//   - Routing: direct delivery (DispatchDirect), tag-based dispatch
//     (DispatchTagged with mode=any|all and round-robin tracking),
//     ACL enforcement (canSend / canReceive), self-exclusion on
//     tag dispatches.
//   - Auto-correlation: per-agent lastInboundCorr stored when delivery
//     happens; consulted on outbound to fill an empty CorrelationID.
//   - Tasks: open/close/list, git worktree allocation, attaching
//     spawning agents to a task by id, auto-announcing TaskOpened to
//     the bridge agent via KindEvent.
//   - Budget enforcement: evaluated after every TurnCompleted /
//     UsageSnapshot event. Warn at 80%, exceeded at 100%, breach
//     policy ("warn", "soft_stop", "hard_interrupt") applied
//     synchronously.
//   - Inbox pumping: per-agent goroutine subscribes to the bus,
//     applies ACL + budget pause + auto-correlation update, and
//     forwards into the agent's session.
//   - Optional callbacks: OnDiscussionTouch fires for any correlated
//     message involving a bridge agent — daemon wires it to the
//     discussions registry.
//
// # Concurrency
//
// Most state lives behind Supervisor.mu (sync.RWMutex). Helper methods
// suffixed Locked require the caller to hold the lock; bare names take
// it themselves. lastInboundCorr is a sync.Map so the auto-correlation
// fast path doesn't contend with mu.
//
// Public methods are safe to call from multiple goroutines. Internal
// callbacks (the deliverInbox goroutine, budget side-effects) are
// designed to run side-effecting work (event emission, session
// interrupts) outside Supervisor.mu to avoid deadlock with subscribers
// that re-enter the supervisor.
//
// # Sequencing
//
// On Spawn: validate spec → register runtime → start session →
// allocate record → index tags → attach to task → start pump and
// inbox goroutines.
//
// On Kill: remove from agents map → deindex tags → schedule cascade
// for any OnParentExit=kill children → cancel session context →
// session.Close().
package supervisor
