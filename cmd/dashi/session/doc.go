// Package session stores the metadata of Dashi CLI sessions on disk: the
// title, model, and directory of each, the diffs shown after shell calls,
// and the lock that keeps one process per session.
//
// The conversation itself is a dashi.Session over the turn records, which
// live in a filerecorder beside the session file. This package never keeps
// a second copy of it, not even the list of turns.
package session
