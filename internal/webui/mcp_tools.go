// MCP tool catalogue.
//
// Every tool is one permit-checked entry in a fixed registry. A tool either
// dispatches one typed capsulad.web.v1.Command through the shared operation
// layer (so MCP and the browser WebSocket cannot drift apart) or performs one
// administrative file/read operation that the browser performs over its own
// HTTP routes. There is no caller-supplied service, RPC or raw payload: the
// only variable is the tool's argument object.
//
// The input schema of a command tool is generated from the protobuf descriptor
// itself, so the schema, the accepted field names and the daemon's validation
// stay in step with web.proto instead of being a third hand-maintained copy.
package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	webv1 "capsulad/gen/go/capsulad/web/v1"
	"capsulad/internal/assets"
	"capsulad/internal/uichanges"
	"capsulad/internal/websecurity"
)

// mcpTool is one advertised MCP tool.
type mcpTool struct {
	Name        string
	Description string
	// Scope is the grant for this variant; assistant is a restricted variant
	// of an operator tool, present only for explicitly delegated tools.
	Scope     string
	Schema    map[string]any
	assistant *mcpTool
	fields    mcpAssistantFields
	// args is a template protobuf message for the tool's argument object. When
	// set, the schema is generated from its descriptor, the call's arguments are
	// decoded into a fresh instance, and command() maps them onto the shared
	// webv1.Command vocabulary.
	args    proto.Message
	command func(proto.Message) *webv1.Command
	// handle serves a tool that is not a single command dispatch.
	handle func(ctx context.Context, s *Server, sess *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError)
}

func (t *mcpTool) permitted(scopes []string) *mcpTool {
	if websecurity.ScopesAllow(scopes, t.Scope) {
		return t
	}
	if t.assistant != nil && websecurity.ScopesAllow(scopes, t.assistant.Scope) {
		return t.assistant
	}
	return nil
}

func (t *mcpTool) serve(ctx context.Context, s *Server, sess *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	if t.handle != nil {
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		return t.handle(ctx, s, sess, args)
	}
	if t.fields != nil {
		if err := t.fields.check(args, t.args.ProtoReflect().Descriptor()); err != nil {
			return nil, err
		}
	}
	fresh := t.args.ProtoReflect().Type().New().Interface()
	if len(args) > 0 {
		if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(args, fresh); err != nil {
			return nil, mcpInvalidParams(err)
		}
	}
	return s.mcpDispatch(ctx, sess, t.command(fresh), t.Scope == websecurity.ScopeAssistant)
}

// mcpToolsList advertises only the tool variants callable with this key.
func (s *Server) mcpToolsList(auth mcpAuth) map[string]any {
	tools := make([]map[string]any, 0, len(mcpToolCatalogue))
	for i := range mcpToolCatalogue {
		if tool := mcpToolCatalogue[i].permitted(auth.scopes); tool != nil {
			tools = append(tools, map[string]any{
				"name": tool.Name, "description": tool.Description, "inputSchema": tool.Schema,
			})
		}
	}
	return map[string]any{"tools": tools}
}

// mcpToolsCall resolves, authorizes and runs one tool. requestID is the
// JSON-RPC id, which is the handle a notifications/cancelled message names.
func (s *Server) mcpToolsCall(ctx context.Context, auth mcpAuth, sess *mcpSession, params json.RawMessage, requestID string) (any, *mcpError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, mcpInvalidParams(err)
	}
	if strings.TrimSpace(call.Name) == "" {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: "a tool name is required"}
	}
	var tool *mcpTool
	for i := range mcpToolCatalogue {
		if mcpToolCatalogue[i].Name == call.Name {
			tool = &mcpToolCatalogue[i]
			break
		}
	}
	if tool == nil {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: fmt.Sprintf("unknown tool %q", call.Name)}
	}
	if tool = tool.permitted(auth.scopes); tool == nil {
		return nil, &mcpError{Code: mcpCodeForbidden, Message: fmt.Sprintf(
			"the API key does not permit tool %q", call.Name)}
	}
	// Record which API key requested the operation, not the secret or arguments.
	if s.Logs != nil {
		s.Logs.Infof("mcp", "", "tool %s called by API key %q", tool.Name, auth.info.Name)
	}
	// Register the call before it starts so a cancellation notification that
	// races the call is not lost.
	toolCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sess.trackRequest(requestID, cancel)
	defer sess.untrackRequest(requestID)
	result, rpcErr := tool.serve(toolCtx, s, sess, call.Arguments)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if result == nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "the tool returned no result"}
	}
	return result, nil
}

// commandTool describes a tool whose arguments are one webv1 command payload.
// required names the fields the daemon enforces, so the machine-readable schema
// agrees with the description instead of leaving mandatory fields to prose.
func commandTool(name, scope, description string, required []string, args proto.Message, command func(proto.Message) *webv1.Command) mcpTool {
	schema := mcpSchemaForMessage(args)
	if len(required) > 0 {
		schema["required"] = required
	}
	return mcpTool{
		Name: name, Description: description, Scope: scope,
		Schema: schema, args: args, command: command,
	}
}

// readTool describes a tool served by a dedicated handler.
func readTool(name, scope, description string, schema map[string]any, handle func(context.Context, *Server, *mcpSession, json.RawMessage) (*mcpToolResult, *mcpError)) mcpTool {
	return mcpTool{Name: name, Description: description, Scope: scope, Schema: schema, handle: handle}
}

// objectSchema builds an object schema with the given properties and required
// names. Like the descriptor-generated command schemas it forbids unknown
// properties, because the tool decoder (strictJSON) rejects them too.
func objectSchema(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func strProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func numProp(description string) map[string]any {
	return map[string]any{"type": "number", "description": description}
}

func strArrayProp(description string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": description}
}

func strEnumProp(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": description}
}

// --- schema generation from the protobuf descriptor ------------------------

// mcpSchemaDepth bounds nested message expansion so a recursive descriptor
// cannot produce an unbounded schema.
const mcpSchemaDepth = 8

func mcpSchemaForMessage(msg proto.Message) map[string]any {
	return mcpMessageSchema(msg.ProtoReflect().Descriptor(), 0)
}

func mcpMessageSchema(md protoreflect.MessageDescriptor, depth int) map[string]any {
	return mcpScopedMessageSchema(md, depth, nil)
}

func mcpScopedMessageSchema(md protoreflect.MessageDescriptor, depth int, allowed mcpAssistantFields) map[string]any {
	if md == nil || depth > mcpSchemaDepth {
		return map[string]any{"type": "object"}
	}
	props := map[string]any{}
	var required []string
	var groups []map[string]any
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		name := string(fd.Name())
		if allowed == nil {
			props[name] = mcpFieldSchema(fd, depth)
			continue
		}
		if field, ok := allowed[name]; ok && field.permits(fd) {
			props[name] = field.schema(fd, depth)
			if field.required {
				required = append(required, name)
			}
			for _, entry := range field.mask {
				groups = append(groups, map[string]any{
					"if": map[string]any{"properties": map[string]any{
						name: map[string]any{"contains": map[string]any{"const": entry}},
					}},
					"then": map[string]any{"required": []string{entry}},
				})
			}
		}
	}
	// Unknown field names are rejected by the server's decoder (protojson with
	// DiscardUnknown false), so the schema says so.
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	var prose []string
	oneofs := md.Oneofs()
	for i := range oneofs.Len() {
		oneof := oneofs.Get(i)
		if oneof.IsSynthetic() {
			continue
		}
		names := make([]string, 0, oneof.Fields().Len())
		alternatives := make([]map[string]any, 0, oneof.Fields().Len())
		for j := range oneof.Fields().Len() {
			name := string(oneof.Fields().Get(j).Name())
			if _, ok := props[name]; !ok {
				continue
			}
			names = append(names, name)
			alternatives = append(alternatives, map[string]any{"required": []string{name}})
		}
		if len(alternatives) == 0 {
			continue
		}
		// A proto3 oneof is either unset or exactly one member, and every oneof
		// in the command vocabulary is required, so each group is its own
		// exactly-one constraint. Keeping one allOf entry per group is what makes
		// a message with several oneofs correct: a flat oneOf would demand one
		// member across all groups at once.
		groups = append(groups, map[string]any{"oneOf": alternatives})
		prose = append(prose, strings.Join(names, " | "))
	}
	if len(prose) > 0 {
		schema["description"] = "exactly one of: " + strings.Join(prose, "; ")
	}
	if len(groups) > 0 {
		schema["allOf"] = groups
	}
	return schema
}

func mcpFieldSchema(fd protoreflect.FieldDescriptor, depth int) map[string]any {
	switch {
	case fd.IsMap():
		return map[string]any{"type": "object", "additionalProperties": mcpScalarSchema(fd.MapValue(), depth+1)}
	case fd.IsList():
		return map[string]any{"type": "array", "items": mcpScalarSchema(fd, depth+1)}
	default:
		return mcpScalarSchema(fd, depth+1)
	}
}

func mcpScalarSchema(fd protoreflect.FieldDescriptor, depth int) map[string]any {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return map[string]any{"type": "boolean"}
	case protoreflect.StringKind:
		return map[string]any{"type": "string"}
	case protoreflect.BytesKind:
		return map[string]any{"type": "string", "description": "base64-encoded bytes"}
	case protoreflect.EnumKind:
		values := fd.Enum().Values()
		names := make([]string, 0, values.Len())
		for i := range values.Len() {
			names = append(names, string(values.Get(i).Name()))
		}
		return map[string]any{"type": "string", "enum": names}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return mcpMessageSchema(fd.Message(), depth)
	case protoreflect.DoubleKind, protoreflect.FloatKind:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "integer"}
	}
}

// --- catalogue -------------------------------------------------------------

var (
	settingsUpdate = &webv1.SettingsUpdate{}
	launchApp      = &webv1.LaunchApp{}
	stopRun        = &webv1.StopRun{}
	triggerAction  = &webv1.TriggerAppAction{}
	appSettings    = &webv1.AppSettingsUpdate{}
	mediaCommand   = &webv1.MediaCommand{}
	bluetoothCmd   = &webv1.BluetoothCommand{}
	wifiCmd        = &webv1.WifiCommand{}
	recordSample   = &webv1.RecordSample{}
	deleteSample   = &webv1.DeleteSample{}
	deleteWord     = &webv1.DeleteWakeword{}
	falseDetect    = &webv1.FalseDetection{}
	cancelTraining = &webv1.CancelTraining{}
	clockCommand   = &webv1.ClockCommand{}
	revokeKey      = &webv1.RevokeApiKey{}
)

// mcpToolCatalogue is the closed tool set. Order is the order tools/list
// returns; a status-scoped key sees only the read-only prefix.
var mcpToolCatalogue = []mcpTool{
	readTool("get_status", websecurity.ScopeStatus,
		"Read the daemon's live status: device identity, subsystems, resources, runs and system state. "+
			"Equivalent to the Web UI's GET /api/status; available to any valid API key.",
		objectSchema(map[string]any{}), mcpGetStatus),
	readTool("get_certificate", websecurity.ScopeStatus,
		"Return the daemon's self-signed HTTPS certificate in PEM form, to trust the listener. "+
			"Equivalent to the Web UI's GET /api/cert.pem.",
		objectSchema(map[string]any{}), mcpGetCertificate),
	mcpStateTool(),
	readTool("list_assets", websecurity.ScopeOperator,
		"List the shared asset store: every regular file below <data-dir>/assets with its relative id, "+
			"sandbox path, media type, size and SHA-256.",
		objectSchema(map[string]any{"prefix": strProp("only list assets whose id starts with this prefix")}),
		mcpListAssets),
	readTool("list_api_keys", websecurity.ScopeOperator,
		"List the static API keys with their prefix, creation time, revocation state and scopes. "+
			"Secret values are never stored and never returned.",
		objectSchema(map[string]any{}), mcpListAPIKeys),

	assistantSettingsTool(commandTool("update_settings", websecurity.ScopeOperator,
		"Patch one settings group (display, assistant, microphone, muted_indicator, volume, startup_sound, "+
			"timezone or hostname). expected_revision is mandatory: read it from get_state topic settings and "+
			"the update is rejected if another client changed the store meanwhile.",
		[]string{"expected_revision"}, settingsUpdate, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_Settings{Settings: m.(*webv1.SettingsUpdate)}}
		})),
	assistantLaunchTool(commandTool("launch_app", websecurity.ScopeOperator,
		"Launch an installed application. Only launch arguments declared by the application's manifest "+
			"are accepted.",
		[]string{"app_id"}, launchApp, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_Launch{Launch: m.(*webv1.LaunchApp)}}
		})),
	assistantStopTool(commandTool("stop_run", websecurity.ScopeOperator,
		"Stop one run of one application. The run must belong to that application; stopping a selected "+
			"provider restarts it, and the result says so.",
		[]string{"app_id", "run_id"}, stopRun, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_Stop{Stop: m.(*webv1.StopRun)}}
		})),
	commandTool("trigger_action", websecurity.ScopeOperator,
		"Trigger one action declared by an installed application's manifest.",
		[]string{"app_id", "action_id"}, triggerAction, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_Action{Action: m.(*webv1.TriggerAppAction)}}
		}),
	commandTool("update_app_settings", websecurity.ScopeOperator,
		"Patch an installed application's typed settings. expected_revision is mandatory and each change "+
			"must name a setting the application declares.",
		[]string{"app_id", "expected_revision", "changes"}, appSettings, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_AppSettings{AppSettings: m.(*webv1.AppSettingsUpdate)}}
		}),
	assistantMediaTool(commandTool("media_command", websecurity.ScopeOperator,
		"Send PLAY, PAUSE, NEXT or PREVIOUS to a media provider. Set kind to "+
			"MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA for the daemon's own Bluetooth source "+
			"(then player_id must be \"bluetooth\"); otherwise app_id and player_id address an app provider.",
		[]string{"player_id", "action"}, mediaCommand, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_Media{Media: m.(*webv1.MediaCommand)}}
		})),
	commandTool("bluetooth_command", websecurity.ScopeOperator,
		"Administer the Bluetooth adapter: set_discoverable, set_incoming_connections, unpair (by the "+
			"opaque device_id from get_state, never a raw address) or set_adapter_name (administrator only).",
		nil, bluetoothCmd, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_Bluetooth{Bluetooth: m.(*webv1.BluetoothCommand)}}
		}),
	commandTool("wifi_command", websecurity.ScopeOperator,
		"Scan for networks, connect to one (passphrase is forwarded once and never stored or echoed) or "+
			"forget a saved network.",
		nil, wifiCmd, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_Wifi{Wifi: m.(*webv1.WifiCommand)}}
		}),
	commandTool("record_sample", websecurity.ScopeOperator,
		"Record one wakeword training sample from the microphone. label is word, speech, room or background; "+
			"the take may take up to 25 s and the microphone indication gate still applies.",
		[]string{"label"}, recordSample, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_RecordSample{RecordSample: m.(*webv1.RecordSample)}}
		}),
	commandTool("delete_sample", websecurity.ScopeOperator,
		"Delete one recorded training sample by sample_id.",
		[]string{"sample_id"}, deleteSample, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_DeleteSample{DeleteSample: m.(*webv1.DeleteSample)}}
		}),
	readTool("train_wakeword", websecurity.ScopeOperator,
		"Train a wakeword classifier from stored samples and install it atomically. Returns a training_id "+
			"immediately; with wait_seconds > 0 (clamped to 300 s) the call waits for the run to finish and "+
			"reports its terminal stage (installed/error). One training run may be active per MCP session. Progress is also "+
			"visible through get_state topic wakeword, and the run can be aborted with cancel_training.",
		objectSchema(map[string]any{
			"display_name":      strProp("the phrase being trained, e.g. \"capsula\""),
			"language":          strProp("language code, default ru"),
			"model_id":          strProp("retrain this installed model in place; empty creates a new word"),
			"seed":              map[string]any{"type": "integer", "description": "optional deterministic seed"},
			"word_sample_ids":   strArrayProp("sample ids labelled word"),
			"room_sample_ids":   strArrayProp("sample ids labelled room"),
			"speech_sample_ids": strArrayProp("sample ids labelled speech"),
			"false_sample_ids":  strArrayProp("sample ids labelled background"),
			"wait_seconds": map[string]any{
				"type": "number", "minimum": 0, "maximum": mcpMaxWaitSeconds,
				"description": "0 returns the training_id at once; >0 waits up to this long (clamped to 300 s) for the terminal stage",
			},
		}, "display_name"), mcpTrainWakeword),
	commandTool("cancel_training", websecurity.ScopeOperator,
		"Abort the active wakeword training run. The daemon reports whether the installed model was left "+
			"unchanged or the result is unknown.",
		[]string{"training_id"}, cancelTraining, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_CancelTraining{CancelTraining: m.(*webv1.CancelTraining)}}
		}),
	commandTool("delete_wakeword", websecurity.ScopeOperator,
		"Delete one installed wakeword model. Recordings are kept: they are shared training data.",
		[]string{"model_id"}, deleteWord, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_DeleteWakeword{DeleteWakeword: m.(*webv1.DeleteWakeword)}}
		}),
	commandTool("dismiss_detection", websecurity.ScopeOperator,
		"Dismiss one stored wakeword detection the operator judged spurious. Presentational only: no "+
			"training data is affected.",
		[]string{"detection_id"}, falseDetect, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_FalseDetection{FalseDetection: m.(*webv1.FalseDetection)}}
		}),
	assistantClockTool(commandTool("clock_command", websecurity.ScopeOperator,
		"Control the built-in clock: the stopwatch, timers, alarms and active alerts. app_id must be "+
			"capsulad.clock.",
		[]string{"app_id"}, clockCommand, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_Clock{Clock: m.(*webv1.ClockCommand)}}
		})),
	readTool("create_api_key", websecurity.ScopeOperator,
		"Create a static API key. The secret is returned exactly once and only its hash is stored. "+
			"scopes selects the grant: [\"status\"] (status and certificate, the default), "+
			"[\"assistant\"] (limited speaker controls) or [\"operator\"] (full administration). "+
			"Request assistant or operator explicitly; nothing is promoted implicitly.",
		objectSchema(map[string]any{
			"name":   strProp("key name, 1..64 characters of [A-Za-z0-9_.-]"),
			"scopes": strArrayProp("scopes to grant; default [\"status\"]"),
		}, "name"), mcpCreateAPIKey),
	commandTool("revoke_api_key", websecurity.ScopeOperator,
		"Revoke one API key by name. Revocation takes effect immediately for every frontend.",
		[]string{"name"}, revokeKey, func(m proto.Message) *webv1.Command {
			return &webv1.Command{Op: &webv1.Command_RevokeApiKey{RevokeApiKey: m.(*webv1.RevokeApiKey)}}
		}),
	readTool("install_app", websecurity.ScopeOperator,
		"Install or atomically replace one application package from one base64 body. The daemon refuses a "+
			"running application and a compiled-in system app. This one-call form carries the archive in a "+
			"single POST (bounded by the MCP request limit); for an archive larger than that use "+
			"begin_upload kind=app, which streams into the same sink with the full package limit.",
		objectSchema(map[string]any{
			"content_base64": strProp("the package archive, base64-encoded"),
			"filename":       strProp("archive name, diagnostics only; the daemon never installs from a client-named path"),
		}, "content_base64"), mcpInstallApp),
	readTool("remove_app", websecurity.ScopeOperator,
		"Remove one installed application package. Its settings are deliberately retained.",
		objectSchema(map[string]any{"app_id": strProp("the application to remove")}, "app_id"), mcpRemoveApp),
	readTool("upload_asset", websecurity.ScopeOperator,
		"Write one shared asset from one base64 body. path is the store-relative id. The store rejects "+
			"traversal, absolute paths, symlinks, reserved names and files over 32 MiB, and it replaces an "+
			"existing file atomically. This one-call form is bounded by the MCP request limit; for a larger "+
			"asset use begin_upload kind=asset, which streams into the same store with the full 32 MiB limit.",
		objectSchema(map[string]any{
			"path":           strProp("store-relative asset path, e.g. sounds/ding.wav"),
			"content_base64": strProp("the asset body, base64-encoded"),
		}, "path", "content_base64"), mcpUploadAsset),
	readTool("begin_upload", websecurity.ScopeOperator,
		"Begin a staged upload and return an upload_id. kind=asset requires path (the store-relative id); "+
			"kind=app accepts filename (diagnostics only). Chunks are written into the same sink the Web UI "+
			"uses (the asset store's streaming writer or the package InstallApplication stream), so the full "+
			"32 MiB asset limit and package archive limit apply without buffering the file in daemon memory. "+
			"Follow with upload_chunk calls, then complete_upload, or abort_upload to discard.",
		objectSchema(map[string]any{
			"kind":     strEnumProp("what is being uploaded", "asset", "app"),
			"path":     strProp("store-relative asset path; required for kind=asset"),
			"filename": strProp("archive name for kind=app, diagnostics only"),
		}, "kind"), mcpBeginUpload),
	readTool("upload_chunk", websecurity.ScopeOperator,
		"Append one base64 chunk to a staged upload, in order. A chunk may carry at most 1 MiB decoded; "+
			"writes are backpressured by the daemon sink, so a slow disk slows the call instead of buffering "+
			"the file. Send chunks sequentially: a chunk that arrives while the upload is already completing "+
			"is refused rather than appended. A chunk call also fails with the sink's own result once the "+
			"store or daemon refuses the upload.",
		objectSchema(map[string]any{
			"upload_id":      strProp("the id begin_upload returned"),
			"content_base64": strProp("the next slice of the file, base64-encoded (at most 1 MiB decoded)"),
		}, "upload_id", "content_base64"), mcpUploadChunk),
	readTool("complete_upload", websecurity.ScopeOperator,
		"Finish a staged upload and return the asset entry or the installed-package result. The file is "+
			"published/installed atomically by the same code path the Web UI uses.",
		objectSchema(map[string]any{"upload_id": strProp("the staged upload to finish")}, "upload_id"), mcpCompleteUpload),
	readTool("abort_upload", websecurity.ScopeOperator,
		"Discard a staged upload; the daemon removes the partial file and cancels the sink. It is "+
			"idempotent: aborting an unknown or already finished id reports aborted=false.",
		objectSchema(map[string]any{"upload_id": strProp("the staged upload to discard")}, "upload_id"), mcpAbortUpload),
	readTool("delete_asset", websecurity.ScopeOperator,
		"Delete one regular file from the shared asset store. Directories and symlinks are never removed.",
		objectSchema(map[string]any{"path": strProp("store-relative asset path")}, "path"), mcpDeleteAsset),
}

// --- read handlers ---------------------------------------------------------

func mcpGetStatus(ctx context.Context, s *Server, _ *mcpSession, _ json.RawMessage) (*mcpToolResult, *mcpError) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dev := frameworkv1.NewDeviceServiceClient(s.Conn)
	info, err := dev.GetInfo(ctx, &frameworkv1.GetInfoRequest{})
	if err != nil {
		return nil, mcpRPCCallError(err)
	}
	resources, _ := dev.ListResources(ctx, &frameworkv1.ListResourcesRequest{})
	runs, _ := frameworkv1.NewApplicationServiceClient(s.Conn).ListRuns(ctx, &frameworkv1.ListRunsRequest{})
	system, _ := frameworkv1.NewSystemServiceClient(s.Conn).GetStatus(ctx, &frameworkv1.GetStatusRequest{})
	raw, err := json.Marshal(map[string]json.RawMessage{
		"device":    mcpProtoJSON(info),
		"resources": mcpProtoListJSON(resources.GetResources()),
		"runs":      mcpProtoListJSON(runs.GetRuns()),
		"status":    mcpProtoJSON(system),
	})
	if err != nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the status"}
	}
	return mcpJSONResult(raw)
}

func mcpGetCertificate(_ context.Context, s *Server, _ *mcpSession, _ json.RawMessage) (*mcpToolResult, *mcpError) {
	pem, err := os.ReadFile(s.Auth.CertPath())
	if err != nil {
		return mcpToolFailure(errNotFound, "the server certificate is unavailable")
	}
	return &mcpToolResult{
		Content:           []mcpContent{{Type: "text", Text: string(pem), MimeType: "application/x-pem-file"}},
		StructuredContent: map[string]any{"pem": string(pem)},
	}, nil
}

func mcpListAssets(_ context.Context, s *Server, _ *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	if s.Assets == nil {
		return mcpToolFailure(errUnavailable, "asset store is unavailable")
	}
	var in struct {
		Prefix string `json:"prefix"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	list := s.Assets.List(strings.TrimSpace(in.Prefix))
	var total int64
	for _, entry := range list {
		total += entry.Size
	}
	raw, err := json.Marshal(map[string]any{"assets": list, "total_bytes": total})
	if err != nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the asset list"}
	}
	return mcpJSONResult(raw)
}

func mcpListAPIKeys(_ context.Context, s *Server, _ *mcpSession, _ json.RawMessage) (*mcpToolResult, *mcpError) {
	keys := s.Auth.ListAPIKeys()
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, map[string]any{
			"name": key.Name, "prefix": key.Prefix, "created_unix_ms": key.CreatedAt.UnixMilli(),
			"revoked": key.Revoked, "scopes": key.Scopes,
		})
	}
	raw, err := json.Marshal(map[string]any{"keys": out})
	if err != nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the key list"}
	}
	return mcpJSONResult(raw)
}

// --- file and key handlers -------------------------------------------------

// Only an operator MCP key may mint an assistant or operator key. The
// browser's own create_api_key operation keeps minting status-only keys.
func mcpCreateAPIKey(_ context.Context, s *Server, _ *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	var in struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: "key name is required"}
	}
	value, info, err := s.Auth.CreateAPIKeyWithScopes(name, in.Scopes)
	if err != nil {
		message := err.Error()
		switch {
		case strings.HasPrefix(message, "key name must match"), strings.HasPrefix(message, "unknown API key scope"):
			return nil, &mcpError{Code: mcpCodeInvalidParams, Message: message}
		case strings.Contains(message, "already exists"), strings.HasPrefix(message, "maximum of"):
			return mcpToolFailure(errConflict, message)
		default:
			return mcpToolFailure(errInternal, message)
		}
	}
	s.publish(uichanges.Access)
	raw, err := json.Marshal(map[string]any{
		"name": info.Name, "value": value, "prefix": info.Prefix, "scopes": info.Scopes,
	})
	if err != nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the new key"}
	}
	return mcpJSONResult(raw)
}

func mcpInstallApp(ctx context.Context, s *Server, _ *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	var in struct {
		ContentBase64 string `json:"content_base64"`
		Filename      string `json:"filename"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	archive, rpcErr := mcpDecodeBase64(in.ContentBase64, "content_base64")
	if rpcErr != nil {
		return nil, rpcErr
	}
	return s.mcpUploadSink(ctx, "app", "", strings.TrimSpace(in.Filename), bytes.NewReader(archive))
}

func mcpRemoveApp(ctx context.Context, s *Server, _ *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	var in struct {
		AppID string `json:"app_id"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	appID := strings.TrimSpace(in.AppID)
	if appID == "" {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: "app_id is required"}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := frameworkv1.NewApplicationServiceClient(s.Conn).UninstallApplication(ctx,
		&frameworkv1.UninstallApplicationRequest{AppId: appID}); err != nil {
		return mcpRPCPackageError(err)
	}
	raw, _ := json.Marshal(map[string]any{"app_id": appID, "removed": true})
	return mcpJSONResult(raw)
}

func mcpUploadAsset(ctx context.Context, s *Server, _ *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	if s.Assets == nil {
		return mcpToolFailure(errUnavailable, "asset store is unavailable")
	}
	var in struct {
		Path          string `json:"path"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	target := strings.TrimSpace(in.Path)
	if target == "" {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: "path is required"}
	}
	body, rpcErr := mcpDecodeBase64(in.ContentBase64, "content_base64")
	if rpcErr != nil {
		return nil, rpcErr
	}
	return s.mcpUploadSink(ctx, "asset", target, "", bytes.NewReader(body))
}

func mcpDeleteAsset(_ context.Context, s *Server, _ *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	if s.Assets == nil {
		return mcpToolFailure(errUnavailable, "asset store is unavailable")
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	target := strings.TrimSpace(in.Path)
	if target == "" {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: "path is required"}
	}
	if err := s.Assets.Remove(target); err != nil {
		return mcpAssetError(err)
	}
	raw, _ := json.Marshal(map[string]any{"path": target, "removed": true})
	return mcpJSONResult(raw)
}

// --- staged uploads --------------------------------------------------------

func mcpBeginUpload(_ context.Context, s *Server, sess *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	var in struct {
		Kind     string `json:"kind"`
		Path     string `json:"path"`
		Filename string `json:"filename"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	kind := strings.TrimSpace(in.Kind)
	path := strings.TrimSpace(in.Path)
	switch kind {
	case "asset":
		if path == "" {
			return nil, &mcpError{Code: mcpCodeInvalidParams, Message: "path is required for an asset upload"}
		}
	case "app":
	default:
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: `kind must be "asset" or "app"`}
	}
	upload, rpcErr := s.startMCPUpload(sess, kind, path, strings.TrimSpace(in.Filename))
	if rpcErr != nil {
		return nil, rpcErr
	}
	raw, err := json.Marshal(map[string]any{
		"upload_id": upload.id, "kind": kind, "max_bytes": upload.limit,
	})
	if err != nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the upload handle"}
	}
	return mcpJSONResult(raw)
}

func mcpUploadChunk(_ context.Context, s *Server, sess *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	var in struct {
		UploadID      string `json:"upload_id"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	upload := sess.upload(strings.TrimSpace(in.UploadID))
	if upload == nil {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: "unknown or finished upload_id"}
	}
	// The encoded size is checked first: decoding an arbitrarily large body and
	// then rejecting it would allocate the spike the cap exists to avoid.
	if encoded := strings.TrimSpace(in.ContentBase64); len(encoded) > mcpMaxChunkBase64Bytes {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: fmt.Sprintf(
			"one chunk may carry at most %d encoded bytes (%d decoded)", mcpMaxChunkBase64Bytes, mcpMaxChunkBytes)}
	}
	data, rpcErr := mcpDecodeBase64(in.ContentBase64, "content_base64")
	if rpcErr != nil {
		return nil, rpcErr
	}
	// The encoded bound above is a pre-allocation guard; base64 padding can make
	// a one-byte-oversized payload encode to the same length, so the decoded
	// size is still the authoritative cap.
	if len(data) > mcpMaxChunkBytes {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: fmt.Sprintf(
			"one chunk may carry at most %d decoded bytes", mcpMaxChunkBytes)}
	}
	if result, rpcErr := upload.write(data); result != nil || rpcErr != nil {
		return result, rpcErr
	}
	raw, err := json.Marshal(map[string]any{
		"upload_id": upload.id, "received_bytes": upload.receivedBytes(),
	})
	if err != nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the chunk acknowledgement"}
	}
	return mcpJSONResult(raw)
}

func mcpCompleteUpload(ctx context.Context, s *Server, sess *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	var in struct {
		UploadID string `json:"upload_id"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	upload := sess.dropUpload(strings.TrimSpace(in.UploadID))
	if upload == nil {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: "unknown or finished upload_id"}
	}
	// A chunk may still be inside pw.Write. Closing the pipe under it would make
	// the reader see EOF mid-chunk and publish a truncated file, so completion
	// first stops accepting chunks and waits for the in-flight ones.
	drained, ended, rpcErr := upload.beginComplete()
	if rpcErr != nil {
		return nil, rpcErr
	}
	if ended != nil {
		return ended, nil
	}
	if drained != nil {
		select {
		case <-drained:
		case <-ctx.Done():
			upload.abort(errMCPUploadAborted)
		case <-time.After(mcpUploadDrainWait):
			upload.abort(errMCPUploadStalled)
		}
	}
	// The end of the body is what lets the sink publish the asset or close the
	// package stream.
	_ = upload.pw.Close()
	select {
	case <-upload.done:
	case <-ctx.Done():
		upload.abort(errMCPUploadAborted)
		select {
		case <-upload.done:
		case <-time.After(5 * time.Second):
		}
		return mcpToolFailure(errUnavailable, "the upload did not finish in time")
	}
	if upload.result != nil {
		return upload.result, nil
	}
	if upload.err != nil {
		return nil, upload.err
	}
	return mcpToolFailure(errInternal, "the upload produced no outcome")
}

// mcpAbortUpload discards a staged upload. It is idempotent: aborting an
// unknown id reports that nothing was aborted rather than failing a cleanup
// after a completed upload.
func mcpAbortUpload(_ context.Context, s *Server, sess *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	var in struct {
		UploadID string `json:"upload_id"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	id := strings.TrimSpace(in.UploadID)
	if id == "" {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: "upload_id is required"}
	}
	aborted := false
	if upload := sess.dropUpload(id); upload != nil {
		aborted = true
		upload.abort(errMCPUploadAborted)
		select {
		case <-upload.done:
		case <-time.After(5 * time.Second):
		}
	}
	raw, err := json.Marshal(map[string]any{"upload_id": id, "aborted": aborted})
	if err != nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the abort result"}
	}
	return mcpJSONResult(raw)
}

// mcpTrainWakeword starts one training run and optionally waits for it. The
// run is registered on the MCP session, so cancel_training and session teardown
// both abort it instead of leaking a device-side run.
func mcpTrainWakeword(ctx context.Context, s *Server, sess *mcpSession, args json.RawMessage) (*mcpToolResult, *mcpError) {
	var in struct {
		DisplayName     string   `json:"display_name"`
		Language        string   `json:"language"`
		ModelID         string   `json:"model_id"`
		Seed            uint64   `json:"seed"`
		WordSampleIDs   []string `json:"word_sample_ids"`
		RoomSampleIDs   []string `json:"room_sample_ids"`
		SpeechSampleIDs []string `json:"speech_sample_ids"`
		FalseSampleIDs  []string `json:"false_sample_ids"`
		WaitSeconds     float64  `json:"wait_seconds"`
	}
	if err := strictJSON(args, &in); err != nil {
		return nil, mcpInvalidParams(err)
	}
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: "display_name is required"}
	}
	// A huge or negative wait must not overflow time.Duration or hang a request
	// past the response budget.
	waitSeconds := in.WaitSeconds
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	if waitSeconds > mcpMaxWaitSeconds {
		waitSeconds = mcpMaxWaitSeconds
	}
	language := strings.TrimSpace(in.Language)
	if language == "" {
		language = "ru"
	}
	rec := newMCPRecorder(sess)
	s.dispatch(rec, 1, &webv1.Command{Op: &webv1.Command_Train{Train: &webv1.TrainWakeword{
		ModelId: in.ModelID, DisplayName: displayName, Language: language, Seed: in.Seed,
		WordSampleIds: in.WordSampleIDs, RoomSampleIds: in.RoomSampleIDs,
		SpeechSampleIds: in.SpeechSampleIDs, FalseSampleIds: in.FalseSampleIDs,
	}}}, ctx)
	if !rec.wait(ctx) {
		return mcpToolFailure(errUnavailable, "the daemon did not answer the training request")
	}
	res, code, message, settled := rec.snapshot()
	if !settled || res == nil {
		return mcpToolFailure(code, message)
	}
	handle := res.GetTraining()
	if handle == nil {
		return mcpProtoResult(res)
	}
	trainingID := handle.GetTrainingId()
	out := map[string]any{"training_id": trainingID, "status": "running"}
	if waitSeconds <= 0 {
		raw, err := json.Marshal(out)
		if err != nil {
			return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the training handle"}
		}
		return mcpJSONResult(raw)
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(waitSeconds*float64(time.Second)))
	defer cancel()
	if done := rec.trainingDone(trainingID); done != nil {
		select {
		case <-done:
		case <-waitCtx.Done():
		}
	}
	last := rec.lastProgress()
	status, stage := mcpTrainingOutcome(last)
	out["status"], out["stage"] = status, stage
	if last != nil {
		out["fraction"], out["log"] = last.GetFraction(), last.GetLog()
		if last.GetFailure() != nil {
			out["failure"] = mcpProtoJSON(last.GetFailure())
		}
		if len(last.GetMetrics()) > 0 {
			out["metrics"] = mcpProtoListJSON(last.GetMetrics())
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the training outcome"}
	}
	result, rpcErr := mcpJSONResult(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if status == "error" {
		result.IsError = true
	}
	return result, nil
}

// mcpTrainingOutcome maps the newest progress stage onto a reported status.
// "installed" is the daemon's terminal success stage and "error" its terminal
// failure stage; anything else means the run had not finished when the wait
// expired.
func mcpTrainingOutcome(p *webv1.Progress) (status, stage string) {
	if p == nil {
		return "running", ""
	}
	switch p.GetStage() {
	case "installed":
		return "installed", p.GetStage()
	case "error":
		return "error", p.GetStage()
	default:
		return "running", p.GetStage()
	}
}

// --- staged and one-call uploads ------------------------------------------

// mcpUploadSink consumes one upload body into the same sink the browser uses:
// the asset store's streaming writer, or the package InstallApplication stream.
// The one-call tools and the staged chunk tools both end here, so the size
// limits, error mapping and result shape cannot diverge.
func (s *Server) mcpUploadSink(ctx context.Context, kind, path, filename string, body io.Reader) (*mcpToolResult, *mcpError) {
	switch kind {
	case "asset":
		if s.Assets == nil {
			return mcpToolFailure(errUnavailable, "asset store is unavailable")
		}
		entry, err := s.Assets.Write(path, body)
		if err != nil {
			if result, ok := mcpUploadCauseResult(err); ok {
				return result, nil
			}
			return mcpAssetError(err)
		}
		raw, err := json.Marshal(map[string]any{"asset": entry})
		if err != nil {
			return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the asset"}
		}
		return mcpJSONResult(raw)
	case "app":
		res, err := s.installArchive(ctx, body, filename)
		if err != nil {
			if result, ok := mcpUploadCauseResult(err); ok {
				return result, nil
			}
			return mcpRPCPackageError(err)
		}
		app := res.GetApplication()
		raw, err := json.Marshal(map[string]any{
			"app_id": app.GetAppId(), "display_name": app.GetDisplayName(), "version": app.GetVersion(),
			"replaced": res.GetReplaced(), "previous_version": res.GetPreviousVersion(),
		})
		if err != nil {
			return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the install result"}
		}
		return mcpJSONResult(raw)
	default:
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: `kind must be "asset" or "app"`}
	}
}

// --- shared helpers --------------------------------------------------------

// strictJSON decodes a tool argument object, rejecting unknown fields so a
// typo is an error instead of a silently ignored argument.
func strictJSON(args json.RawMessage, out any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	return nil
}

// mcpDecodeBase64 decodes one bounded base64 argument. The decoded size is
// bounded by the request limit, so a caller cannot amplify memory.
func mcpDecodeBase64(value, field string) ([]byte, *mcpError) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: field + " is required"}
	}
	data, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: field + " is not valid base64"}
	}
	if len(data) == 0 {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: field + " is empty"}
	}
	return data, nil
}

func mcpProtoJSON(msg proto.Message) json.RawMessage {
	if msg == nil {
		return json.RawMessage("null")
	}
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		return json.RawMessage("null")
	}
	return raw
}

func mcpProtoListJSON[T proto.Message](items []T) json.RawMessage {
	parts := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		parts = append(parts, mcpProtoJSON(item))
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return json.RawMessage("[]")
	}
	return raw
}

// mcpRPCCallError maps a failed read onto a protocol error: the daemon could
// not answer, which is not a tool-level refusal.
func mcpRPCCallError(err error) *mcpError {
	st, _ := status.FromError(err)
	message := st.Message()
	if message == "" {
		message = err.Error()
	}
	code := mcpCodeInternal
	switch st.Code() {
	case codes.InvalidArgument, codes.NotFound, codes.FailedPrecondition:
		code = mcpCodeInvalidParams
	case codes.PermissionDenied, codes.Unauthenticated:
		code = mcpCodeForbidden
	case codes.Unavailable, codes.DeadlineExceeded:
		code = mcpCodeInternal
	}
	return &mcpError{Code: code, Message: message}
}

// mcpRPCPackageError maps a package refusal onto a failed tool call, keeping
// the daemon's actionable explanation for a client-visible refusal.
func mcpRPCPackageError(err error) (*mcpToolResult, *mcpError) {
	class, message := classifyPackageFailure(err)
	switch class {
	case packageFailureInvalid, packageFailureTooLarge:
		return mcpToolFailure(errInvalid, message)
	case packageFailureNotFound:
		return mcpToolFailure(errNotFound, message)
	case packageFailureConflict:
		return mcpToolFailure(errConflict, message)
	case packageFailureForbidden:
		return mcpToolFailure(errForbidden, message)
	case packageFailureBusy, packageFailureUnavailable:
		return mcpToolFailure(errUnavailable, message)
	default:
		return mcpToolFailure(errInternal, message)
	}
}

// mcpAssetError maps an asset-store refusal onto a failed tool call.
func mcpAssetError(err error) (*mcpToolResult, *mcpError) {
	switch {
	case errors.Is(err, assets.ErrTooLarge):
		return mcpToolFailure(errInvalid, "asset exceeds the 32 MiB limit")
	case errors.Is(err, assets.ErrStoreFull):
		return mcpToolFailure(errInvalid, "asset store is full")
	case errors.Is(err, assets.ErrDeleted):
		return mcpToolFailure(errConflict, err.Error())
	case errors.Is(err, syscall.ENOSPC):
		return mcpToolFailure(errUnavailable, "device storage is full")
	case errors.Is(err, assets.ErrInvalidPath):
		return mcpToolFailure(errInvalid, err.Error())
	case errors.Is(err, fs.ErrNotExist):
		return mcpToolFailure(errNotFound, "asset not found")
	case errors.Is(err, fs.ErrPermission):
		return mcpToolFailure(errForbidden, "asset store permission denied")
	default:
		return mcpToolFailure(errInternal, "asset store operation failed")
	}
}
