// Package session stores Dashi CLI sessions on disk.
//
// A session is an ordered list of turn IDs plus a title, model, and the
// directory it belongs to. The turn records live in a filerecorder beside the
// session file, so this package never keeps a second copy of the
// conversation: History builds model messages from the records each time.
package session
