package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// FormatRecoveryMaxAttempts is the hard cap on format-only recovery attempts
// after a completed task whose terminal text failed the result schema.
const FormatRecoveryMaxAttempts = 2

// ErrFormatRecoveryFailed stops format-only recovery immediately. The completed
// task is not run again.
var ErrFormatRecoveryFailed = errors.New("format-only recovery failed")

// FormatRecovery restates a captured terminal result as schema JSON.
// Format receives only that text and the schema. It is invoked only when
// CanEditOrTest is false, Bound is at least 1, and Format is non-nil.
// Anything else fails the format error without another task run.
type FormatRecovery struct {
	Bound         int
	Format        func(ctx context.Context, terminal string, schema json.RawMessage) (json.RawMessage, error)
	CanEditOrTest bool
}

// terminalResultError carries the exact terminal text of a rejected structured
// result. The text is not copied into the error string, which stays the
// original diagnostic.
type terminalResultError struct {
	err  error
	text string
}

func (e *terminalResultError) Error() string { return e.err.Error() }
func (e *terminalResultError) Unwrap() error { return e.err }

func attachTerminalResult(err error, text string) error {
	if err == nil || text == "" {
		return err
	}
	return &terminalResultError{err: err, text: text}
}

func terminalResultText(err error) (string, bool) {
	var terminal *terminalResultError
	if !errors.As(err, &terminal) || terminal.text == "" {
		return "", false
	}
	return terminal.text, true
}

// isResultFormatError reports a completed turn whose terminal text was rejected
// as structured output. Cancellation and deadlines are not format failures, and
// neither is a structured-output rejection that did not capture terminal text
// (for example a missing Pi output tool call, which the retry loop still owns).
func isResultFormatError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	_, ok := terminalResultText(err)
	return ok && IsStructuredOutputRejected(err)
}

// ConsumeStructuredResult runs task once. Schema-valid terminal text is
// accepted. A result-format failure does not run task again. When recovery is
// enforceable, Format may restate the terminal text up to the attempt bound and
// the result is accepted only when it validates against the same schema and is
// not an earlier JSON object from that text. Otherwise the format error is
// returned and the task's effects stay as they were.
func ConsumeStructuredResult(ctx context.Context, opts RunOpts, task func(context.Context) (string, error)) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	text, err := task(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, parseErr := finalizeTextResult("structured-result", text, opts.JSONSchema, TokenUsage{})
	if parseErr == nil {
		return result, nil
	}
	if !isResultFormatError(parseErr) {
		return result, parseErr
	}
	return recoverResultFormat(ctx, opts, result, text, parseErr)
}

func recoverResultFormat(ctx context.Context, opts RunOpts, result *Result, terminal string, cause error) (*Result, error) {
	recovery := opts.FormatRecovery
	if recovery == nil || recovery.Format == nil || recovery.CanEditOrTest || recovery.Bound < 1 {
		return keptFormatResult(result), formatKept(cause, formatRecoveryRefusal(recovery))
	}
	bound := recovery.Bound
	if bound > FormatRecoveryMaxAttempts {
		bound = FormatRecoveryMaxAttempts
	}
	var lastErr error
	for attempt := 1; attempt <= bound; attempt++ {
		if err := ctx.Err(); err != nil {
			return keptFormatResult(result), err
		}
		output, err := recovery.Format(ctx, terminal, opts.JSONSchema)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return keptFormatResult(result), err
			}
			lastErr = err
			if errors.Is(err, ErrFormatRecoveryFailed) {
				return keptFormatResult(result), formatKept(cause, "format-only recovery failed: "+err.Error())
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return keptFormatResult(result), err
		}
		if err := acceptFormattedResult(opts.JSONSchema, output); err != nil {
			lastErr = err
			continue
		}
		if selectsEarlierResult(terminal, opts.JSONSchema, output) {
			lastErr = errors.New("format-only recovery selected an earlier JSON object instead of the terminal outcome")
			continue
		}
		return withStructuredOutput(result, terminal, output), nil
	}
	return keptFormatResult(result), formatKept(cause, fmt.Sprintf("format-only recovery exhausted %d attempts: %v", bound, lastErr))
}

// keptFormatResult marks a completed format failure as ineligible for session
// resume. A nil flag is treated as retryable once a resumed session id is
// present, which would run the same fixer turn again in a fresh session.
func keptFormatResult(result *Result) *Result {
	var kept Result
	if result != nil {
		kept = *result
	}
	retryable := false
	kept.ResumeRetryable = &retryable
	return &kept
}

func formatRecoveryRefusal(recovery *FormatRecovery) string {
	switch {
	case recovery != nil && recovery.CanEditOrTest:
		return "format-only recovery was refused because it can edit or run tests"
	case recovery != nil && recovery.Format != nil && recovery.Bound < 1:
		return "format-only recovery was not attempted because its attempt bound is empty"
	default:
		return "format-only recovery was not attempted because a recovery request cannot be separated from edits and tests"
	}
}

func formatKept(cause error, reason string) error {
	return fmt.Errorf("result format failed and completed work was kept; %s: %w", reason, cause)
}

func acceptFormattedResult(schema, output json.RawMessage) error {
	if len(bytes.TrimSpace(output)) == 0 {
		return errors.New("format-only recovery returned no JSON object")
	}
	validationSchema, err := textValidationSchema(schema)
	if err != nil {
		return err
	}
	_, err = parseStructuredCandidate(output, validationSchema)
	return err
}

func selectsEarlierResult(terminal string, schema, output json.RawMessage) bool {
	if _, err := textValidationSchema(schema); err != nil {
		return true
	}
	objects := orderedJSONObjects(terminal)
	if len(objects) < 2 {
		return false
	}
	if jsonEqual(output, objects[len(objects)-1]) {
		return false
	}
	for _, earlier := range objects[:len(objects)-1] {
		if jsonEqual(output, earlier) {
			return true
		}
	}
	return false
}

// orderedJSONObjects returns every JSON object in text order. Schema-invalid
// objects and fenced JSON objects stay in the list, so an earlier draft is
// still visible when a later object fails the schema.
func orderedJSONObjects(text string) []json.RawMessage {
	var objects []json.RawMessage
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], "```") {
			contentStart, info := fenceContentStart(text, i)
			if strings.EqualFold(strings.TrimSpace(info), "json") && contentStart > i && contentStart <= len(text) {
				body := text[contentStart:]
				end, closeEnd := indexJSONFenceClose(body)
				candidate := body
				next := len(text)
				if end >= 0 {
					candidate = body[:end]
					next = contentStart + closeEnd
				}
				if obj, ok := rawJSONObject(candidate); ok {
					objects = append(objects, obj)
					if next <= i {
						i++
					} else {
						i = next
					}
					continue
				}
			}
			i += 3
			continue
		}
		if text[i] == '{' {
			end, ok := scanBalancedObject(text, i)
			if ok {
				if obj, ok := rawJSONObject(text[i:end]); ok {
					objects = append(objects, obj)
				}
				if end <= i {
					i++
				} else {
					i = end
				}
				continue
			}
		}
		i++
	}
	return objects
}

func rawJSONObject(candidate string) (json.RawMessage, bool) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || candidate[0] != '{' {
		return nil, false
	}
	var value any
	if err := json.Unmarshal([]byte(candidate), &value); err != nil {
		return nil, false
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, false
	}
	return json.RawMessage(candidate), true
}

func withStructuredOutput(base *Result, terminal string, output json.RawMessage) *Result {
	var res Result
	if base != nil {
		res = *base
	}
	res.Text = terminal
	res.Output = append(json.RawMessage(nil), output...)
	return &res
}
