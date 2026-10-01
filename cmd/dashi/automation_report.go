package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/money"
)

// These private DTOs are the explicit version-1 presentation allowlist.
// None embeds a library record, model response, tool call, or settings object.
type automationResult struct {
	Version          int                   `json:"version"`
	Type             string                `json:"type"`
	Mode             string                `json:"mode"`
	ExitCode         int                   `json:"exit_code"`
	SessionID        string                `json:"session_id,omitempty"`
	RequestTurnID    string                `json:"request_turn_id,omitempty"`
	AnswerTurnID     string                `json:"answer_turn_id,omitempty"`
	Answer           string                `json:"answer"`
	StructuredOutput json.RawMessage       `json:"structured_output,omitempty"`
	Quiet            bool                  `json:"quiet"`
	Turns            []automationTurn      `json:"turns"`
	Tasks            []automationTask      `json:"tasks"`
	Affected         []automationAffected  `json:"affected"`
	Totals           automationTotals      `json:"totals"`
	Persistence      automationPersistence `json:"persistence"`
	Error            *automationError      `json:"error,omitempty"`
	Warnings         []string              `json:"warnings"`
}

type automationTarget struct {
	TurnID string `json:"turn_id,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	CallID string `json:"call_id,omitempty"`
}

type automationParent struct {
	TurnID string `json:"turn_id"`
	CallID string `json:"call_id"`
}

type automationDestination struct {
	ID    string `json:"id,omitempty"`
	API   string `json:"api,omitempty"`
	Model string `json:"model,omitempty"`
}

type automationTurn struct {
	TurnID           string                `json:"turn_id"`
	Revision         int64                 `json:"revision"`
	Origin           string                `json:"origin"`
	Parent           *automationParent     `json:"parent,omitempty"`
	Destination      automationDestination `json:"destination"`
	Stop             automationStop        `json:"stop"`
	Answer           string                `json:"answer"`
	StructuredOutput json.RawMessage       `json:"structured_output,omitempty"`
	PublicReasoning  string                `json:"public_reasoning,omitempty"`
	Pending          []automationPending   `json:"pending"`
	ModelAttempts    int                   `json:"model_attempts"`
	Spend            automationSpend       `json:"spend"`
	RequiredAction   automationAction      `json:"required_action"`
}

type automationTask struct {
	TaskID       string           `json:"task_id"`
	SessionID    string           `json:"session_id"`
	TurnID       string           `json:"turn_id"`
	Parent       automationParent `json:"parent"`
	Subagent     string           `json:"subagent"`
	State        string           `json:"state"`
	NoticeState  string           `json:"notice_state"`
	ChildTurnIDs []string         `json:"child_turn_ids"`
}

type automationStop struct {
	Kind      string           `json:"kind"`
	Reason    string           `json:"reason,omitempty"`
	Terminal  bool             `json:"terminal"`
	Projected bool             `json:"projected"`
	RefID     string           `json:"ref_id,omitempty"`
	Limit     *automationLimit `json:"limit,omitempty"`
}

type automationLimit struct {
	Kind          string `json:"kind"`
	Code          string `json:"code,omitempty"`
	Limit         int64  `json:"limit"`
	Used          int64  `json:"used"`
	CostLimit     string `json:"cost_limit"`
	CostUsed      string `json:"cost_used"`
	CostAtRisk    string `json:"cost_at_risk"`
	Currency      string `json:"currency,omitempty"`
	FitTokens     int    `json:"fit_tokens"`
	UnpricedModel string `json:"unpriced_model,omitempty"`
}

type automationPending struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	AttemptID string `json:"attempt_id,omitempty"`
	Status    string `json:"status,omitempty"`
}

type automationAction struct {
	Kind          string   `json:"kind,omitempty"`
	Prerequisites []string `json:"prerequisites,omitempty"`
}

type automationAffected struct {
	automationTarget
	Stop    automationStop   `json:"stop"`
	Rank    int              `json:"rank"`
	Action  automationAction `json:"action"`
	Primary bool             `json:"primary"`
}

type automationModalityUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type automationUsage struct {
	InputTokens                             int                                `json:"input_tokens"`
	OutputTokens                            int                                `json:"output_tokens"`
	CacheCreationInputTokens                int                                `json:"cache_creation_input_tokens"`
	CacheReadInputTokens                    int                                `json:"cache_read_input_tokens"`
	CacheCreation1hInputTokens              int                                `json:"cache_creation_1h_input_tokens"`
	ToolUseInputTokens                      int                                `json:"tool_use_input_tokens"`
	ReasoningTokens                         int                                `json:"reasoning_tokens"`
	Requests                                map[string]int                     `json:"requests"`
	RequestsFinal                           bool                               `json:"requests_final"`
	ModalityTokens                          map[string]automationModalityUsage `json:"modality_tokens"`
	InputModalityTokenDetailsIncomplete     bool                               `json:"input_modality_token_details_incomplete"`
	OutputModalityTokenDetailsIncomplete    bool                               `json:"output_modality_token_details_incomplete"`
	CacheReadModalityTokenDetailsIncomplete bool                               `json:"cache_read_modality_token_details_incomplete"`
	CacheCreationInputTokensUnavailable     bool                               `json:"cache_creation_input_tokens_unavailable"`
	TotalTokenCountMismatch                 bool                               `json:"total_token_count_mismatch"`
}

type automationSpend struct {
	Usage            automationUsage   `json:"usage"`
	UsageState       string            `json:"usage_state"`
	UsageIncomplete  bool              `json:"usage_incomplete"`
	Estimated        int               `json:"estimated"`
	ModelCost        map[string]string `json:"model_cost"`
	ToolCost         map[string]string `json:"tool_cost"`
	CostUnknown      int               `json:"cost_unknown"`
	CostPartial      int               `json:"cost_partial"`
	UnpricedRequests map[string]int    `json:"unpriced_requests"`
	OmittedRecords   []string          `json:"omitted_records"`
}

type automationCoverage struct {
	TurnID   string `json:"turn_id"`
	Revision int64  `json:"revision"`
}

type automationSpendScopes struct {
	Covered   []automationCoverage `json:"covered"`
	Parent    automationSpend      `json:"parent"`
	Children  automationSpend      `json:"children"`
	Inclusive automationSpend      `json:"inclusive"`
}

type automationTotals struct {
	Invocation automationSpendScopes `json:"invocation"`
	Session    automationSpendScopes `json:"session"`
}

type automationPersistence struct {
	Outcome  string           `json:"outcome,omitempty"`
	Target   automationTarget `json:"target"`
	Revision int64            `json:"revision"`
}

type automationError struct {
	Code       string `json:"code"`
	Diagnostic string `json:"diagnostic"`
}

func projectAutomationStop(stop dashi.Stop) automationStop {
	out := automationStop{Kind: string(stop.Kind), Reason: sanitizeAutomationDiagnostic(stop.Reason), Terminal: stop.Terminal, Projected: stop.Projected, RefID: stop.RefID}
	if l := stop.Limit; l != nil {
		out.Limit = &automationLimit{Kind: string(l.Kind), Code: safeAutomationIdentity(l.Code), Limit: l.Limit, Used: l.Used,
			CostLimit: l.CostLimit.String(), CostUsed: l.CostUsed.String(), CostAtRisk: l.CostAtRisk.String(), Currency: l.Currency,
			FitTokens: l.FitTokens, UnpricedModel: safeAutomationIdentity(l.UnpricedModel)}
	}
	return out
}

func projectAutomationTurn(view dashi.TurnView) automationTurn {
	out := automationTurn{TurnID: view.ID, Revision: view.Revision, Origin: string(view.Origin),
		Destination: automationDestination{safeAutomationIdentity(view.Agent.Provider), safeAutomationIdentity(view.Agent.API), safeAutomationIdentity(view.Agent.Model)},
		Stop:        projectAutomationStop(view.Stop), Answer: view.Answer, ModelAttempts: view.ModelAttempts,
		Spend: projectAutomationSpend(view.Spend, nil), Pending: []automationPending{}, RequiredAction: automationActionFor(view)}
	if view.Parent != nil {
		out.Parent = &automationParent{view.Parent.TurnID, view.Parent.CallID}
	}
	if view.Output != nil {
		out.StructuredOutput = slices.Clone(view.Output.Value)
	}
	for _, pending := range view.Pending {
		p := automationPending{Kind: string(pending.Kind), ID: pending.ID, AttemptID: pending.AttemptID}
		if pending.Tool != nil {
			p.Status = string(pending.Tool.Status)
		}
		out.Pending = append(out.Pending, p)
	}
	return out
}

// Never interpret arbitrary provider errors or header values as diagnostics.
// URL-shaped routing identities retain no userinfo, query, or fragment.
func safeAutomationIdentity(value string) string {
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil || u.Host == "" {
			return ""
		}
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		return u.String()
	}
	return value
}

var automationCredentialHeader = regexp.MustCompile(`(?i)(authorization|proxy-authorization|api[-_]key|access[-_]token|password|secret)[ \t]*[:=][^\r\n]*`)
var automationDiagnosticURL = regexp.MustCompile(`https?://[^\s"'<>]+`)

func sanitizeAutomationDiagnostic(value string) string {
	value = automationCredentialHeader.ReplaceAllString(value, "$1: [redacted]")
	return automationDiagnosticURL.ReplaceAllStringFunc(value, safeAutomationIdentity)
}

func projectAutomationSpend(spend dashi.Spend, omitted []string) automationSpend {
	u := spend.Usage
	usage := automationUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens: u.CacheReadInputTokens, CacheCreation1hInputTokens: u.CacheCreation1hInputTokens,
		ToolUseInputTokens: u.ToolUseInputTokens, ReasoningTokens: u.ReasoningTokens, Requests: maps.Clone(u.Requests), RequestsFinal: u.RequestsFinal,
		InputModalityTokenDetailsIncomplete: u.InputModalityTokenDetailsIncomplete, OutputModalityTokenDetailsIncomplete: u.OutputModalityTokenDetailsIncomplete,
		CacheReadModalityTokenDetailsIncomplete: u.CacheReadModalityTokenDetailsIncomplete, CacheCreationInputTokensUnavailable: u.CacheCreationInputTokensUnavailable,
		TotalTokenCountMismatch: u.TotalTokenCountMismatch, ModalityTokens: map[string]automationModalityUsage{}}
	for name, v := range u.ModalityTokens {
		usage.ModalityTokens[name] = automationModalityUsage{v.InputTokens, v.OutputTokens, v.CacheCreationInputTokens, v.CacheReadInputTokens}
	}
	state := "complete"
	if spend.Estimated > 0 {
		state = "estimated"
	}
	if spend.UsageIncomplete || len(omitted) > 0 {
		state = "incomplete"
	}
	return automationSpend{Usage: usage, UsageState: state, UsageIncomplete: spend.UsageIncomplete || len(omitted) > 0,
		Estimated: spend.Estimated, ModelCost: automationMoney(spend.Cost), ToolCost: automationMoney(spend.ToolCost),
		CostUnknown: spend.CostUnknown, CostPartial: spend.CostPartial, UnpricedRequests: maps.Clone(spend.UnpricedRequests),
		OmittedRecords: slices.Clone(omitted)}
}

func automationMoney(amounts map[string]money.Amount) map[string]string {
	out := make(map[string]string, len(amounts))
	for currency, amount := range amounts {
		out[currency] = amount.String()
	}
	return out
}

func automationActionFor(view dashi.TurnView) automationAction {
	switch view.Stop.Kind {
	case dashi.StopCompleted:
		return automationAction{}
	case dashi.StopUncertain:
		return automationAction{Kind: "reconcile"}
	case dashi.StopWaiting:
		for _, p := range view.Pending {
			if p.Kind == dashi.PendingApproval {
				return automationAction{Kind: "approve"}
			}
		}
		return automationAction{Kind: "reconcile"}
	case dashi.StopInterrupted:
		return automationAction{Kind: "continue", Prerequisites: []string{"validate live bindings and recovery readiness"}}
	case dashi.StopCanceled, dashi.StopRefused:
		return automationAction{Kind: "new_request"}
	default:
		return automationAction{Kind: "inspect"}
	}
}

func automationStopRank(kind string) int {
	switch dashi.StopKind(kind) {
	case dashi.StopFailed:
		return 1
	case dashi.StopWaiting, dashi.StopUncertain, dashi.StopInterrupted:
		return 2
	case dashi.StopLimited:
		return 3
	case dashi.StopRefused:
		return 4
	case dashi.StopCanceled:
		return 5
	case dashi.StopCompleted:
		return 0
	default:
		// An unknown or still-running stop cannot yield successful completion.
		return 2
	}
}

// rankAutomationAffected copies its inputs. Ranks are separate from exit
// codes; target ties apply within a rank only.
func rankAutomationAffected(input []automationAffected, originalParent string) []automationAffected {
	out := slices.Clone(input)
	for i := range out {
		out[i].Rank = automationStopRank(out[i].Stop.Kind)
		out[i].Primary = false
		out[i].Action.Prerequisites = slices.Clone(out[i].Action.Prerequisites)
		if out[i].Stop.Limit != nil {
			limit := *out[i].Stop.Limit
			out[i].Stop.Limit = &limit
		}
	}
	slices.SortFunc(out, func(a, b automationAffected) int {
		if v := cmp.Compare(a.Rank, b.Rank); v != 0 {
			return v
		}
		aParent, bParent := a.TurnID == originalParent, b.TurnID == originalParent
		if aParent != bParent {
			if aParent {
				return -1
			}
			return 1
		}
		if v := strings.Compare(a.TurnID, b.TurnID); v != 0 {
			return v
		}
		if v := strings.Compare(a.TaskID, b.TaskID); v != 0 {
			return v
		}
		return strings.Compare(a.CallID, b.CallID)
	})
	for i := range out {
		if out[i].Rank > 0 {
			out[i].Primary = true
			break
		}
	}
	return out
}

func automationExitCode(signalCode int, outputOrPersistenceFailure bool, preflightFailure bool, affected []automationAffected, quiet bool) int {
	if signalCode == 130 || signalCode == 143 {
		return signalCode
	}
	if outputOrPersistenceFailure {
		return 1
	}
	if preflightFailure {
		return 2
	}
	best := 0
	for _, target := range affected {
		rank := automationStopRank(target.Stop.Kind)
		if rank > 0 && (best == 0 || rank < best) {
			best = rank
		}
	}
	if best == 1 {
		return 1
	}
	if best == 2 {
		return 3
	}
	if best > 2 {
		return 4
	}
	if !quiet {
		return 3
	}
	return 0
}

// The controller supplies validated coverage. This helper never discovers
// children, loads sessions, consults credentials, or sums streamed previews.
type automationSpendRecord struct {
	View  dashi.TurnView
	Child bool
}

func sumAutomationRecords(records []automationSpendRecord, missingParent, missingChildren []string) (automationSpendScopes, error) {
	records = slices.Clone(records)
	slices.SortFunc(records, func(a, b automationSpendRecord) int { return strings.Compare(a.View.ID, b.View.ID) })
	var parent, children dashi.Spend
	covered := []automationCoverage{}
	seen := make(map[string]automationSpendRecord)
	for _, record := range records {
		if prior, ok := seen[record.View.ID]; ok {
			if prior.View.Revision != record.View.Revision || prior.Child != record.Child {
				return automationSpendScopes{}, errors.New("inconsistent covered Turn")
			}
			continue
		}
		seen[record.View.ID] = record
		covered = append(covered, automationCoverage{record.View.ID, record.View.Revision})
		if record.Child {
			children = children.Add(record.View.Spend)
		} else {
			parent = parent.Add(record.View.Spend)
		}
	}
	missing := append(slices.Clone(missingParent), missingChildren...)
	slices.Sort(missing)
	return automationSpendScopes{Covered: covered, Parent: projectAutomationSpend(parent, missingParent), Children: projectAutomationSpend(children, missingChildren), Inclusive: projectAutomationSpend(parent.Add(children), missing)}, nil
}
