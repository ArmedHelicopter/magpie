package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// A model closes a tool call's arguments with the wrong bracket now and
// then: the array it closes with '}', the object with ']', or with one
// bracket too many. The client cannot parse the arguments, the call never
// runs, and the turn stops without a word (#7678). The gateway fixes the
// brackets before the client sees them.

// brokenArgs is the arguments of the call that stopped the turn of the
// session of 2026-11-19, as the model wrote them: it closed the array of
// observations with '}' and wrote one '}' too many, so the arguments held
// six closers where four were meant.
const brokenArgs = `{"entities": [{"name": "全局工作规则", "entityType": "rule", "observations": ["填写或引用学术论文时，绝不凭借刻板印象、记忆或猜测填写论文题目、作者、期刊、年份与数据；必须先用联网检索或原文核实，确认无误后再写入文档。", "禁止使用 DeepSeek 官方搜索（其 web_search 走 DeepSeek API，HTTP 402 不可用且被用户禁用）；联网检索改用 firecrawl 或 magpie-web-search 等工具。", "使用 firecrawl 之前必须先拉起 Docker：启动 Docker Desktop 并使 firecrawl 本地服务（docker compose up）就绪后再调用 firecrawl 工具。"}]}]}`

// wantArgs is the same arguments with their brackets as they were meant.
const wantArgs = `{"entities": [{"name": "全局工作规则", "entityType": "rule", "observations": ["填写或引用学术论文时，绝不凭借刻板印象、记忆或猜测填写论文题目、作者、期刊、年份与数据；必须先用联网检索或原文核实，确认无误后再写入文档。", "禁止使用 DeepSeek 官方搜索（其 web_search 走 DeepSeek API，HTTP 402 不可用且被用户禁用）；联网检索改用 firecrawl 或 magpie-web-search 等工具。", "使用 firecrawl 之前必须先拉起 Docker：启动 Docker Desktop 并使 firecrawl 本地服务（docker compose up）就绪后再调用 firecrawl 工具。"]}]}`

func TestRepairArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"the session's broken arguments", brokenArgs, wantArgs},
		{"an array closed with an object", `{"a": [1, 2}`, `{"a": [1, 2]}`},
		{"an object closed with an array", `{"a": 1]`, `{"a": 1}`},
		{"one bracket too many", `{"a": 1}}`, `{"a": 1}`},
		{"one bracket too many, twice", `{"a": 1}}}`, `{"a": 1}`},
		{"a bracket closed early is not recoverable", `{"a": 1], "b": 2}`, `{"a": 1], "b": 2}`},
		{"brackets left open", `{"a": 1`, `{"a": 1}`},
		{"brackets left open, in an array", `{"a": [1, 2`, `{"a": [1, 2]}`},
		{"a string left open", `{"a": "b`, `{"a": "b"}`},
		{"a bracket in a string", `{"a": "]}", "b": 1}`, `{"a": "]}", "b": 1}`},
		{"an escaped quote in a string", `{"a": "b\"}", "b": 1}`, `{"a": "b\"}", "b": 1}`},
		{"a bracket in a string, broken", `{"a": "]}"}`, `{"a": "]}"}`},
		{"whole arguments are said as they came", `{"a": [1, {"b": 2}], "c": "d"}`, `{"a": [1, {"b": 2}], "c": "d"}`},
		{"text that is not JSON is said as it came", `not json at all`, `not json at all`},
		{"two objects said one after the other are said as they came", `{"a": 1}{"b": 2}`, `{"a": 1}{"b": 2}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := repairArgs(c.in)
			if got != c.want {
				t.Fatalf("repairArgs(%q)\ngot  %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// TestRepairArgsWholeIsWhole says the arguments of a call that the model
// wrote whole are whole after the fix, or the fix has said something that
// is not JSON at all.
func TestRepairArgsWholeIsWhole(t *testing.T) {
	cases := []string{
		brokenArgs,
		`{"a": [1, 2}`,
		`{"a": 1]`,
		`{"a": 1}}`,
		`{"a": 1`,
		`{"a": "b`,
		`{"a": [1, 2], "b": {"c": [3]}`,
	}
	for _, in := range cases {
		out := repairArgs(in)
		if !json.Valid([]byte(out)) {
			t.Fatalf("repairArgs(%q) is not JSON: %q", in, out)
		}
	}
}

// TestRepairArgsPiecesAreWhole says the fix does not depend on where the
// arguments were cut: the same arguments in pieces give the same text as
// the whole, and the text is whole either way.
func TestRepairArgsPiecesAreWhole(t *testing.T) {
	for _, in := range []string{brokenArgs, `{"a": [1, 2}`, `{"a": 1]`, `{"a": 1`, `{"a": "b`} {
		want := repairArgs(in)
		for _, n := range []int{1, 2, 3, 7, 16, 64} {
			var b strings.Builder
			var br jsonBrackets
			for i := 0; i < len(in); i += n {
				j := min(i+n, len(in))
				b.WriteString(br.feed(in[i:j]))
			}
			got := b.String() + br.close()
			if got != want {
				t.Fatalf("in pieces of %d: got %q\nwant %q", n, got, want)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("in pieces of %d: not JSON: %q", n, got)
			}
		}
	}
}

// TestRepairArgsSaysWholeAsItCame says arguments that are whole JSON
// reach the client as they came, byte for byte.
func TestRepairArgsSaysWholeAsItCame(t *testing.T) {
	for _, in := range []string{
		`{}`,
		`{"a": 1}`,
		`{"a": [1, 2, {"b": null}]}`,
		`{"a": "]}"}`,
		`{"a": "\"}"}`,
		`{"a": 1.5, "b": -2e3}`,
	} {
		if got := repairArgs(in); got != in {
			t.Fatalf("repairArgs(%q) = %q", in, got)
		}
	}
}

// TestJsonRepairEncoder says the arguments of a call that the model closed
// wrong are whole when the encoder says them.
func TestJsonRepairEncoder(t *testing.T) {
	var got strings.Builder
	enc := &jsonRepairEncoder{streamEncoder: &textEncoder{w: &got}}
	enc.event(Event{Kind: KToolStart, ID: "call_1", Name: "m"})
	for _, piece := range []string{
		`{"a": [1, 2}`,
		`]}`,
	} {
		enc.event(Event{Kind: KToolArgs, Text: piece})
	}
	enc.event(Event{Kind: KStop})
	args := got.String()
	if args != `{"a": [1, 2]}` {
		t.Fatalf("the arguments are %q", args)
	}
}

// textEncoder is a stream encoder that writes the text of the events it
// is given, one after the other, for the tests of the encoders.
type textEncoder struct {
	w *strings.Builder
}

func (e *textEncoder) event(ev Event) {
	if ev.Kind == KToolArgs {
		e.w.WriteString(ev.Text)
	}
}

func (e *textEncoder) finish()    {}
func (e *textEncoder) keepalive() {}

// TestBracketTidyChatStream says a Chat reply relayed as it came reaches
// the client with the arguments of its call whole.
func TestBracketTidyChatStream(t *testing.T) {
	stream := sse(
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"m","arguments":"{\"a\": [1, 2}"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"]}"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	tidy := &bracketTidy{proto: provider.Chat, sse: true}
	got := string(tidy.write([]byte(stream))) + string(tidy.flush())
	args := chatArgsOf(t, got)
	if args != `{"a": [1, 2]}` {
		t.Fatalf("the arguments are %q", args)
	}
}

// TestBracketTidyChatWhole says a whole Chat reply relayed as it came
// reaches the client with the arguments of its call whole.
func TestBracketTidyChatWhole(t *testing.T) {
	whole := `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"m","arguments":"{\"a\": [1, 2}"}}]},"finish_reason":"tool_calls"}]}`
	tidy := &bracketTidy{proto: provider.Chat}
	got := string(tidy.write([]byte(whole))) + string(tidy.flush())
	args := chatArgsOf(t, got)
	if args != `{"a": [1, 2]}` {
		t.Fatalf("the arguments are %q", args)
	}
}

// TestBracketTidyResponsesStream says a Responses reply relayed as it came
// reaches the client with the arguments of its call whole.
func TestBracketTidyResponsesStream(t *testing.T) {
	stream := sse(
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"m","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"a\": [1, 2}"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"a\": [1, 2}"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"m","arguments":"{\"a\": [1, 2}"}}`,
	)
	tidy := &bracketTidy{proto: provider.Responses, sse: true}
	got := string(tidy.write([]byte(stream))) + string(tidy.flush())
	found := false
	for _, ev := range events(got) {
		if ev["type"] == "response.function_call_arguments.done" {
			found = true
			if args, _ := ev["arguments"].(string); args != `{"a": [1, 2]}` {
				t.Fatalf("the arguments are %q", args)
			}
		}
	}
	if !found {
		t.Fatalf("the call's arguments were not said:\n%s", got)
	}
}

// TestBracketTidyAnthropicStream says a Messages reply relayed as it came
// reaches the client with the arguments of its call whole.
func TestBracketTidyAnthropicStream(t *testing.T) {
	stream := sse(
		`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"m","input":{}}}`,
		`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\": [1, 2}"}}`,
		`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"]}"}}`,
		`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
	)
	tidy := &bracketTidy{proto: provider.Anthropic, sse: true}
	got := string(tidy.write([]byte(stream))) + string(tidy.flush())
	t.Logf("GOT:\n%s", got)
	partial := ""
	for _, ev := range events(got) {
		if ev["type"] == "content_block_delta" {
			d, _ := ev["delta"].(map[string]any)
			if d != nil && d["type"] == "input_json_delta" {
				s, _ := d["partial_json"].(string)
				partial += s
			}
		}
	}
	if partial != `{"a": [1, 2]}` {
		t.Fatalf("the arguments are %q", partial)
	}
}

// TestBracketTidySaysWholeAsItCame says a reply whose calls' arguments are
// whole JSON reaches the client as it came, byte for byte.
func TestBracketTidySaysWholeAsItCame(t *testing.T) {
	stream := sse(
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"m","arguments":"{\"a\": [1, 2], \"b\": \"}\"}"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	tidy := &bracketTidy{proto: provider.Chat, sse: true}
	got := string(tidy.write([]byte(stream))) + string(tidy.flush())
	if got != stream {
		t.Fatalf("a whole healthy stream changed:\n%s\nwant\n%s", got, stream)
	}
}

// chatArgsOf reads what a Chat client was given of a tool call's input:
// its arguments pieces (streamed) or its whole arguments.
func chatArgsOf(t *testing.T, body string) string {
	t.Helper()
	var out strings.Builder
	add := func(calls []any) {
		for _, tc := range calls {
			tm, _ := tc.(map[string]any)
			fn, _ := tm["function"].(map[string]any)
			if fn == nil {
				continue
			}
			s, _ := fn["arguments"].(string)
			out.WriteString(s)
		}
	}
	// a whole reply
	var whole map[string]any
	if json.Unmarshal([]byte(body), &whole) == nil {
		choices, _ := whole["choices"].([]any)
		for _, c := range choices {
			ch, _ := c.(map[string]any)
			if ch == nil {
				continue
			}
			if m, _ := ch["message"].(map[string]any); m != nil {
				calls, _ := m["tool_calls"].([]any)
				add(calls)
			}
		}
		return out.String()
	}
	// a stream
	for _, ev := range events(body) {
		choices, _ := ev["choices"].([]any)
		for _, c := range choices {
			ch, _ := c.(map[string]any)
			if ch == nil {
				continue
			}
			if d, _ := ch["delta"].(map[string]any); d != nil {
				calls, _ := d["tool_calls"].([]any)
				add(calls)
			}
		}
	}
	return out.String()
}
