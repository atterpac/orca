// Package events provides the observability fan-out that runs alongside
// (but distinct from) the message bus.
//
// Where bus carries inter-agent traffic that must be routed and ACL'd,
// the event bus is one-way and broadcast: anything that happens in the
// system (agent spawned, message delivered, tool called, decision
// requested, budget warned) emits an Event, and every Subscriber whose
// Filter matches receives a copy.
//
// Subscribers include the registry's SSE handler, the daemon's optional
// --events-log JSONL writer, the discussions registry sweeper, and any
// external observer (TUI, metrics exporter) that connects to GET
// /events.
//
// Like bus.InProc, Bus uses drop-on-full backpressure so a slow
// subscriber cannot stall the rest of the system.
//
// Concurrency: Emit and Subscribe are safe to call from multiple
// goroutines.
package events
