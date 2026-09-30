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
	Dir    string
	Model  string
	Client llm.Client
	Store  *session.Store
	// ModelAttempts overrides the per-turn model-call limit; zero means 40.
	ModelAttempts int

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
	var ctrl *Controller
	w, err := buildWiring(Env{
		Dir: cfg.Dir,
		Approve: func(ctx context.Context, r ApprovalRequest) (Approval, error) {
			return ctrl.Approve(ctx, r)
		},
		Ask: func(ctx context.Context, q Question) (workspace.Answer, error) {
			return ctrl.Ask(ctx, q)
		},
	})
	if err != nil {
		return nil, err
	}
	eng := NewEngine(EngineConfig{
		Dir:           cfg.Dir,
		Client:        cfg.Client,
		Model:         cfg.Model,
		Store:         cfg.Store,
		Wiring:        w,
		ModelAttempts: cfg.ModelAttempts,
		Emit:          func(b Block) { ui.Emit(printable(b)) },
		recorder:      cfg.recorder,
	})
	ctrl = NewController(ctx, eng, ui, cfg.Store, cfg.Dir)
	return &app{eng: eng, ctrl: ctrl, wiring: w}, nil
}

// openSession opens the session the flags name, or a new one. Resuming a
// directory with no session starts a new one and says so.
func (a *app) openSession(
	store *session.Store,
	dir string,
	o Options,
) (notice string, err error) {
	var s *session.Session
	if o.Resume {
		if o.ResumeID != "" {
			s, err = store.Load(o.ResumeID)
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
	} else if s.Model != "" && o.Model == "" && os.Getenv("DASHI_MODEL") == "" {
		a.eng.SetModel(s.Model)
	}
	if err := a.eng.Open(s); err != nil {
		return "", err
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
