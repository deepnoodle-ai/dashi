package toolcheck

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/decide"
)

// Defaults for a CommandRisk's zero fields. 0.8 is the flag of decide's
// command-risk template: on decide's set of 151 commands, severe scored 12
// of 13 disasters at 0.85 or more and every other command at 0.63 or less.
const (
	defaultCommandMin     = 0.8
	defaultCommandTimeout = 10 * time.Second
)

// CommandRisk asks a decision model whether a shell command could cause
// severe harm that is hard to undo, beyond the local checkout: deleting a
// home folder, force-pushing a shared branch, dropping a database,
// destroying cloud resources, or sending secrets away. Routine work that
// discards local changes, such as git checkout -- ., is not severe.
//
// The zero value works. One CommandRisk serves many turns at once.
//
// Its hook checks every shell call, even one an earlier hook holds, since a
// hook does not see earlier answers. Add it where nothing else asks about
// each command, such as an agent that runs commands without approval.
type CommandRisk struct {
	// Client asks the decision model. Nil means defaultclient.Load, which reads
	// the environment, such as TYPESAFE_API_KEY.
	Client *decide.Client
	// ShellTools names the tools whose "command" argument is a shell
	// command. Nil means {"bash"}, the workspace bash tool.
	ShellTools []string
	// MinProbability is the P(yes) of "severe" that flags a command. Zero
	// means 0.8. A negative value never flags, but still checks, so a host
	// can watch OnCheck first.
	MinProbability float64
	// Timeout bounds each check. Zero means 10 seconds. A timeout never
	// pauses the check; the hook holds the call for approval.
	Timeout time.Duration
	// OnCheck, if set, sees each check's verdict and error, from
	// Check and from the hook. It is not called for a call the check does
	// not read. It may be called from many turns at once.
	OnCheck func(Verdict, error)
}

// Check asks whether the command of a shell call could cause severe harm.
// For a call the check does not read, such as read_file, it makes no
// request and returns a Verdict with Checked false and no error. An error
// means the check was skipped; IsOutage tells an outage from a failure the
// input could cause. Only a setup failure, such as no key or a failed
// login, pauses the check for a minute; Check then returns a *PausedError,
// as it does during the pause. A Decide server or connection error never
// pauses it, so a command cannot turn off the check for other commands.
func (c CommandRisk) Check(ctx context.Context, p dashi.ToolPlan) (Verdict, error) {
	v := Verdict{Call: p.Call.Clone()}
	command, ok := shellCommand(p.Call, c.ShellTools)
	if !ok {
		return v, nil
	}
	a, err := ask(ctx, c.Client, "command-risk",
		cmp.Or(c.Timeout, defaultCommandTimeout),
		"command", command, commandQuestions, true, notSetUp)
	v.Requests, v.Usage = a.requests, a.usage
	if err != nil {
		c.report(v, err)
		return v, err
	}
	v.Checked = true
	v.Probabilities = a.probabilities
	v.Model, v.RequestID, v.Usage, v.Partial = a.model, a.requestID, a.usage, a.partial
	v.Flagged = flagged(a, commandQuestions, cmp.Or(c.MinProbability, defaultCommandMin))
	v.Reason = reason(a, commandQuestions)
	c.report(v, nil)
	return v, nil
}

func (c CommandRisk) report(v Verdict, err error) {
	if c.OnCheck != nil {
		c.OnCheck(v, err)
	}
}

// Hooks returns a BeforeTool hook named CommandRiskName. A flagged command
// is held for approval with the verdict's Reason; a passed or unread call
// gets no opinion. Any other failure, such as a server error, a timeout,
// or a command too long to check, also holds the call, with a Reason that
// says why it was not checked. Only an outage (see IsOutage) fails open:
// the call runs and Dashi records a hook_failed warning. It never allows a
// call.
func (c CommandRisk) Hooks() dashi.Hooks {
	return dashi.ToolHook{
		Name:     CommandRiskName,
		FailOpen: true,
		Func: func(ctx context.Context, p dashi.ToolPlan) (dashi.ToolDecision, error) {
			v, err := c.Check(ctx, p)
			switch {
			case err != nil && (IsOutage(err) || ctx.Err() != nil):
				return dashi.ToolDecision{}, err
			case err != nil:
				return dashi.NeedsApproval(uncheckedReason(err)), nil
			case !v.Flagged:
				return dashi.ToolDecision{}, nil
			}
			d := dashi.NeedsApproval(v.Reason)
			d.RulesVersion = v.Model
			return d, nil
		},
	}.Hooks()
}

// uncheckedReason is the Reason for a command held because its check
// failed.
func uncheckedReason(err error) string {
	why := "the check failed"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		why = "the check timed out"
	case errors.Is(err, ErrTooLarge):
		why = "the command is too long to check"
	case errors.Is(err, decide.ErrRateLimited):
		why = "Decide's rate limit was reached"
	case errors.Is(err, decide.ErrInvalidAnswer):
		why = "Decide gave no valid answer"
	case errors.Is(err, decide.ErrServer):
		why = "Decide returned a server error"
	case CauseOf(err) == CauseNetworkError:
		why = "Decide's connection failed"
	case errors.Is(err, decide.ErrValidation), errors.Is(err, decide.ErrInvalidRequest):
		why = "Decide rejected the request"
	default:
		if _, ok := errors.AsType[*decide.APIError](err); ok {
			why = "Decide rejected the request"
		}
	}
	return "toolcheck could not check this command: " + why
}
