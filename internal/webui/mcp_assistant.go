package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"

	webv1 "capsulad/gen/go/capsulad/web/v1"
	"capsulad/internal/websecurity"
)

// These are positive allowlists, not the operator schema with today's unsafe
// fields removed. New options (including nested fields and oneof members)
// require an explicit review before they become available to an assistant.
// The same definitions produce the advertised schema and check raw arguments
// before protobuf decoding or dispatch. In particular, proto3 decoding alone
// cannot tell whether an operator-only scalar was explicitly sent as its zero
// value or null.
type mcpAssistantFields map[string]mcpAssistantField

type mcpAssistantField struct {
	fields   mcpAssistantFields
	enum     []string
	mask     []string // nonempty field mask; an empty display mask would write every field
	required bool
}

func (f mcpAssistantField) permits(fd protoreflect.FieldDescriptor) bool {
	switch {
	case fd == nil:
		return false
	case f.mask != nil:
		return fd.IsList() && fd.Kind() == protoreflect.StringKind
	case fd.IsList() || fd.IsMap():
		return false
	case f.fields != nil:
		return fd.Kind() == protoreflect.MessageKind
	case f.enum != nil:
		return fd.Kind() == protoreflect.EnumKind || fd.Kind() == protoreflect.StringKind
	default:
		// A leaf never opts in to an entire message, enum or opaque payload.
		return fd.Message() == nil && fd.Enum() == nil && fd.Kind() != protoreflect.BytesKind
	}
}

func (f mcpAssistantField) schema(fd protoreflect.FieldDescriptor, depth int) map[string]any {
	switch {
	case f.fields != nil:
		return mcpScopedMessageSchema(fd.Message(), depth+1, f.fields)
	case f.enum != nil:
		return map[string]any{"type": "string", "enum": f.enum}
	case f.mask != nil:
		return map[string]any{"type": "array", "minItems": 1,
			"items": map[string]any{"type": "string", "enum": f.mask}}
	case fd.Kind() == protoreflect.Uint64Kind:
		// ProtoJSON emits 64-bit integers as decimal strings and accepts either
		// representation, including when reusing a settings snapshot revision.
		return map[string]any{"anyOf": []map[string]any{
			{"type": "integer", "minimum": 0},
			{"type": "string", "pattern": "^[0-9]+$"},
		}}
	default:
		return mcpFieldSchema(fd, depth)
	}
}

func (fields mcpAssistantFields) check(raw json.RawMessage, descriptor protoreflect.MessageDescriptor) *mcpError {
	var args map[string]json.RawMessage
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return mcpInvalidParams(err)
		}
		if args == nil {
			return mcpInvalidParams(fmt.Errorf("arguments must be an object"))
		}
	}
	for name, value := range args {
		field, ok := fields[name]
		fd := descriptor.Fields().ByName(protoreflect.Name(name))
		if !ok || !field.permits(fd) {
			return &mcpError{Code: mcpCodeForbidden, Message: fmt.Sprintf("the assistant scope does not permit field %q", name)}
		}
		switch {
		case field.fields != nil:
			if err := field.fields.check(value, fd.Message()); err != nil {
				return err
			}
		case field.mask != nil:
			var mask []string
			if err := json.Unmarshal(value, &mask); err != nil {
				return mcpInvalidParams(err)
			}
			if len(mask) == 0 {
				return &mcpError{Code: mcpCodeForbidden, Message: "the assistant scope requires a nonempty display update_fields mask"}
			}
			for _, entry := range mask {
				if !slices.Contains(field.mask, entry) {
					return &mcpError{Code: mcpCodeForbidden, Message: fmt.Sprintf("the assistant scope does not permit updating display field %q", entry)}
				}
			}
			for _, entry := range mask {
				if _, ok := args[entry]; !ok {
					return mcpInvalidParams(fmt.Errorf("display update_fields requires a value for %q", entry))
				}
			}
		case field.enum != nil:
			var valueName string
			if err := json.Unmarshal(value, &valueName); err != nil {
				return mcpInvalidParams(err)
			}
			if !slices.Contains(field.enum, valueName) {
				return &mcpError{Code: mcpCodeForbidden, Message: fmt.Sprintf("the assistant scope does not permit this value for %q", name)}
			}
		case bytes.Equal(value, []byte("null")):
			return mcpInvalidParams(fmt.Errorf("field %q cannot be null", name))
		case fd.Kind() == protoreflect.FloatKind:
			// protojson also accepts string forms of nonfinite values. A user
			// setting is a JSON number, not a way to write NaN or infinity.
			var number float32
			if err := json.Unmarshal(value, &number); err != nil {
				return mcpInvalidParams(err)
			}
		}
	}
	for name, field := range fields {
		if _, ok := args[name]; ok || !field.required {
			continue
		}
		if field.mask != nil {
			return &mcpError{Code: mcpCodeForbidden, Message: "the assistant scope requires a nonempty display update_fields mask"}
		}
		return mcpInvalidParams(fmt.Errorf("field %q is required", name))
	}
	return nil
}

func assistantCommandTool(tool mcpTool, fields mcpAssistantFields) mcpTool {
	variant := tool
	variant.Scope = websecurity.ScopeAssistant
	variant.fields = fields
	variant.Schema = mcpScopedMessageSchema(tool.args.ProtoReflect().Descriptor(), 0, fields)
	tool.assistant = &variant
	return tool
}

func assistantSettingsTool(tool mcpTool) mcpTool {
	tool = assistantCommandTool(tool, mcpAssistantFields{
		"expected_revision": {required: true},
		"display": {fields: mcpAssistantFields{
			"resource_id": {required: true},
			"brightness": {},
			"brightness_mode": {enum: []string{
				webv1.BrightnessMode_BRIGHTNESS_MODE_CONST.String(),
				webv1.BrightnessMode_BRIGHTNESS_MODE_AUTO.String(),
			}},
			"update_fields": {required: true, mask: []string{"brightness", "brightness_mode"}},
		}},
		"volume":        {fields: mcpAssistantFields{"master_volume": {required: true}}},
		"startup_sound": {fields: mcpAssistantFields{"enabled": {required: true}}},
		"timezone":      {fields: mcpAssistantFields{"timezone": {required: true}}},
	})
	tool.assistant.Description = "Patch one speaker settings group: master volume, display brightness or brightness mode, startup sound or timezone. " +
		"Reuse expected_revision from get_state topic settings as a decimal string or integer. A display patch must name a resource_id, " +
		"list brightness and/or brightness_mode in update_fields, and provide a value for each listed field."
	return tool
}

func assistantLaunchTool(tool mcpTool) mcpTool {
	tool = assistantCommandTool(tool, mcpAssistantFields{"app_id": {required: true}})
	tool.assistant.Description = "Explicitly launch an installed application by app_id, without launch arguments."
	return tool
}

func assistantStopTool(tool mcpTool) mcpTool {
	return assistantCommandTool(tool, mcpAssistantFields{"app_id": {required: true}, "run_id": {required: true}})
}

func assistantMediaTool(tool mcpTool) mcpTool {
	return assistantCommandTool(tool, mcpAssistantFields{
		"app_id": {}, "player_id": {required: true},
		"action": {required: true, enum: []string{
			webv1.MediaAction_MEDIA_ACTION_PLAY.String(), webv1.MediaAction_MEDIA_ACTION_PAUSE.String(),
			webv1.MediaAction_MEDIA_ACTION_NEXT.String(), webv1.MediaAction_MEDIA_ACTION_PREVIOUS.String(),
		}},
		"kind": {enum: []string{
			webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_APP.String(),
			webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA.String(),
		}},
	})
}

func assistantClockTool(tool mcpTool) mcpTool {
	return assistantCommandTool(tool, mcpAssistantFields{
		"app_id":           {required: true, enum: []string{clockAppID}},
		"stopwatch_start":  {fields: mcpAssistantFields{}},
		"stopwatch_pause":  {fields: mcpAssistantFields{}},
		"stopwatch_resume": {fields: mcpAssistantFields{}},
		"stopwatch_reset":  {fields: mcpAssistantFields{}},
		"start_timer":      {fields: mcpAssistantFields{"duration_ms": {required: true}, "label": {}}},
		"cancel_timer":     {fields: mcpAssistantFields{"timer_id": {required: true}}},
		"set_alarm": {fields: mcpAssistantFields{
			"alarm_id": {}, "label": {}, "hour": {}, "minute": {}, "days_of_week": {}, "timezone": {}, "enabled": {},
		}},
		"set_alarm_enabled": {fields: mcpAssistantFields{"alarm_id": {required: true}, "enabled": {required: true}}},
		"delete_alarm":      {fields: mcpAssistantFields{"alarm_id": {required: true}}},
		"dismiss_alert":     {fields: mcpAssistantFields{"alert_id": {required: true}}},
	})
}

var mcpTopics = []struct {
	name      string
	topic     webv1.Topic
	assistant bool
}{
	{"overview", webv1.Topic_TOPIC_OVERVIEW, true},
	{"settings", webv1.Topic_TOPIC_SETTINGS, true},
	{"application", webv1.Topic_TOPIC_APPLICATION, true},
	{"diagnostics", webv1.Topic_TOPIC_DIAGNOSTICS, false},
	{"wakeword", webv1.Topic_TOPIC_WAKEWORD, false},
	{"access", webv1.Topic_TOPIC_ACCESS, false},
	{"wifi", webv1.Topic_TOPIC_WIFI, false},
}

func mcpStateTool() mcpTool {
	schema := func(assistant bool) map[string]any {
		var names []string
		for _, topic := range mcpTopics {
			if !assistant || topic.assistant {
				names = append(names, topic.name)
			}
		}
		return objectSchema(map[string]any{
			"topic":  strEnumProp("which snapshot to read", names...),
			"app_id": strProp("application id, required when topic is application"),
		}, "topic")
	}
	tool := readTool("get_state", websecurity.ScopeOperator,
		"Read one live snapshot the Web UI subscribes to. Topics: overview (device, audio, apps, runs, "+
			"and the Now Playing media projection: only players that report PLAYBACK_PLAYING, with "+
			"idle/paused/stopped providers omitted), settings, application (needs app_id), diagnostics "+
			"(logs and metrics), wakeword, access (the listener/credential status and the API-key list "+
			"with its scopes) and wifi. Use this to observe state that a mutation streams to the Web UI, "+
			"for example wakeword training progress.",
		schema(false), mcpStateHandler(false))
	assistantSchema := schema(true)
	assistantSchema["if"] = map[string]any{"properties": map[string]any{"topic": map[string]any{"const": "application"}}}
	assistantSchema["then"] = map[string]any{"required": []string{"app_id"}}
	variant := readTool(tool.Name, websecurity.ScopeAssistant,
		"Read limited speaker state. overview lists installed apps, runs, volume and playing media; "+
			"settings shows the speaker settings and their revision; application (requires app_id) shows app identity, "+
			"runs and controls state for the built-in clock or ring.",
		assistantSchema, mcpStateHandler(true))
	tool.assistant = &variant
	return tool
}

func mcpStateHandler(assistant bool) func(context.Context, *Server, *mcpSession, json.RawMessage) (*mcpToolResult, *mcpError) {
	return func(ctx context.Context, s *Server, _ *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
		var in struct {
			Topic string `json:"topic"`
			AppID string `json:"app_id"`
		}
		if err := strictJSON(args, &in); err != nil {
			return nil, mcpInvalidParams(err)
		}
		name, appID := in.Topic, strings.TrimSpace(in.AppID)
		if !assistant {
			name = strings.TrimSpace(name)
		}
		for _, topic := range mcpTopics {
			if topic.name != name {
				continue
			}
			if assistant && !topic.assistant {
				return nil, &mcpError{Code: mcpCodeForbidden, Message: fmt.Sprintf("the assistant scope does not permit topic %q", name)}
			}
			return mcpReadState(ctx, s, topic.topic, appID, assistant)
		}
		return nil, mcpInvalidParams(fmt.Errorf("unknown topic %q", in.Topic))
	}
}

func mcpReadState(ctx context.Context, s *Server, topic webv1.Topic, appID string, assistant bool) (*mcpToolResult, *mcpError) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	read := s.snapshot
	if assistant {
		read = s.assistantSnapshot
	}
	snap, err := read(ctx, topic, appID)
	if err != nil {
		return nil, mcpRPCCallError(err)
	}
	return mcpProtoResult(snap)
}
