// Package bus implements orca's in-process message routing layer.
//
// The Bus interface decouples senders from receivers: publishers call
// Publish, and any subscribers whose Filter matches the message receive
// a copy on their dedicated channel. Subscribers register a Filter
// (AgentID, Topic, Kinds) and get back a receive-only channel.
//
// The shipped implementation, InProc, holds the subscriber map under a
// single RWMutex. Publish fans out to matching subscribers without
// blocking the publisher: if a subscriber's channel is full, the
// message is dropped for that subscriber (drop-on-full backpressure).
// Slow consumers therefore can't stall fast publishers, which is the
// right policy for a bus that needs to keep agent throughput moving.
//
// Request is a correlated request/response helper: it generates a
// CorrelationID, publishes the message, and blocks until a matching
// Response arrives or the context expires. Used internally for daemon
// → daemon calls; agents use the higher-level send_message MCP tool.
//
// TTL on a Message decrements on every Publish. Reaching zero produces
// ErrTTLExpired so message loops can't grow unboundedly.
//
// Concurrency: every public method on InProc is safe to call from
// multiple goroutines.
package bus
