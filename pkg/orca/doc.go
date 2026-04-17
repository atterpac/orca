// Package orca holds orca's public types — the vocabulary every other
// package and binary depends on.
//
// Categories:
//
//   - Routing primitives: Message, MessageKind, DispatchMode.
//   - Lifecycle types: AgentSpec, AgentInfo, AgentStatus.
//   - First-class concepts: Task + OpenTaskRequest, Decision +
//     DecisionAnswer + AskHumanRequest, Discussion + DiscussionStatus,
//     Update (the structured progress payload behind report_* MCP
//     verbs).
//   - Runtime adapter contracts: Runtime, Session, RuntimeCaps.
//   - Token accounting: TokenUsage, Budget.
//   - Observability: Event, EventKind.
//   - Access control: ACL.
//   - Validation: ValidationError, ValidationContext, ValidateSpec,
//     FatalCount.
//
// Subpackage roletemplates contains framework-provided prompt blocks
// composed into agents at spawn time based on AgentSpec.RoleTemplate.
//
// This package has no internal dependencies — it can be imported by
// anything (cmd, internal, sidecars, third-party tooling) without
// pulling in transport or storage code.
package orca
