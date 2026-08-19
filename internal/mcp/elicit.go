package mcp

// Elicitation: a server asking the user a question, answered through the same
// surface the ask tool asks through.
//
// The role is granted, never assumed. Sampling and roots stay declined because
// each hands the server something the user never offered — a sampling request
// spends the user's model budget, a roots request describes the filesystem —
// but a question is neither. It is the posture ask already states: a question
// is interaction, not an effect, and the answer channel is a person who can
// refuse in person. What keeps an unattended surface from being asked is the
// absent questioner, exactly as it is for the tool: headless runs, delegate
// subagents, and race branches never set one, the capability is therefore not
// declared at initialize, and a server that asks anyway is declined the way it
// was before this file existed.
//
// Two things travel in from a server that is not trusted with either. The
// message is text on the user's screen, so the dialog names the server that
// wrote it and the text is capped; a question that looked like Switchboard's
// own would be the whole attack. And the answer travels outward to an
// unconfined process, which is a stronger reason to redact than the tool has:
// it passes credential.ScanPrompt and redacts unconditionally, the same
// posture, applied where the consequence is larger.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/tools"
)

const (
	// maxElicitFields bounds how many dialogs one request can open. A server
	// that wants a form can ask again; a server that wants the screen cannot
	// have it.
	maxElicitFields = 4

	// maxElicitMessage caps the server's own prose. Past this the dialog stops
	// being a question and starts being a page.
	maxElicitMessage = 500

	// maxElicitOptions matches what the ask dialog reads comfortably. An enum
	// longer than this is refused rather than scrolled.
	maxElicitOptions = 12
)

// elicitAction is the protocol's three answers. Declining and cancelling are
// different facts and the server is entitled to both: the user waving the
// question away is a decision, the turn ending underneath it is not.
const (
	elicitAccept  = "accept"
	elicitDecline = "decline"
	elicitCancel  = "cancel"
)

type elicitRequest struct {
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
}

type elicitResult struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
}

// elicitField is the subset of JSON Schema this client answers. Anything a
// server declares beyond it blocks the request rather than being ignored,
// because a control that changes what the answer means cannot be dropped and
// still called an answer.
type elicitField struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Enum        []string `json:"enum"`
	EnumNames   []string `json:"enumNames"`
}

// unsupportedElicit is a schema this client will not answer. It is separate
// from a transport or protocol failure: the request arrived intact and was
// refused on its contents, so the server is told which part.
type unsupportedElicit struct{ reason string }

func (e *unsupportedElicit) Error() string { return e.reason }

// answerElicitation resolves one elicitation/create against the user.
//
// A returned error is a schema this client does not serve. Everything else,
// including the user saying no, is a result: the protocol has words for
// declining and cancelling, and reporting either as an error would make a
// server retry something a person already answered.
func (c *Client) answerElicitation(ctx context.Context, params json.RawMessage) (elicitResult, error) {
	questioner := c.questioner
	if questioner == nil {
		return elicitResult{}, &unsupportedElicit{"no user is attached to this session"}
	}

	var req elicitRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return elicitResult{}, &unsupportedElicit{"malformed elicitation/create params"}
	}

	names, err := propertyOrder(req.RequestedSchema)
	if err != nil {
		return elicitResult{}, err
	}
	if len(names) == 0 {
		return elicitResult{}, &unsupportedElicit{"requestedSchema declares no properties"}
	}
	if len(names) > maxElicitFields {
		return elicitResult{}, &unsupportedElicit{fmt.Sprintf(
			"requestedSchema declares %d properties; this client asks at most %d per request",
			len(names), maxElicitFields)}
	}

	var schema struct {
		Properties map[string]elicitField `json:"properties"`
	}
	if err := json.Unmarshal(req.RequestedSchema, &schema); err != nil {
		return elicitResult{}, &unsupportedElicit{"requestedSchema is not an object schema"}
	}

	content := make(map[string]any, len(names))
	for _, name := range names {
		field := schema.Properties[name]
		question, err := elicitQuestion(c.spec.Name, req.Message, name, field)
		if err != nil {
			return elicitResult{}, err
		}

		answer, err := questioner.AskUser(ctx, question)
		if err != nil {
			// The turn ended or the program quit underneath the dialog. That
			// is not the user declining, and the protocol distinguishes them.
			return elicitResult{Action: elicitCancel}, nil
		}

		value, ok := elicitValue(field, answer)
		if !ok {
			if answer.Text != "" {
				// The one place an unusable answer is not a lie to report as a
				// decline: the user answered, the answer does not fit the type
				// the server asked for, and nobody but the user can fix that.
				c.logf("warn", fmt.Sprintf("mcp %s asked for a %s in %q and the answer was not one; declining",
					c.spec.Name, field.Type, name))
			}
			return elicitResult{Action: elicitDecline}, nil
		}
		content[name] = value
	}
	return elicitResult{Action: elicitAccept, Content: content}, nil
}

// elicitQuestion renders one field as a question the ask surface can show.
//
// The server's name leads, because the user is being asked by something that
// is not Switchboard and not the model, and no other part of the dialog says
// so.
func elicitQuestion(server, message, name string, field elicitField) (tools.Question, error) {
	label := field.Title
	if label == "" {
		label = name
	}

	var b strings.Builder
	fmt.Fprintf(&b, "MCP server %s asks: ", server)
	if message != "" {
		b.WriteString(truncateElicit(message))
		b.WriteString("\n\n")
	}
	b.WriteString(label)
	if field.Description != "" {
		b.WriteString(" — ")
		b.WriteString(truncateElicit(field.Description))
	}
	question := tools.Question{Question: b.String()}

	switch field.Type {
	case "string":
		if len(field.Enum) == 0 {
			// No options is the free-text dialog: the type-your-own row and
			// esc, which is exactly what an unconstrained string wants.
			return question, nil
		}
		if len(field.Enum) > maxElicitOptions {
			return tools.Question{}, &unsupportedElicit{fmt.Sprintf(
				"property %q offers %d enum values; this client shows at most %d",
				name, len(field.Enum), maxElicitOptions)}
		}
		for i, value := range field.Enum {
			option := tools.QuestionOption{Label: value}
			if i < len(field.EnumNames) && field.EnumNames[i] != "" {
				// enumNames is the display text and enum is the wire value.
				// Showing the value and answering with it keeps the two from
				// drifting; the name rides as the detail so the user reads
				// what the server meant.
				option.Detail = field.EnumNames[i]
			}
			question.Options = append(question.Options, option)
		}
		return question, nil

	case "boolean":
		question.Options = []tools.QuestionOption{{Label: "yes"}, {Label: "no"}}
		return question, nil

	case "number", "integer":
		return question, nil

	default:
		// An empty type included: a property with no declared type is not a
		// string by default here, because guessing one would put an arbitrary
		// value into a field the server will act on.
		return tools.Question{}, &unsupportedElicit{fmt.Sprintf(
			"property %q has type %q, which this client does not ask for", name, field.Type)}
	}
}

// elicitValue maps what the user did onto the type the server declared. The
// false return is "no usable answer", which covers declining and covers an
// answer that does not fit; the caller separates them for the log, not for the
// server, because the server gets the same word either way.
func elicitValue(field elicitField, answer tools.Answer) (any, bool) {
	if answer.Declined {
		return nil, false
	}

	text := answer.Text
	if len(answer.Picked) > 0 {
		text = answer.Picked[0]
	}
	if text == "" {
		return nil, false
	}

	// Outbound to a process this program does not confine. The tool redacts a
	// typed answer for a weaker reason than this one.
	if leaks := credential.ScanPrompt(text); len(leaks) > 0 {
		text = credential.Redact(text, leaks)
	}

	switch field.Type {
	case "boolean":
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "yes", "true", "y":
			return true, true
		case "no", "false", "n":
			return false, true
		default:
			return nil, false
		}
	case "integer":
		n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return nil, false
		}
		return n, true
	case "number":
		n, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return nil, false
		}
		return n, true
	default:
		if len(field.Enum) > 0 && !containsString(field.Enum, text) {
			// A typed answer past a closed set is not one of the values the
			// server said it accepts, so sending it would break the contract
			// the enum is.
			return nil, false
		}
		return text, true
	}
}

// propertyOrder reads the property names in the order the server wrote them.
//
// Decoding into a map would hand back Go's randomized iteration order, so the
// same schema would ask its questions in a different order on each run. The
// order the document carries is the only ordering evidence there is, and a
// form whose fields shuffle between runs reads as a broken program.
func propertyOrder(raw json.RawMessage) ([]string, error) {
	var envelope struct {
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Properties) == 0 {
		return nil, &unsupportedElicit{"requestedSchema has no properties object"}
	}

	dec := json.NewDecoder(bytes.NewReader(envelope.Properties))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, &unsupportedElicit{"requestedSchema properties is not an object"}
	}
	var names []string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, &unsupportedElicit{"requestedSchema properties is malformed"}
		}
		name, ok := key.(string)
		if !ok {
			return nil, &unsupportedElicit{"requestedSchema properties is malformed"}
		}
		names = append(names, name)
		// Skip the value whole; only the key order is wanted here.
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			return nil, &unsupportedElicit{"requestedSchema properties is malformed"}
		}
	}
	return names, nil
}

func truncateElicit(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxElicitMessage {
		return text
	}
	return text[:maxElicitMessage] + "…"
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
