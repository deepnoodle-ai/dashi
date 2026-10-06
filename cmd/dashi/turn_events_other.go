//go:build !(linux || darwin || freebsd || openbsd || netbsd || android || ios)

package main

// turnEventsOpenFlags is empty where the platform has no O_NOFOLLOW;
// openTurnEvents still rejects a non-regular file after the open.
const turnEventsOpenFlags = 0
