package dashitest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"math/big"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
)

func TestMemoryRecorderConforms(t *testing.T) {
	dashitest.Run(t, func(*testing.T) dashitest.Store {
		return dashi.NewMemoryRecorder()
	})
}

func TestJSONBStoreConforms(t *testing.T) {
	dashitest.Run(t, func(*testing.T) dashitest.Store {
		return newJSONBStore()
	})
}

func TestJSONBStoreRespellsJSON(t *testing.T) {
	in := `{"required":["z"],"type":"object","text":"<a>",` +
		`"properties":{"z":{"maxLength":1e2}}}`
	want := `{"text": "<a>", "type": "object", "required": ["z"], ` +
		`"properties": {"z": {"maxLength": 100}}}`
	if got := string(jsonbNormalize([]byte(in))); got != want {
		t.Fatalf("jsonb store wrote %s; want %s", got, want)
	}
}

// brokenStores are recorders Run must fail, keyed by the environment value
// that selects one in a child test process, with the subtest that must fail.
var brokenStores = map[string]struct {
	store   func() dashitest.Store
	failing string
}{
	"ignores-claim": {
		store: func() dashitest.Store {
			return ignoresClaim{dashi.NewMemoryRecorder()}
		},
		failing: "RejectsStaleOwner",
	},
	"dedupes-by-kind": {
		store: func() dashitest.Store {
			return dedupesByKind{dashi.NewMemoryRecorder()}
		},
		failing: "RejectsRetimedStep",
	},
	"changes-meaning": {
		store: func() dashitest.Store {
			return changesMeaning{newJSONBStore()}
		},
		failing: "AppendsInOrder",
	},
}

const brokenEnv = "DASHITEST_BROKEN_STORE"

// TestBrokenStoresFail runs the suite against each broken store in a child
// process, because a failing Run must fail its own *testing.T.
func TestBrokenStoresFail(t *testing.T) {
	for name, broken := range brokenStores {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(
				os.Args[0],
				"-test.run=^TestBrokenStoreChild$",
				"-test.v",
			)
			cmd.Env = append(os.Environ(), brokenEnv+"="+name)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("suite passed a store that %s:\n%s", name, out)
			}
			want := "--- FAIL: TestBrokenStoreChild/" + broken.failing
			if !strings.Contains(string(out), want) {
				t.Fatalf("output does not contain %q:\n%s", want, out)
			}
		})
	}
}

func TestBrokenStoreChild(t *testing.T) {
	broken, ok := brokenStores[os.Getenv(brokenEnv)]
	if !ok {
		t.Skip("run by TestBrokenStoresFail")
	}
	dashitest.Run(t, func(*testing.T) dashitest.Store {
		return broken.store()
	})
}

// jsonbStore returns steps the way a Postgres jsonb column would: the same
// JSON meaning in different bytes. Keys come back shortest first, whitespace
// changes, HTML characters are no longer escaped, and exponent numbers are
// expanded (1e2 becomes 100).
type jsonbStore struct {
	*dashi.MemoryRecorder
}

func newJSONBStore() jsonbStore {
	return jsonbStore{dashi.NewMemoryRecorder()}
}

func (s jsonbStore) Load(ctx context.Context, id string) (dashi.Turn, error) {
	turn, err := s.MemoryRecorder.Load(ctx, id)
	if err != nil {
		return dashi.Turn{}, err
	}
	for i, step := range turn.Steps {
		raw, err := json.Marshal(step)
		if err != nil {
			return dashi.Turn{}, err
		}
		var stored dashi.Step
		if err := json.Unmarshal(jsonbNormalize(raw), &stored); err != nil {
			return dashi.Turn{}, err
		}
		turn.Steps[i] = stored
	}
	return turn, nil
}

func jsonbNormalize(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	writeJSONB(&buf, value)
	return buf.Bytes()
}

func writeJSONB(buf *bytes.Buffer, value any) {
	switch v := value.(type) {
	case map[string]any:
		keys := slices.SortedFunc(maps.Keys(v), func(a, b string) int {
			if len(a) != len(b) {
				return len(a) - len(b)
			}
			return strings.Compare(a, b)
		})
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteString(", ")
			}
			writeJSONB(buf, k)
			buf.WriteString(": ")
			writeJSONB(buf, v[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteString(", ")
			}
			writeJSONB(buf, item)
		}
		buf.WriteByte(']')
	case json.Number:
		n, ok := new(big.Rat).SetString(string(v))
		if ok && n.IsInt() {
			buf.WriteString(n.Num().String())
		} else {
			buf.WriteString(string(v))
		}
	default:
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			panic(err)
		}
		buf.Truncate(buf.Len() - 1) // Encode appends a newline.
	}
}

// ignoresClaim accepts every owner, so it has no lease fence.
type ignoresClaim struct {
	*dashi.MemoryRecorder
}

func (ignoresClaim) Claim(context.Context, string, string) error {
	return nil
}

// dedupesByKind treats any step at an occupied sequence number with the same
// kind as a retry, as an ON CONFLICT DO NOTHING keyed too loosely would. It
// would let a worker with an old snapshot run past the record.
type dedupesByKind struct {
	*dashi.MemoryRecorder
}

func (s dedupesByKind) Commit(
	ctx context.Context,
	req dashi.CommitRequest,
) error {
	turn, err := s.Load(ctx, req.Step.TurnID)
	if err == nil && int64(len(turn.Steps)) >= req.Step.Seq &&
		turn.Steps[req.Step.Seq-1].Kind == req.Step.Kind {
		return nil
	}
	return s.MemoryRecorder.Commit(ctx, req)
}

// changesMeaning alters a number inside raw JSON on load, which is a
// different value rather than a different spelling.
type changesMeaning struct {
	jsonbStore
}

func (s changesMeaning) Load(
	ctx context.Context,
	id string,
) (dashi.Turn, error) {
	turn, err := s.jsonbStore.Load(ctx, id)
	if err != nil || len(turn.Steps) == 0 || turn.Steps[0].Started == nil {
		return turn, err
	}
	schema := string(turn.Steps[0].Started.OutputSchema)
	turn.Steps[0].Started.OutputSchema = json.RawMessage(
		strings.Replace(schema, "100", "101", 1),
	)
	return turn, nil
}
