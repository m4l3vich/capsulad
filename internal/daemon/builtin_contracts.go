package daemon

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
	ringv1 "capsulad/gen/go/capsula/contracts/ring/v1"
	"capsulad/internal/registry"
)

// Public contract identity for the compiled-in clock and ring applications.
// These services are advertised through AppInterfaceService exactly like an
// installed app's declared interface, and ordinary supervised apps call them
// through AppBusService.Call targeting the builtin app ID.
const (
	clockServiceName = "capsula.contracts.clock.v1.ClockService"
	ringServiceName  = "capsula.contracts.ring.v1.RingService"

	clockContractVersion = "1.0.0"
	ringContractVersion  = "1.0.0"
)

// clockInterface returns the discovery metadata for ClockService. The
// descriptor set is generated from the compiled Go descriptor, so it can never
// drift from the wire types.
func clockInterface() registry.Interface {
	data := descriptorSet(clockv1.File_capsula_contracts_clock_v1_clock_proto)
	return registry.Interface{
		Service: clockServiceName, Version: clockContractVersion,
		Summary:    "Built-in clock: stopwatch, timers, alarms and active alerts.",
		Docs:       clockContractDocs,
		Descriptor: data,
		Methods:    registry.MethodsForService(data, clockServiceName),
	}
}

func ringInterface() registry.Interface {
	data := descriptorSet(ringv1.File_capsula_contracts_ring_v1_ring_proto)
	return registry.Interface{
		Service: ringServiceName, Version: ringContractVersion,
		Summary:    "Built-in LED ring: render state plus validated, revisioned settings.",
		Docs:       ringContractDocs,
		Descriptor: data,
		Methods:    registry.MethodsForService(data, ringServiceName),
	}
}

func descriptorSet(file protoreflect.FileDescriptor) []byte {
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(file),
	}}
	data, err := proto.Marshal(set)
	if err != nil {
		panic("marshal builtin contract descriptor: " + err.Error())
	}
	return data
}

const clockContractDocs = `# ClockService

Standard app contract for the compiled-in Clock application. Discover it through
AppInterfaceService and call it through AppBusService.Call with
target_app_id "capsula.clock".

## GetState: returns the authoritative clock snapshot

Local time and timezone, stopwatch, timers, alarms and active alerts.
revision increases on every observable change.

## WatchState: streams a full snapshot first, then observable changes

Each event is a complete ClockState. A slow consumer keeps only the latest
snapshot, so a missed transition still converges.

## StopwatchStart: starts the stopwatch from zero when idle

No-op while already running.

## StopwatchPause: pauses a running stopwatch

Accumulated monotonic elapsed time is preserved.

## StopwatchResume: resumes a paused stopwatch

Preserves accumulated elapsed time.

## StopwatchReset: returns the stopwatch to idle

Elapsed time becomes zero.

## StartTimer: starts an independent duration timer

duration_ms must be between 1 and 2592000000.

## CancelTimer: cancels one running timer

A completed timer is removed by dismissing its alert instead.

## ListTimers: lists active and completed timers

Timers survive wall-clock and timezone changes.

## ListAlarms: lists configured alarms

Each entry is a five-field dcron schedule plus optional modifiers and carries
its next trigger, label and whether it is currently ringing.

## AddAlarm: adds one schedule to the alarms setting

The schedule is one five-field dcron expression. Optional timezone, one_shot,
disabled and label are stored without changing schedule semantics. Adding an
existing identity is idempotent. expected_revision is the app-settings revision
for compare-and-swap; 0 skips the check.

## DeleteAlarm: removes one configured alarm

alarm_id is the stable identity expr [@zone] [!once]; its label is removed too.

## SetAlarmEnabled: disables or re-enables a configured alarm

An explicit disable dismisses that alarm's active alert. A one-shot that
disarmed itself after firing keeps its alert until the user dismisses it.

## DismissAlert: stops one or every active alert

An empty alert_id dismisses all alerts and the response counts them.

## Alarms grammar

    alarms = entry { ", " entry }
    entry  = expr [ " @" IANA-zone ] [ " !once" ] [ " !disabled" ]
    expr   = minute SP hour SP day-of-month SP month SP day-of-week

Each field accepts *, a number, a range a-b, or a comma list/step combination
such as 0,30 or 1-30/5. minute 0-59, hour 0-23, day-of-month 1-31, month 1-12,
day-of-week 0-7 (0 and 7 are Sunday). A field covering every value is treated
as a wildcard for the standard DOM/DOW OR rule. Commas inside a field bind to
that field; entries are separated by a comma after the completed fifth field
and its modifiers (canonically a comma plus one space). Alarms are evaluated in
their own zone or the device timezone. A one-shot entry fires at its next match
and is then persisted with !disabled.
`

const ringContractDocs = `# RingService

Standard app contract for the compiled-in Ring application. Discover it through
AppInterfaceService and call it through AppBusService.Call with
target_app_id "capsula.ring". The baseline presentation still uses normal
routing; these RPCs only read and change declared settings.

## GetState: returns the effective rendered state

Effect, palette, brightness, speed, baseline presentation ID, active-window
state and capability-derived pixel count.

## WatchState: streams state snapshots

A full snapshot first, then the newest snapshot whenever the render state or
active window changes.

## GetSettings: reads all declared settings with their revision

Every setting is returned with the store revision used for compare-and-swap.

## UpdateSettings: validates and commits selected settings

Requires update_fields naming the fields to change and uses expected_revision
as compare-and-swap (0 skips the check). Validation is identical to
ApplicationConfigurationService; stale revisions return ABORTED and invalid
values return INVALID_ARGUMENT.

## WatchSettings: streams settings snapshots

An initial snapshot first, then convergence on the newest accepted revision; a
slow consumer may skip intermediate revisions.

## Settings

- effect: breathe, spinner, rainbow, pulse, comet, sparkle or static. Static is
  one solid colour and does not animate. Rainbow rotates the palette samples
  taken at 0%, 1/3 and 2/3 around the ring instead of inventing hues.
- palette: canonical gradient linear-gradient(<angle>deg, #RRGGBB <pct>%, ...)
  or a single #RRGGBB solid palette. Named palettes are UI conveniences only.
- speed [0.1,4], brightness [0,1].
- schedule_start/schedule_end: HH:MM in the device timezone; the half-open
  window may cross midnight and equal bounds mean all day. Outside the window
  the app keeps ownership and presents an explicit blank frame.
`
