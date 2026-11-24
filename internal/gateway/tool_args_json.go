package gateway

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/provider"
)

// A model sometimes closes a tool call's arguments with the wrong bracket:
// the array it closes with '}', the object with ']', or with one bracket
// too many. The client then cannot parse the arguments, the call never
// runs, and the turn stops without a word (yetone/magpie#7678). The cure
// is here, at the gateway, which sees the text the model said before the
// client does: the brackets tell which closer was meant, so a closer that
// does not match the bracket it closes is made to match it, one that
// closes nothing is left out, and what the text leaves open is closed at
// the end of the call.

// jsonBrackets follows a JSON text, a piece at a time, for the brackets it
// leaves open. A bracket inside a string is a character, not structure.
type jsonBrackets struct {
	stack []byte // the brackets left open, '{' or '['
	str   bool   // the text is inside a string
	esc   bool   // the text is inside a string, after a backslash
}

// feed reads the next piece of the text and gives back what to say of it:
// the piece as it came, with a closer that does not match the bracket it
// closes made to match it, and a closer that closes nothing left out.
//
// A closer the text has no bracket for closes nothing: the bracket it was
// meant to close has been closed already, so saying it would break the
// text further.
//
// The fix is made as the text comes, since the brackets tell which closer
// was meant without looking ahead: what the text leaves open is closed by
// close, at the end of the call.
func (b *jsonBrackets) feed(s string) string {
	if s == "" {
		return ""
	}
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case b.esc:
			b.esc = false
		case b.str:
			switch c {
			case '\\':
				b.esc = true
			case '"':
				b.str = false
			}
		case c == '"':
			b.str = true
		case c == '{' || c == '[':
			b.stack = append(b.stack, c)
		case c == '}' || c == ']':
			if len(b.stack) == 0 {
				continue // a closer that closes nothing
			}
			want := byte('}')
			if b.stack[len(b.stack)-1] == '[' {
				want = ']'
			}
			if c != want {
				c = want // the bracket says which closer was meant
			}
			b.stack = b.stack[:len(b.stack)-1]
		}
		out.WriteByte(c)
	}
	return out.String()
}

// close gives back the text that closes what the text left open: the
// string it is in, if it is in one, and the brackets it leaves open, from
// the innermost out. A text that leaves nothing open closes with nothing.
func (b *jsonBrackets) close() string {
	var out strings.Builder
	if b.str {
		out.WriteByte('"')
		b.str = false
	}
	for i := len(b.stack) - 1; i >= 0; i-- {
		if b.stack[i] == '[' {
			out.WriteByte(']')
		} else {
			out.WriteByte('}')
		}
	}
	b.stack = nil
	return out.String()
}

// repairArgs is a call's arguments with a bracket slip fixed: the text as
// it came, with a closer that does not match the bracket it closes made to
// match it, a closer that closes nothing left out, and what the text
// leaves open closed at the end.
//
// Text that is already whole JSON is said as it came, and text the fix
// cannot make whole is said as it came too: a fix that breaks the text
// further is worse than no fix at all.
func repairArgs(s string) string {
	if json.Valid([]byte(s)) {
		return s
	}
	var b jsonBrackets
	out := b.feed(s) + b.close()
	if json.Valid([]byte(out)) {
		return out
	}
	return s
}

// jsonRepairEncoder hands its encoder a call's arguments with a bracket
// slip fixed, streamed in pieces, so that a client never sees arguments it
// cannot parse.
//
// What the arguments leave open is closed when the call ends: the next
// call, or the end of the reply.
type jsonRepairEncoder struct {
	streamEncoder
	args jsonBrackets
}

// event gives the encoder the event, with the arguments of the call it
// carries fixed.
func (e *jsonRepairEncoder) event(ev Event) {
	switch ev.Kind {
	case KToolArgs:
		ev.Text = e.args.feed(ev.Text)
		if ev.Text == "" {
			return // the piece was a closer that closes nothing
		}
	case KToolStart, KText, KThink, KSig, KSealed, KTextSig, KSearch, KImage, KStop:
		// the call is over: close what its arguments left open
		if tail := e.args.close(); tail != "" {
			e.streamEncoder.event(Event{Kind: KToolArgs, Text: tail})
		}
	}
	e.streamEncoder.event(ev)
}

// jsonRepairResult is a whole reply with the arguments of its calls fixed.
func jsonRepairResult(res Result) Result {
	parts := make([]Part, len(res.Parts))
	copy(parts, res.Parts)
	for i := range parts {
		if parts[i].Kind != ToolCall {
			continue
		}
		args := string(parts[i].Args)
		if out := repairArgs(args); out != args {
			parts[i].Args = json.RawMessage(out)
		}
	}
	res.Parts = parts
	return res
}

// bracketTidy fixes, in a reply relayed as it came, the brackets of its
// calls' arguments: Responses' function_call items and argument deltas,
// Chat's tool_calls, and Messages' tool_use input.
//
// The arguments of a call come in pieces, and the fix is made as they come,
// since the brackets say which closer was meant without looking ahead: what
// the pieces leave open is closed when the call ends, or, in a reply that
// says a call's arguments whole, in that text.
//
// A stream's lines pass as they came unless one carries arguments that
// change; a reply that isn't a stream is held whole and gone through at the
// end.
type bracketTidy struct {
	proto provider.Protocol
	sse   bool
	buf   []byte
	calls map[string]*jsonBrackets // a streamed call's brackets so far, by its index or item id
}

func (t *bracketTidy) write(b []byte) []byte {
	t.buf = append(t.buf, b...)
	if !t.sse {
		return nil
	}
	i := bytes.LastIndexByte(t.buf, '\n')
	if i < 0 {
		return nil
	}
	out := t.lines(t.buf[:i+1])
	t.buf = append(t.buf[:0], t.buf[i+1:]...)
	return out
}

func (t *bracketTidy) flush() []byte {
	b := t.buf
	t.buf, t.calls = nil, nil
	if !t.sse {
		// a whole reply: its calls' arguments are said whole, so they are
		// fixed whole
		if !t.carries(b) {
			return b
		}
		if out, ok := t.event(b); ok {
			return out
		}
		return b
	}
	return t.lines(b)
}

// carries is whether b may carry a call's arguments.
func (t *bracketTidy) carries(b []byte) bool {
	switch t.proto {
	case provider.Responses:
		return bytes.Contains(b, []byte(`function_call`))
	case provider.Chat:
		return bytes.Contains(b, []byte(`"tool_calls"`))
	case provider.Anthropic:
		return bytes.Contains(b, []byte(`"tool_use"`)) || bytes.Contains(b, []byte(`"input_json_delta"`))
	}
	return false
}

func (t *bracketTidy) lines(b []byte) []byte {
	if !t.carries(b) {
		return append([]byte(nil), b...)
	}
	var out []byte
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i+1], b[i+1:]
		} else {
			b = nil
		}
		body := bytes.TrimRight(line, "\r\n")
		data, ok := bytes.CutPrefix(body, []byte("data:"))
		if !ok || !t.carries(data) {
			out = append(out, line...)
			continue
		}
		nb, ok := t.event(data)
		if !ok {
			out = append(out, line...)
			continue
		}
		out = append(append(append(out, "data: "...), nb...), line[len(body):]...)
	}
	return out
}

// event is an event, or a whole reply, with the brackets of its calls'
// arguments fixed, and whether that changed it.
func (t *bracketTidy) event(data []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // the event's own numbers as they came
	var ev map[string]any
	if dec.Decode(&ev) != nil || ev == nil {
		return nil, false
	}
	var changed bool
	switch t.proto {
	case provider.Responses:
		changed = t.responses(ev)
	case provider.Chat:
		changed = t.chat(ev)
	case provider.Anthropic:
		changed = t.anthropic(ev)
	}
	if !changed {
		return nil, false
	}
	nb, err := marshalPlain(ev)
	if err != nil {
		return nil, false
	}
	return nb, true
}

// stream is the brackets of the arguments so far of the streamed call at
// key.
func (t *bracketTidy) stream(key string) *jsonBrackets {
	if t.calls == nil {
		t.calls = map[string]*jsonBrackets{}
	}
	b := t.calls[key]
	if b == nil {
		b = &jsonBrackets{}
		t.calls[key] = b
	}
	return b
}

// close is what the arguments of the streamed call at key leave open, said
// as the call's last piece of arguments.
func (t *bracketTidy) close(key string) string {
	b := t.calls[key]
	if b == nil {
		return ""
	}
	delete(t.calls, key)
	return b.close()
}

func (t *bracketTidy) responses(ev map[string]any) bool {
	changed := false
	item := func(it map[string]any) {
		if it != nil && it["type"] == "function_call" {
			if s, ok := it["arguments"].(string); ok {
				if out := repairArgs(s); out != s {
					it["arguments"] = out
					changed = true
				}
			}
		}
	}
	it, _ := ev["item"].(map[string]any)
	item(it)
	switch ev["type"] {
	case "response.function_call_arguments.delta":
		key := callKeyOf(ev)
		s, _ := ev["delta"].(string)
		if out := t.stream(key).feed(s); out != s {
			ev["delta"] = out
			changed = true
		}
	case "response.function_call_arguments.done":
		// the call's arguments are said whole here: what the deltas leave
		// open is closed in this text
		key := callKeyOf(ev)
		s, _ := ev["arguments"].(string)
		out := repairArgs(s)
		if tail := t.close(key); tail != "" && out == s {
			out += tail
		}
		if out != s {
			ev["arguments"] = out
			changed = true
		}
	}
	res, _ := ev["response"].(map[string]any)
	if res == nil && ev["object"] == "response" {
		res = ev // a whole reply
	}
	if res != nil {
		out, _ := res["output"].([]any)
		for _, o := range out {
			im, _ := o.(map[string]any)
			item(im)
		}
	}
	return changed
}

func (t *bracketTidy) chat(ev map[string]any) bool {
	changed := false
	choices, _ := ev["choices"].([]any)
	for _, c := range choices {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		// a stream's delta, its arguments in pieces by the call's index
		if d, _ := cm["delta"].(map[string]any); d != nil {
			calls, _ := d["tool_calls"].([]any)
			for j, tc := range calls {
				tm, _ := tc.(map[string]any)
				fn, _ := tm["function"].(map[string]any)
				if fn == nil {
					continue
				}
				idx := jsonText(tm["index"])
				if tm["index"] == nil {
					idx = strconv.Itoa(j)
				}
				key := jsonText(cm["index"]) + "/" + idx
				s, _ := fn["arguments"].(string)
				if out := t.stream(key).feed(s); out != s {
					fn["arguments"] = out
					changed = true
				}
			}
		}
		// a whole reply's message
		if m, _ := cm["message"].(map[string]any); m != nil {
			calls, _ := m["tool_calls"].([]any)
			for _, tc := range calls {
				tm, _ := tc.(map[string]any)
				fn, _ := tm["function"].(map[string]any)
				if fn == nil {
					continue
				}
				s, _ := fn["arguments"].(string)
				if out := repairArgs(s); out != s {
					fn["arguments"] = out
					changed = true
				}
			}
		}
	}
	return changed
}

func (t *bracketTidy) anthropic(ev map[string]any) bool {
	changed := false
	// a whole reply's tool_use blocks, their input an object
	block := func(b map[string]any) {
		if b == nil || b["type"] != "tool_use" || b["input"] == nil {
			return
		}
		in, err := marshalPlain(b["input"])
		if err != nil {
			return
		}
		out := repairArgs(string(in))
		if out != string(in) {
			b["input"] = json.RawMessage(out)
			changed = true
		}
	}
	content, _ := ev["content"].([]any)
	for _, c := range content {
		cm, _ := c.(map[string]any)
		block(cm)
	}
	cb, _ := ev["content_block"].(map[string]any)
	block(cb)
	// a stream's input, in pieces by the block's index
	if ev["type"] == "content_block_delta" {
		if d, _ := ev["delta"].(map[string]any); d != nil && d["type"] == "input_json_delta" {
			key := jsonText(ev["index"])
			s, _ := d["partial_json"].(string)
			if out := t.stream(key).feed(s); out != s {
				d["partial_json"] = out
				changed = true
			}
		}
	}
	return changed
}
