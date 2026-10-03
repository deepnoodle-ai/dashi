// Package listtool gives an agent tools for a host list too long to read,
// such as 300 open tickets: judge_items hands the reading to a decision
// model, such as TypeSafe's Jev, through the Decide library, so no LLM
// reads every item.
//
// A host defines each list once, as a List: a name, a description the
// model reads, an Items function that returns the items for a query, and
// ID and Text functions for each item. Config holds the lists for both
// tools:
//
//	tickets := listtool.List[Ticket]{
//		Name:        "tickets",
//		Description: "Open support tickets. The query is a search such as \"label:billing\".",
//		Items:       searchTickets,
//		ID:          func(t Ticket) string { return t.ID },
//		Text:        func(t Ticket) string { return t.Subject + "\n" + t.Body },
//	}
//	cfg := listtool.Config{Lists: []listtool.AnyList{tickets}}
//	judge, err := listtool.JudgeItems(cfg)
//	// handle err
//	runner.Tools = tool.List{judge, readTicket}
//
// judge_items asks the same questions, yes_no or choice, about every item a
// query returns, one Decide request per item. It returns counts of each
// answer, the IDs of the items that match the answers the model asked for
// (find), best first, and close calls the model should read itself. The
// result never holds item text, so an item cannot speak to the model.
// MaxQuestions, MaxRequests, and MaxTurnRequests bound each call before it
// spends anything; a call over a limit fails at once and says why, so the
// model narrows its query.
//
// The tool is read-only. When Decide fails, a call returns an ordinary
// failed result that tells the model to use its other tools or ask the
// user; it never guesses and never stops the turn. Each result
// stays under the turn's tool result limit, so Dashi keeps its
// Outcome.Data: the list, query, model, request count, token usage, and
// every item's answers. With Price set, Outcome.Cost carries the spend,
// which Dashi adds to Spend.ToolCost, failed calls included. OnResult
// receives every call's full Result, also when the call is cancelled and
// Dashi records no Cost.
//
// The tool claims no accuracy: an answer counts when its probability is
// above 0.5. Pin the model with decide.WithModel.
package listtool
