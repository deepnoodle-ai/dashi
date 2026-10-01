//go:build darwin || linux

package main

import (
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const automationProcessPoll = 20 * time.Millisecond

// Serialize temporary flag changes across adapters. Two different descriptor
// numbers can share one open description, including redirected stdin/stdout.
var automationProcessFlagsMu sync.Mutex

func automationProcessTerminal(file *os.File) (bool, error) {
	conn, err := file.SyscallConn()
	if err != nil {
		return false, errors.New("cannot inspect process descriptor")
	}
	var terminal bool
	if err := conn.Control(func(fd uintptr) { terminal = term.IsTerminal(int(fd)) }); err != nil {
		return false, errors.New("cannot inspect process descriptor")
	}
	return terminal, nil
}

// dup shares open-description flags. Change O_NONBLOCK only for one syscall,
// then restore the exact flags before returning or waiting. Poll has a short
// upper bound, so Close and deadline changes require no blocked goroutine.
// Neither File.Fd nor Go's runtime FIFO poller enters this path.
type automationProcessPipe struct {
	mu       sync.Mutex
	fd       int
	write    bool
	closed   bool
	closeErr error
	deadline time.Time
}

func openAutomationProcessPipe(file *os.File, write bool) (automationProcessPipeIO, error) {
	conn, err := file.SyscallConn()
	if err != nil {
		return nil, errors.New("cannot inspect process pipe")
	}
	fd := -1
	var setupErr error
	err = conn.Control(func(source uintptr) {
		automationProcessFlagsMu.Lock()
		defer automationProcessFlagsMu.Unlock()
		flags, e := unix.FcntlInt(source, unix.F_GETFL, 0)
		if e != nil {
			setupErr = e
			return
		}
		access := flags & unix.O_ACCMODE
		if write && access == unix.O_RDONLY || !write && access == unix.O_WRONLY {
			setupErr = errors.New("pipe direction is invalid")
			return
		}
		fd, setupErr = unix.FcntlInt(source, unix.F_DUPFD_CLOEXEC, 3)
		if setupErr != nil {
			return
		}
		// Qualification exercises setting and restoring the shared flags
		// before the controller can admit work.
		if _, setupErr = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags|unix.O_NONBLOCK); setupErr != nil {
			return
		}
		_, setupErr = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
		if setupErr == nil && write {
			ready := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
			_, setupErr = unix.Poll(ready, 0)
			if setupErr == nil && ready[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				setupErr = errors.New("stdout pipe has no reader")
			}
		}
	})
	if err != nil || setupErr != nil {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		return nil, errors.New("cannot qualify process pipe")
	}
	return &automationProcessPipe{fd: fd, write: write}, nil
}

func (p *automationProcessPipe) Read(data []byte) (int, error) {
	if p.write {
		return 0, errors.New("output pipe cannot read")
	}
	return p.transfer(data)
}

func (p *automationProcessPipe) Write(data []byte) (int, error) {
	if !p.write {
		return 0, errors.New("input pipe cannot write")
	}
	return p.transfer(data)
}

func (p *automationProcessPipe) transfer(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return 0, os.ErrClosed
		}
		if !p.deadline.IsZero() && !time.Now().Before(p.deadline) {
			p.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		n, err := p.attemptLocked(data)
		fd := p.fd
		wait := automationProcessPoll
		if !p.deadline.IsZero() {
			wait = min(wait, time.Until(p.deadline))
		}
		p.mu.Unlock()
		if err == nil {
			if n == 0 && !p.write {
				return 0, io.EOF
			}
			return n, nil
		}
		if n > 0 {
			return n, err
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return 0, err
		}
		if wait <= 0 {
			continue
		}
		events := int16(unix.POLLIN)
		if p.write {
			events = unix.POLLOUT
		}
		// Retry the syscall even after a poll timeout. This observes EOF on
		// Darwin FIFOs whose last-writer close can fail to notify kqueue.
		_, err = unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: events}}, max(1, int(wait.Milliseconds())))
		if err != nil && !errors.Is(err, unix.EINTR) && !errors.Is(err, unix.EBADF) {
			return 0, err
		}
	}
}

func (p *automationProcessPipe) attemptLocked(data []byte) (int, error) {
	automationProcessFlagsMu.Lock()
	defer automationProcessFlagsMu.Unlock()
	flags, err := unix.FcntlInt(uintptr(p.fd), unix.F_GETFL, 0)
	if err != nil {
		return 0, err
	}
	changed := flags&unix.O_NONBLOCK == 0
	if changed {
		if _, err := unix.FcntlInt(uintptr(p.fd), unix.F_SETFL, flags|unix.O_NONBLOCK); err != nil {
			return 0, err
		}
	}
	var n int
	if p.write {
		n, err = unix.Write(p.fd, data)
	} else {
		n, err = unix.Read(p.fd, data)
	}
	if changed {
		if _, restoreErr := unix.FcntlInt(uintptr(p.fd), unix.F_SETFL, flags); restoreErr != nil {
			return max(n, 0), errors.New("cannot restore process pipe flags")
		}
	}
	return max(n, 0), err
}

func (p *automationProcessPipe) SetWriteDeadline(deadline time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return os.ErrClosed
	}
	p.deadline = deadline
	return nil
}

func (p *automationProcessPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	p.closeErr = unix.Close(p.fd)
	return p.closeErr
}
