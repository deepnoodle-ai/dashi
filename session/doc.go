// Package session runs a conversation of many turns on a dashi.Runner.
//
// A Session numbers the turns of one conversation, gives each new turn the
// history built from the turns before it, carries the agent forward,
// queues and steers user input, compacts a long history with a Compactor,
// and applies the rules for a new message while the last turn is still
// open. It keeps no record of its own: turn n of session S is the turn
// TurnID(S, n), and the turn records in the Runner's Recorder are the only
// truth.
//
// Session uses only the public contract of package dashi. A host that
// stores its own turns can drive dashi.Runner directly instead, with
// dashi.BuildHistory as the history rule.
package session
