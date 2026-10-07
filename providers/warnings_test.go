package providers_test

import (
	"cmp"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestPrepareSendsUnknownModelByDefault(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			server := serveFixtures(t, tc)
			p := configured(t, tc, clientConfig{endpoint: server.URL})
			prep, err := p.Prepare(t.Context(), modelInput())
			if err != nil {
				t.Fatalf("default policy rejected an unknown model: %v", err)
			}
			snapshot := prep.Snapshot()
			if snapshot.CapabilityPolicy != llm.CapabilityAdjust {
				t.Fatalf("recorded policy = %q, want adjust", snapshot.CapabilityPolicy)
			}
			if !slices.Contains(snapshot.Unverified, llm.FeatureText) {
				t.Fatal("unknown text support was not recorded as unverified")
			}
			if snapshot.ModelInfo != nil {
				t.Fatal("unknown model was recorded with evidence")
			}
			if len(snapshot.Warnings) != 0 {
				t.Fatal("unknown support alone produced a warning")
			}
			if _, err := p.Send(
				t.Context(),
				llm.Call{
					Request: prep,
				},
			); err != nil {
				t.Fatalf("send unknown model: %v", err)
			}
		})
	}
}

func TestPrepareClampsToNearestSupportedValue(t *testing.T) {
	for _, tc := range cases(t) {
		for _, test := range []struct {
			name     string
			provider string
			settings llm.Settings
			field    llm.Setting
			want     llm.Settings
		}{
			{
				name:     "effort below Messages levels",
				provider: "anthropic",
				settings: llm.Settings{
					Reasoning: &llm.ReasoningSettings{Effort: "minimal"},
				},
				field: llm.SettingReasoning,
				want: llm.Settings{
					Reasoning: &llm.ReasoningSettings{Effort: "low"},
				},
			},
			{
				name:     "effort above Responses levels",
				provider: "openai",
				settings: llm.Settings{
					Reasoning: &llm.ReasoningSettings{Effort: "max"},
				},
				field: llm.SettingReasoning,
				want: llm.Settings{
					Reasoning: &llm.ReasoningSettings{Effort: "xhigh"},
				},
			},
			{
				name:     "temperature above Messages range",
				provider: "anthropic",
				settings: llm.Settings{Temperature: new(1.5)},
				field:    llm.SettingTemperature,
				want:     llm.Settings{Temperature: new(1.0)},
			},
			{
				name:     "max tokens above model limit",
				provider: "openai",
				settings: llm.Settings{MaxTokens: 10000},
				field:    llm.SettingMaxTokens,
				want:     llm.Settings{MaxTokens: 8192},
			},
		} {
			if tc.name != test.provider {
				continue
			}
			t.Run(test.name, func(t *testing.T) {
				server := serveFixtures(t, tc)
				info := controlsModel(tc, server.URL)
				// Leave effort levels to the API so its own list is fitted.
				info.Capabilities.ReasoningEfforts = nil
				catalog := map[string]llm.ModelInfo{"requested": info}
				p := configured(
					t,
					tc,
					clientConfig{
						endpoint: server.URL,
						models:   catalog,
					},
				)
				for _, stream := range []bool{false, true} {
					r := modelInput()
					r.Stream, r.Settings = stream, test.settings
					prep, err := p.Prepare(t.Context(), r)
					if err != nil {
						t.Fatalf("prepare out-of-range setting: %v", err)
					}
					got := settingValue(t, prep.Effective(), test.field)
					if want := settingValue(t, test.want, test.field); got != want {
						t.Fatalf("effective %s = %s, want %s", test.field, got, want)
					}
					i := slices.IndexFunc(
						prep.Adjustments(),
						func(a llm.Adjustment) bool {
							return a.Field == test.field
						},
					)
					if i < 0 {
						t.Fatal("clamp was not recorded as an adjustment")
					}
					adjustment := prep.Adjustments()[i]
					if adjustment.Action != llm.AdjustmentClamp {
						t.Fatalf("adjustment action = %q, want clamp", adjustment.Action)
					}
					warning, ok := adjustment.Warning()
					if !ok || warning.Code != llm.WarningSettingClamped {
						t.Fatal("clamp adjustment was not marked as a warning")
					}
					want := []llm.Warning{warning}
					if !slices.Equal(prep.Warnings(), want) {
						t.Fatalf("request warnings = %+v", prep.Warnings())
					}
					if !slices.Equal(prep.Snapshot().Warnings, want) {
						t.Fatalf("snapshot warnings = %+v", prep.Snapshot().Warnings)
					}
					var result llm.Result
					if stream {
						for event, err := range llm.Stream(
							t.Context(),
							p,
							llm.Call{
								Request: prep,
							},
						) {
							if err != nil {
								t.Fatalf("stream adjusted request: %v", err)
							}
							if event.Result != nil {
								result = *event.Result
							}
						}
					} else {
						result, err = p.Send(t.Context(), llm.Call{Request: prep})
						if err != nil {
							t.Fatalf("send adjusted request: %v", err)
						}
					}
					if !slices.Equal(result.Warnings, want) {
						t.Fatalf("result warnings (stream %v) = %+v", stream, result.Warnings)
					}
				}
			})
		}
	}
}

func TestPrepareRejectsKnownMissingVisionWithFix(t *testing.T) {
	for _, tc := range cases(t) {
		for _, policy := range []llm.CapabilityPolicy{
			llm.CapabilityAdjust,
			llm.CapabilityStrict,
		} {
			t.Run(tc.name+"/"+cmp.Or(string(policy), "adjust"), func(t *testing.T) {
				endpoint := "https://example.com/api"
				info := textModel(tc, endpoint)
				info.Capabilities.Features[llm.FeatureImage] = llm.SupportUnsupported
				p := configured(
					t,
					tc,
					clientConfig{
						endpoint: endpoint,
						models:   map[string]llm.ModelInfo{"requested": info},
						policy:   policy,
					},
				)
				r := modelInput()
				image := content.ImageBlock(content.Source{
					URL: "https://example.com/image.png",
				})
				r.Messages[0].Content = append(r.Messages[0].Content, llm.Block(image))
				_, err := p.Prepare(t.Context(), r)
				if err == nil {
					t.Fatal("image input reached a model known to lack vision")
				}
				fix := "remove the image content or use a model that accepts it"
				if !strings.Contains(err.Error(), fix) {
					t.Fatalf("error does not name the fix: %v", err)
				}
			})
		}
	}
}

// serveFixtures answers each call with tc's complete or streamed fixture,
// following the stream flag in the request body.
func serveFixtures(t *testing.T, tc providerCase) *httptest.Server {
	t.Helper()
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Stream bool }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode outgoing fixture request: %v", err)
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, tc.stream)
			return
		}
		io.WriteString(w, tc.complete)
	})
}

// settingValue returns one setting's JSON as Settings encodes it.
func settingValue(t *testing.T, s llm.Settings, field llm.Setting) string {
	t.Helper()
	var fields map[llm.Setting]json.RawMessage
	if err := json.Unmarshal([]byte(mustJSON(s)), &fields); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	return string(fields[field])
}
