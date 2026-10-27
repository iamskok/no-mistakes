package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func doneResultSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"done":{"type":"boolean"}},"required":["done"]}`)
}

type taskEffects struct {
	calls   int
	edits   int
	tests   int
	formats int
	patch   string
}

func (e *taskEffects) writePatch(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(e.patch, []byte("saved-work"), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	e.edits++
}

func (e *taskEffects) assertKept(t *testing.T) {
	t.Helper()
	body, err := os.ReadFile(e.patch)
	if err != nil {
		t.Fatalf("completed work was not kept: %v", err)
	}
	if string(body) != "saved-work" {
		t.Fatalf("patch = %q, want saved-work", body)
	}
}

func (e *taskEffects) task(t *testing.T, text string, runTests bool, after func(context.Context) error) func(context.Context) (string, error) {
	t.Helper()
	return func(ctx context.Context) (string, error) {
		e.calls++
		if err := ctx.Err(); err != nil {
			return "", err
		}
		e.writePatch(t)
		if runTests {
			e.tests++
		}
		if after != nil {
			if err := after(ctx); err != nil {
				return "", err
			}
		}
		return text, nil
	}
}

func newEffects(t *testing.T) *taskEffects {
	t.Helper()
	return &taskEffects{patch: filepath.Join(t.TempDir(), "patch.txt")}
}

func assertStrictDone(t *testing.T, result *Result, want bool) {
	t.Helper()
	if result == nil || len(result.Output) == 0 {
		t.Fatal("expected a strict structured result")
	}
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("accepted output is not JSON: %v", err)
	}
	if output["done"] != want {
		t.Fatalf("done = %v, want %v", output["done"], want)
	}
	if _, ok := output["done"].(bool); !ok {
		t.Fatalf("done = %T, want bool", output["done"])
	}
}

func assertFormatKept(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a format failure")
	}
	if !strings.Contains(err.Error(), "completed work was kept") {
		t.Fatalf("error = %v, want a clear kept-work failure", err)
	}
	if !IsStructuredOutputRejected(err) {
		t.Fatalf("format failure = %v, want a structured-output rejection", err)
	}
}

func TestConsumeStructuredResult(t *testing.T) {
	schema := doneResultSchema()

	t.Run("valid schema", func(t *testing.T) {
		effects := newEffects(t)
		formats := 0
		result, err := ConsumeStructuredResult(context.Background(), RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: FormatRecoveryMaxAttempts,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					formats++
					return json.RawMessage(`{"done":false}`), nil
				},
			},
		}, effects.task(t, `{"done":true}`, true, nil))
		if err != nil {
			t.Fatalf("valid schema returned %v", err)
		}
		assertStrictDone(t, result, true)
		if effects.calls != 1 || effects.edits != 1 || effects.tests != 1 || formats != 0 {
			t.Fatalf("calls=%d edits=%d tests=%d formats=%d, want 1/1/1/0", effects.calls, effects.edits, effects.tests, formats)
		}
		effects.assertKept(t)
	})

	t.Run("JSON plus prose", func(t *testing.T) {
		effects := newEffects(t)
		text := "{\"done\":true}\nThe edits and tests are already complete."
		result, err := ConsumeStructuredResult(context.Background(), RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: 1,
				Format: func(_ context.Context, terminal string, _ json.RawMessage) (json.RawMessage, error) {
					effects.formats++
					if !strings.Contains(terminal, "already complete") {
						return nil, ErrFormatRecoveryFailed
					}
					return json.RawMessage(`{"done":true}`), nil
				},
			},
		}, effects.task(t, text, true, nil))
		if err != nil {
			t.Fatalf("format-only recovery returned %v", err)
		}
		assertStrictDone(t, result, true)
		if effects.calls != 1 || effects.edits != 1 || effects.tests != 1 || effects.formats != 1 {
			t.Fatalf("calls=%d edits=%d tests=%d formats=%d, want 1/1/1/1", effects.calls, effects.edits, effects.tests, effects.formats)
		}
		effects.assertKept(t)

		kept := newEffects(t)
		_, err = ConsumeStructuredResult(context.Background(), RunOpts{JSONSchema: schema}, kept.task(t, text, true, nil))
		assertFormatKept(t, err)
		if kept.calls != 1 || kept.edits != 1 || kept.tests != 1 {
			t.Fatalf("unenforced recovery replayed work: calls=%d edits=%d tests=%d", kept.calls, kept.edits, kept.tests)
		}
		kept.assertKept(t)
	})

	t.Run("malformed JSON", func(t *testing.T) {
		effects := newEffects(t)
		_, err := ConsumeStructuredResult(context.Background(), RunOpts{JSONSchema: schema}, effects.task(t, `{"done":`, true, nil))
		assertFormatKept(t, err)
		if effects.calls != 1 || effects.edits != 1 || effects.tests != 1 {
			t.Fatalf("malformed JSON replayed work: calls=%d edits=%d tests=%d", effects.calls, effects.edits, effects.tests)
		}
		effects.assertKept(t)
	})

	t.Run("schema-invalid JSON", func(t *testing.T) {
		effects := newEffects(t)
		result, err := ConsumeStructuredResult(context.Background(), RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: 1,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					effects.formats++
					return json.RawMessage(`{"done":"yes"}`), nil
				},
			},
		}, effects.task(t, `{"done":"yes"}`, true, nil))
		assertFormatKept(t, err)
		if result != nil && len(result.Output) != 0 {
			t.Fatalf("schema-invalid JSON was accepted: %s", result.Output)
		}
		if effects.calls != 1 || effects.edits != 1 || effects.tests != 1 || effects.formats != 1 {
			t.Fatalf("calls=%d edits=%d tests=%d formats=%d, want 1/1/1/1", effects.calls, effects.edits, effects.tests, effects.formats)
		}
		effects.assertKept(t)
	})

	t.Run("multiple earlier JSON objects", func(t *testing.T) {
		effects := newEffects(t)
		text := `{"done":true}{"done":false}`
		result, err := ConsumeStructuredResult(context.Background(), RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: 1,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					effects.formats++
					return json.RawMessage(`{"done":true}`), nil
				},
			},
		}, effects.task(t, text, true, nil))
		assertFormatKept(t, err)
		if !strings.Contains(err.Error(), "earlier JSON object") {
			t.Fatalf("error = %v, want the earlier object refused", err)
		}
		if result != nil && len(result.Output) != 0 {
			assertStrictDone(t, result, false)
		}
		if effects.calls != 1 || effects.edits != 1 || effects.tests != 1 || effects.formats != 1 {
			t.Fatalf("calls=%d edits=%d tests=%d formats=%d, want 1/1/1/1", effects.calls, effects.edits, effects.tests, effects.formats)
		}
		effects.assertKept(t)
	})

	t.Run("earlier draft when a later object fails the schema", func(t *testing.T) {
		effects := newEffects(t)
		text := `{"done":true}{"done":"no"}`
		result, err := ConsumeStructuredResult(context.Background(), RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: 1,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					effects.formats++
					return json.RawMessage(`{"done":true}`), nil
				},
			},
		}, effects.task(t, text, true, nil))
		assertFormatKept(t, err)
		if !strings.Contains(err.Error(), "earlier JSON object") {
			t.Fatalf("error = %v, want the earlier draft refused", err)
		}
		if result == nil || result.ResumeRetryable == nil || *result.ResumeRetryable {
			t.Fatalf("ResumeRetryable = %v, want false", result)
		}
		if len(result.Output) != 0 {
			t.Fatalf("earlier draft was accepted: %s", result.Output)
		}
		if effects.calls != 1 || effects.formats != 1 {
			t.Fatalf("calls=%d formats=%d, want 1/1", effects.calls, effects.formats)
		}
		effects.assertKept(t)
	})

	t.Run("fenced earlier draft when a later object fails the schema", func(t *testing.T) {
		effects := newEffects(t)
		text := "```json\n{\"done\":true}\n```\n{\"done\":false}"
		result, err := ConsumeStructuredResult(context.Background(), RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: 1,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					effects.formats++
					return json.RawMessage(`{"done":true}`), nil
				},
			},
		}, effects.task(t, text, true, nil))
		assertFormatKept(t, err)
		if !strings.Contains(err.Error(), "earlier JSON object") {
			t.Fatalf("error = %v, want the fenced earlier draft refused", err)
		}
		if result != nil && len(result.Output) != 0 {
			t.Fatalf("fenced earlier draft was accepted: %s", result.Output)
		}
		effects.assertKept(t)
	})

	t.Run("canceled task returning valid output is not success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		effects := newEffects(t)
		result, err := ConsumeStructuredResult(ctx, RunOpts{JSONSchema: schema}, func(context.Context) (string, error) {
			effects.calls++
			effects.edits++
			effects.tests++
			effects.writePatch(t)
			cancel()
			return `{"done":true}`, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want canceled", err)
		}
		if result != nil && len(result.Output) != 0 {
			t.Fatalf("canceled task reported output %s", result.Output)
		}
		if effects.calls != 1 {
			t.Fatalf("canceled task ran %d times", effects.calls)
		}
		effects.assertKept(t)
	})

	t.Run("canceled recovery returning valid output is not success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		effects := newEffects(t)
		result, err := ConsumeStructuredResult(ctx, RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: 1,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					effects.formats++
					cancel()
					return json.RawMessage(`{"done":true}`), nil
				},
			},
		}, effects.task(t, "{\"done\":true}\nalready complete", true, nil))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want canceled", err)
		}
		if result != nil && len(result.Output) != 0 {
			t.Fatalf("canceled recovery reported output %s", result.Output)
		}
		if result == nil || result.ResumeRetryable == nil || *result.ResumeRetryable {
			t.Fatalf("ResumeRetryable = %v, want false", result)
		}
		if effects.calls != 1 || effects.formats != 1 {
			t.Fatalf("calls=%d formats=%d, want 1/1", effects.calls, effects.formats)
		}
		effects.assertKept(t)
	})

	t.Run("timeout", func(t *testing.T) {
		effects := newEffects(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		formats := 0
		_, err := ConsumeStructuredResult(ctx, RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: FormatRecoveryMaxAttempts,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					formats++
					return json.RawMessage(`{"done":true}`), nil
				},
			},
		}, func(ctx context.Context) (string, error) {
			effects.calls++
			<-ctx.Done()
			return "", ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want deadline exceeded", err)
		}
		if effects.calls != 1 || effects.edits != 0 || formats != 0 {
			t.Fatalf("timeout replayed work: calls=%d edits=%d formats=%d", effects.calls, effects.edits, formats)
		}
	})

	t.Run("cancellation before edits", func(t *testing.T) {
		effects := newEffects(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		formats := 0
		_, err := ConsumeStructuredResult(ctx, RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: 1,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					formats++
					return json.RawMessage(`{"done":true}`), nil
				},
			},
		}, effects.task(t, `{"done":true}`, true, nil))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want cancellation", err)
		}
		if effects.calls != 0 || effects.edits != 0 || effects.tests != 0 || formats != 0 {
			t.Fatalf("cancelled before start: calls=%d edits=%d tests=%d formats=%d", effects.calls, effects.edits, effects.tests, formats)
		}
	})

	t.Run("cancellation after edits", func(t *testing.T) {
		effects := newEffects(t)
		formats := 0
		_, err := ConsumeStructuredResult(context.Background(), RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: 1,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					formats++
					return json.RawMessage(`{"done":true}`), nil
				},
			},
		}, func(ctx context.Context) (string, error) {
			effects.calls++
			effects.writePatch(t)
			effects.tests++
			return "", context.Canceled
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want cancellation", err)
		}
		if effects.calls != 1 || effects.edits != 1 || effects.tests != 1 || formats != 0 {
			t.Fatalf("cancellation replayed work: calls=%d edits=%d tests=%d formats=%d", effects.calls, effects.edits, effects.tests, formats)
		}
		effects.assertKept(t)
	})

	t.Run("recovery failure", func(t *testing.T) {
		effects := newEffects(t)
		_, err := ConsumeStructuredResult(context.Background(), RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: FormatRecoveryMaxAttempts,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					effects.formats++
					return nil, ErrFormatRecoveryFailed
				},
			},
		}, effects.task(t, "{\"done\":true}\nalready complete", true, nil))
		assertFormatKept(t, err)
		if !strings.Contains(err.Error(), "format-only recovery failed") {
			t.Fatalf("error = %v, want a recovery failure", err)
		}
		if effects.calls != 1 || effects.formats != 1 || effects.edits != 1 || effects.tests != 1 {
			t.Fatalf("recovery failure replayed work: calls=%d edits=%d tests=%d formats=%d", effects.calls, effects.edits, effects.tests, effects.formats)
		}
		effects.assertKept(t)
	})

	t.Run("exhausted bound", func(t *testing.T) {
		effects := newEffects(t)
		_, err := ConsumeStructuredResult(context.Background(), RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound: FormatRecoveryMaxAttempts + 5,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					effects.formats++
					return nil, errors.New("terminal text is still not strict JSON")
				},
			},
		}, effects.task(t, "{\"done\":true}\nalready complete", true, nil))
		assertFormatKept(t, err)
		if !strings.Contains(err.Error(), "exhausted 2 attempts") {
			t.Fatalf("error = %v, want the attempt bound exhausted", err)
		}
		if effects.calls != 1 || effects.formats != FormatRecoveryMaxAttempts || effects.edits != 1 || effects.tests != 1 {
			t.Fatalf("exhausted bound replayed work: calls=%d edits=%d tests=%d formats=%d", effects.calls, effects.edits, effects.tests, effects.formats)
		}
		effects.assertKept(t)
	})

	t.Run("recovery that can edit is refused", func(t *testing.T) {
		effects := newEffects(t)
		_, err := ConsumeStructuredResult(context.Background(), RunOpts{
			JSONSchema: schema,
			FormatRecovery: &FormatRecovery{
				Bound:         1,
				CanEditOrTest: true,
				Format: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					effects.formats++
					effects.edits++
					effects.tests++
					return json.RawMessage(`{"done":true}`), nil
				},
			},
		}, effects.task(t, "{\"done\":true}\nalready complete", true, nil))
		assertFormatKept(t, err)
		if !strings.Contains(err.Error(), "can edit or run tests") {
			t.Fatalf("error = %v, want the effectful recovery refused", err)
		}
		if effects.calls != 1 || effects.formats != 0 || effects.edits != 1 || effects.tests != 1 {
			t.Fatalf("effectful recovery ran: calls=%d edits=%d tests=%d formats=%d", effects.calls, effects.edits, effects.tests, effects.formats)
		}
		effects.assertKept(t)
	})
}

func TestRunWithRetry_FormatFailureDoesNotReplayTheTask(t *testing.T) {
	schema := doneResultSchema()
	text := "{\"done\":true}\nThe edits and tests are already complete."
	calls := 0
	_, err := runWithRetry(context.Background(), "acp:cursor", RunOpts{JSONSchema: schema}, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		return finalizeTextResult("acp:cursor", text, schema, TokenUsage{})
	})
	assertFormatKept(t, err)
	if calls != 1 {
		t.Fatalf("format failure ran the task %d times, want 1", calls)
	}
	if _, retry := classifyTransient(err); retry {
		t.Fatalf("kept format failure was classified as a full retry: %v", err)
	}

	calls = 0
	formats := 0
	result, err := runWithRetry(context.Background(), "acp:cursor", RunOpts{
		JSONSchema: schema,
		FormatRecovery: &FormatRecovery{
			Bound: 1,
			Format: func(_ context.Context, terminal string, _ json.RawMessage) (json.RawMessage, error) {
				formats++
				if !strings.Contains(terminal, "already complete") {
					return nil, ErrFormatRecoveryFailed
				}
				return json.RawMessage(`{"done":true}`), nil
			},
		},
	}, 3, classifyTransient, nil, func() (*Result, error) {
		calls++
		return finalizeTextResult("acp:cursor", text, schema, TokenUsage{})
	})
	if err != nil {
		t.Fatalf("bounded format recovery returned %v", err)
	}
	assertStrictDone(t, result, true)
	if calls != 1 || formats != 1 {
		t.Fatalf("calls=%d formats=%d, want 1/1", calls, formats)
	}
}

func TestSelectsEarlierResult_InvalidLaterObjectKeepsTheDraft(t *testing.T) {
	schema := doneResultSchema()
	for _, terminal := range []string{
		`{"done":true}{"done":"no"}`,
		"```json\n{\"done\":true}\n```\n{\"done\":\"no\"}\n",
	} {
		if !selectsEarlierResult(terminal, schema, json.RawMessage(`{"done":true}`)) {
			t.Fatalf("earlier draft accepted from %q", terminal)
		}
	}
}

func TestFallback_CompletedFormatFailureIsNotResumeRetryable(t *testing.T) {
	schema := doneResultSchema()
	primary := &formatResultAgent{name: "primary", schema: schema}
	spare := &formatResultAgent{name: "spare", schema: schema}
	wrapped := NewFallback([]Agent{primary, spare})
	result, err := wrapped.Run(context.Background(), RunOpts{
		Session: &SessionRef{ID: "fixer-session", Agent: "primary"},
	})
	assertFormatKept(t, err)
	if result == nil || result.ResumeRetryable == nil || *result.ResumeRetryable {
		t.Fatalf("ResumeRetryable = %v, want false", result)
	}
	if primary.calls != 1 || spare.calls != 0 {
		t.Fatalf("primary=%d spare=%d, want 1/0", primary.calls, spare.calls)
	}
}

type formatResultAgent struct {
	name   string
	schema json.RawMessage
	calls  int
}

func (a *formatResultAgent) Name() string                { return a.name }
func (a *formatResultAgent) SupportsSessionResume() bool { return true }
func (a *formatResultAgent) Close() error                { return nil }
func (a *formatResultAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	a.calls++
	opts.JSONSchema = a.schema
	return ConsumeStructuredResult(ctx, opts, func(context.Context) (string, error) {
		return "{\"done\":true}\nalready complete", nil
	})
}
