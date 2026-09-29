package delegate

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
)

func TestMaxTaskTurnsBoundsTheTurnsOfATask(t *testing.T) {
	for _, tc := range []struct {
		max, turns int
		refused    bool
	}{
		{max: 0, turns: 9},
		{max: 0, turns: 10, refused: true},
		{max: 2, turns: 1},
		{max: 2, turns: 2, refused: true},
		{max: -1, turns: 100},
	} {
		ts := NewTasks(Config{MaxTaskTurns: tc.max})
		task := newTask(Subagent{Name: "scout"}, dashi.CallRef{TurnID: "p", CallID: "c"}, "")
		for range tc.turns {
			task.turns = append(task.turns, &childTurn{state: "completed"})
		}
		err := ts.checkTurnsLocked(task)
		if tc.refused != (err != nil) {
			t.Fatalf("max=%d turns=%d: err=%v", tc.max, tc.turns, err)
		}
		if err != nil && !strings.Contains(err.Error(), "start a new task") {
			t.Fatalf("error does not suggest a new task: %v", err)
		}
	}
}
