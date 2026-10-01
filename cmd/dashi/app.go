package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// appConfig holds what buildApp needs.
type appConfig struct {
	Dir           string
	HomeDir       string
	Model         string
	Client        llm.Client
	Store         *session.Store
	Preferences   *cliPreferences
	InvocationDir string
	MCPFiles      []string
	// ModelAttempts overrides the per-turn model-call limit; zero means 40.
	ModelAttempts int
	parentStopped func(int)

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
		Dir:           cfg.Dir,
		Client:        cfg.Client,
		Model:         cfg.Model,
		Store:         cfg.Store,
		Wiring:        w,
		Resources:     resources,
		MCP:           mcp,
		ModelAttempts: cfg.ModelAttempts,
		Emit:          func(b Block) { emitToUI(ui, b) },
		ParentStopped: cfg.parentStopped,
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
	store *session.Store,
	dir string,
	o Options,
) (notice string, err error) {
	dir, err = canonicalWorkspace(dir, "")
	if err != nil {
		return "", err
	}
	var s *session.Session
	if o.Resume {
		if o.ResumeID != "" {
			s, err = store.LoadScoped(context.Background(), dir, o.ResumeID)
		} else {
			s, err = store.Latest(dir)
		}
		if errors.Is(err, session.ErrNotFound) && o.ResumeID == "" {
			notice, s, err = "There is no session to resume. Starting a new one.", nil, nil
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
	} else if a.eng.cfg.Preferences == nil && s.Model != "" && o.Model == "" && os.Getenv("DASHI_MODEL") == "" {
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
