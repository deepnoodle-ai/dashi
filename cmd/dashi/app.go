package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/decide/defaultclient"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/decide"
)

// appConfig holds what buildApp needs.
type appConfig struct {
	Dir           string
	HomeDir       string
	Model         string
	Client        llm.Client
	Store         *sessionstore.Store
	Preferences   *cliPreferences
	InvocationDir string
	MCPFiles      []string
	// ModelCalls overrides the per-turn model-call limit; zero means 40.
	ModelCalls int
	// ApproveAll approves every tool call without asking.
	ApproveAll bool
	// CompactionThreshold is the token count at which history is compacted;
	// zero means the compaction default.
	CompactionThreshold int
	// Decide is which decide features are on, and DecideSetup the default
	// decide client they use.
	Decide        decideSettings
	DecideSetup   decideSetup
	parentStopped func(int)
	// TurnEvents is the --turn-events file; nil without the flag.
	TurnEvents *turnEventLog

	// decideClient replaces the default decide client; tests use it.
	decideClient *decide.Client
	// decideWait replaces the decide check's time limit; tests use it.
	decideWait time.Duration

	// recorder wraps each session's recorder; tests use it.
	recorder func(*filerecorder.Recorder) dashi.Recorder
}

// app is the engine and controller wired to one UI.
type app struct {
	eng    *Engine
	ctrl   *Controller
	wiring wiring
}

// buildApp wires the adapters, the engine, and the controller around ui.
func buildApp(ctx context.Context, cfg appConfig, ui UI) (*app, error) {
	canonical, err := canonicalWorkspace(cfg.Dir, "")
	if err != nil {
		return nil, err
	}
	cfg.Dir = canonical
	var ctrl *Controller
	home := cfg.HomeDir
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	resources := newResourceManager(cfg.Dir, home)
	invocation := cfg.InvocationDir
	if invocation == "" {
		invocation, _ = os.Getwd()
	}
	mcp := newMCPRuntime(cfg.Dir, selectMCP(home, cfg.Dir, invocation, cfg.MCPFiles, os.ReadFile, os.LookupEnv))
	setup := cfg.DecideSetup
	if cfg.decideClient != nil {
		setup = decideSetup{Setup: defaultclient.Setup{Provider: defaultclient.ProviderTypeSafe, Client: cfg.decideClient}}
	}
	rt := newDecideRuntime(cfg.Decide, setup)
	if rt != nil {
		rt.warn = func(text string) { ctrl.emit(warnBlock("%s", text)) }
		rt.outage = func(feature string) { ctrl.warnPause(feature) }
		rt.watchFailed = func(cause string) { ctrl.warnWatchFailed(cause) }
		rt.showRow = func(b Block) { emitToUI(ui, b) }
		if cfg.decideWait > 0 {
			rt.wait, rt.risk.Timeout = cfg.decideWait, cfg.decideWait
		}
	}
	w, err := buildWiring(Env{
		Dir:       cfg.Dir,
		Resources: resources,
		MCP:       mcp,
		Approve: func(ctx context.Context, r ApprovalRequest) (Approval, error) {
			answer, err := ctrl.Approve(ctx, resources.approvalRequest(ctx, r))
			if err == nil {
				resources.rememberApproval(r, answer)
			}
			return answer, err
		},
		Ask: func(ctx context.Context, q Question) (workspace.Answer, error) {
			return ctrl.Ask(ctx, q)
		},
		Decide: rt,
	})
	if err != nil {
		return nil, err
	}
	w.Hooks = attributeToolHooks(w.Hooks)
	if w.Scopes == nil {
		w.Scopes = map[string]string{}
	}
	w.Scopes["task"] = "CLI delegated work; creation approval grants no child effects; no child monetary cap"
	w.Scopes["task_status"] = "CLI recorded child state; read-only; no reference resolution"
	w.Scopes["task_stop"] = "CLI live child cancellation; verify the recorded acknowledgement"
	eng := NewEngine(EngineConfig{
		Dir:                 cfg.Dir,
		Client:              cfg.Client,
		Model:               cfg.Model,
		Store:               cfg.Store,
		Wiring:              w,
		Resources:           resources,
		MCP:                 mcp,
		ModelCalls:          cfg.ModelCalls,
		CompactionThreshold: cfg.CompactionThreshold,
		Emit: func(b Block) {
			rt.noteRow(b)
			emitToUI(ui, b)
		},
		ParentStopped: cfg.parentStopped,
		TurnEvents:    cfg.TurnEvents,
		Receipt: func(in *submittedInput, r inputReceipt) {
			if u, ok := ui.(attachmentUI); ok {
				u.AttachmentReceipt(in, r)
			}
		},
		recorder:    cfg.recorder,
		Preferences: cfg.Preferences,
	})
	resources.warning = eng.warn
	ctrl = NewController(ctx, eng, ui, cfg.Store, cfg.Dir)
	ctrl.approveAll = cfg.ApproveAll
	ctrl.decideOn = cfg.Decide
	ctrl.decide = rt
	ctrl.turnEvents = cfg.TurnEvents
	if cfg.TurnEvents != nil {
		cfg.TurnEvents.session = func() string {
			if s := eng.Session(); s != nil {
				return s.ID
			}
			return ""
		}
	}
	eng.taskWake = ctrl.scheduleTaskNotices
	if cfg.Preferences != nil {
		cfg.Preferences.ask = ctrl.Ask
	}
	return &app{eng: eng, ctrl: ctrl, wiring: w}, nil
}

func (a *app) modelLabel() string {
	if a.eng.cfg.Preferences.selectionReady() != nil {
		return "destination required"
	}
	return a.eng.Model()
}

// openSession opens the session the flags name, or a new one. Resuming a
// directory with no session starts a new one and says so.
func (a *app) openSession(
	store *sessionstore.Store,
	dir string,
	o Options,
) (notice string, err error) {
	dir, err = canonicalWorkspace(dir, "")
	if err != nil {
		return "", err
	}
	var s *sessionstore.Session
	if o.Resume {
		if o.ResumeID != "" {
			s, err = store.LoadScoped(context.Background(), dir, o.ResumeID)
		} else {
			s, err = store.Latest(dir)
		}
		if errors.Is(err, sessionstore.ErrNotFound) && o.ResumeID == "" {
			notice, s, err = "There is no session to resume. Starting a new one.", nil, nil
		}
		if err != nil {
			return "", err
		}
	}
	if s == nil && o.SessionID != "" {
		s, err = store.CreateWithID(dir, a.eng.Model(), o.SessionID)
		if errors.Is(err, sessionstore.ErrExists) {
			return "", fmt.Errorf("session %s already exists; reopen it with --resume %s", o.SessionID, o.SessionID)
		}
		if err != nil {
			return "", err
		}
	}
	if s == nil {
		s, err = store.Create(dir, a.eng.Model())
		if err != nil {
			return "", err
		}
	} else if a.eng.cfg.Preferences == nil && s.Model != "" && o.Model == "" {
		a.eng.SetModel(s.Model)
	}
	if err := a.eng.Open(s); err != nil {
		return "", err
	}
	if resources := a.eng.cfg.Resources; resources != nil {
		found, err := resources.discover()
		if err != nil {
			return "", err
		}
		if notice != "" {
			notice += "\n"
		}
		notice += resourceStartupSummary(found)
	}
	return notice, nil
}

// workDir returns the absolute, symlink-free current directory.
func workDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}
	return wd, nil
}
