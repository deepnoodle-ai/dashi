package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestTaskCopiesOriginalEvidenceAndResourcesAcrossRetryAndReopen(t *testing.T) {
	ctx := t.Context()
	store := sessionstore.NewStore(filepath.Join(t.TempDir(), "sessions"))
	meta, err := store.Create(t.TempDir(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.Lock(meta)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	attachments, err := store.Attachments(meta)
	if err != nil {
		t.Fatal(err)
	}
	defer attachments.Close()
	id, _ := sessionstore.NewAttachmentID()
	ref, err := attachments.Publish(ctx, id, "proof.txt", "text/plain", []byte("original accepted bytes"))
	if err != nil {
		t.Fatal(err)
	}
	recorder := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.CallTool("delegate-call", "task", `{"prompt":"inspect @literal-path"}`), dashitest.Reply("parent answer"))
	host := &taskInputs{loader: recorder, parent: attachments, clientFor: func(context.Context, dashi.Agent) (llm.Client, error) { return client, nil }}
	defer host.close()
	var prepared, original dashi.Start
	var refCall dashi.CallRef
	resource := dashi.ContextItem{ID: "old-resource", Name: "workspace-instructions", Origin: dashi.ContextRuntime, Snapshot: true, Message: llm.ReminderMessage(llm.Reminder{Name: "workspace-instructions", Tier: llm.ReminderContextual, Content: "old instructions"})}
	taskTool, err := tool.Func("task", "Capture accepted input", func(ctx context.Context, _ tool.Call, args struct {
		Prompt string `json:"prompt"`
	}) (tool.Outcome, error) {
		refCall, _ = dashi.CurrentCall(ctx)
		parent, _ := dashi.CurrentTurn(ctx)
		original = dashi.Start{CommandID: delegate.ChildID(refCall.TurnID, refCall.CallID) + "/start", Parent: &refCall, Agent: parent.Agent, Input: []llm.Message{llm.UserMessage(llm.Text(args.Prompt))}}
		original.Agent.ID = "general"
		original.Agent.Instructions = "child instructions"
		original.Agent.Limits.ModelCalls = 32
		var err error
		prepared, err = host.prepareStart(ctx, refCall, original)
		if err != nil {
			return tool.Outcome{}, err
		}
		resource.ID = "later-resource"
		resource.Message = llm.ReminderMessage(llm.Reminder{Name: "workspace-instructions", Tier: llm.ReminderContextual, Content: "later instructions"})
		return tool.Succeeded("accepted"), nil
	}, tool.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	runner := dashi.Runner{Client: client, Recorder: recorder, ContentLoader: attachments, Tools: tool.List{taskTool}, Hooks: dashi.ModelHook{Name: "resource-fixture", Func: func(ctx context.Context, plan dashi.ModelPlan) (dashi.ModelDecision, error) {
		return dashi.SeedContext(resource).Func(ctx, plan)
	}}.Hooks()}
	parentID := session.TurnID(meta.ID, 1)
	out, err := runner.Run(ctx, dashi.Turn{ID: parentID}, dashi.Start{Agent: dashi.Agent{Model: dashitest.Model, NoInheritedSettings: true, Settings: llm.Settings{Temperature: new(0.0)}, ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000}}, Input: []llm.Message{llm.UserMessage(llm.Text("unrelated parent prompt"), llm.Block(content.DocumentBlock(content.Source{MIMEType: "text/plain", Ref: &ref}, "proof.txt")))}})
	if err != nil || out.Stop.Kind != dashi.StopCompleted || prepared.Parent == nil {
		t.Fatalf("stop=%+v err=%v prepared=%+v", out.Stop, err, prepared)
	}
	if len(prepared.Input) != 3 || prepared.Input[1].Text() != "" || prepared.Input[2].Context == nil || prepared.Input[2].Context.Name != resource.Name || prepared.Input[2].Content[0].Reminder == nil || prepared.Input[2].Content[0].Reminder.Content != "old instructions" {
		t.Fatalf("unrelated text or missing resource identity: %+v", prepared.Input)
	}
	if prepared.Input[0].Text() != "inspect @literal-path" || prepared.Agent.Instructions != "child instructions" || prepared.Agent.Limits.ModelCalls != 32 || prepared.Agent.Settings.Temperature == nil || *prepared.Agent.Settings.Temperature != 0 || prepared.Agent.ActiveCompaction.Window != 16000 {
		t.Fatal("accepted behavior changed")
	}
	retried, err := host.prepareStart(ctx, refCall, original)
	if err != nil || !reflect.DeepEqual(prepared, retried) {
		t.Fatalf("retry changed accepted input: %v", err)
	}
	childRef := *prepared.Input[1].Content[0].Block.Document.Source.Ref
	if childRef.ID == ref.ID {
		t.Fatal("parent ref forwarded")
	}
	// Corrupt parent bytes after the child copy; only fresh preparation fails.
	if err := os.WriteFile(filepath.Join(store.Root(), meta.ID, "attachments", ref.ID, "blob"), []byte(strings.Repeat("x", int(ref.Size))), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.prepareStart(ctx, refCall, original); err == nil {
		t.Fatal("corrupt parent accepted")
	}
	host.close()
	recovered := &taskInputs{loader: recorder, parent: attachments, clientFor: host.clientFor}
	defer recovered.close()
	services, err := recovered.bindServices(ctx, refCall, prepared.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if services.Client != client {
		t.Fatal("recorded client was replaced by foreground service")
	}
	childTurn := session.TurnID(delegate.ChildID(refCall.TurnID, refCall.CallID), 1)
	data, err := services.ContentLoader.Load(ctx, childTurn, childRef)
	if err != nil || string(data) != "original accepted bytes" {
		t.Fatalf("recovery changed evidence: %q %v", data, err)
	}
	if _, err := services.ContentLoader.Load(ctx, parentID, childRef); err == nil {
		t.Fatal("parent authorized by child resolver")
	}
}

func TestTaskResourcesSkipLiveRediscovery(t *testing.T) {
	manager := newResourceManager(filepath.Join(t.TempDir(), "missing"), "")
	ref := dashi.CallRef{TurnID: "parent", CallID: "call"}
	decision, err := manager.beforeModel(t.Context(), dashi.ModelPlan{Turn: dashi.TurnView{Parent: &ref}})
	if err != nil || decision.Stop != nil || len(decision.AddMessages) != 0 {
		t.Fatalf("child read live resources: %+v %v", decision, err)
	}
}
