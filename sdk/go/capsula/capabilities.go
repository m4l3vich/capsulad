package capsula

// Capability vocabulary implemented by this binding and by the ABI v1.0
// engine. It is the authoritative vocabulary from the native runtime
// contract; launchers also use it as preflight metadata in
// capsula-native.json.
const (
	CapabilityABIV1               = "capsula.abi.v1"
	CapabilityTransportUnixGRPC   = "capsula.transport.unix-grpc"
	CapabilityReflectJSONProto    = "capsula.reflect.json-proto"
	CapabilityMessagesRawProto    = "capsula.messages.raw-proto"
	CapabilityTransportUnary      = "capsula.transport.unary"
	CapabilityTransportServer     = "capsula.transport.server-streaming"
	CapabilityTransportClient     = "capsula.transport.client-streaming"
	CapabilityTransportBidi       = "capsula.transport.bidi"
	CapabilityBusConnect          = "capsula.bus.connect"
	CapabilityBusEvents           = "capsula.bus.events"
	CapabilityBusNotifications    = "capsula.bus.notifications"
	CapabilityBusRoutedCalls      = "capsula.bus.routed-calls"
	CapabilityAudioCapture        = "capsula.audio.capture"
	CapabilityAudioPlayback       = "capsula.audio.playback"
	CapabilityAudioPCMFastPath    = "capsula.audio.pcm-fast-path"
	CapabilityDescriptorsRegister = "capsula.descriptors.register"
	CapabilityDeadlines           = "capsula.deadlines"
	CapabilityCancel              = "capsula.cancel"
	CapabilityConcurrentHandles   = "capsula.concurrent-handles"
	CapabilityServiceDevice       = "capsula.service.device"
	CapabilityServiceSettings     = "capsula.service.settings"
	CapabilityServiceRouting      = "capsula.service.routing"
	CapabilityServicePresentation = "capsula.service.presentation"
	CapabilityServiceAudio        = "capsula.service.audio"
	CapabilityServiceInput        = "capsula.service.input"
	CapabilityServiceAssistant    = "capsula.service.assistant"
	CapabilityServiceApplication  = "capsula.service.application"
	CapabilityServiceAppConfig    = "capsula.service.application-configuration"
	CapabilityServiceAppInterface = "capsula.service.app-interface"
	CapabilityServiceAppBus       = "capsula.service.appbus"
	CapabilityServiceSensor       = "capsula.service.sensor"
	CapabilityServiceSystem       = "capsula.service.system"
	CapabilityServiceDiagnostics  = "capsula.service.diagnostics"
	CapabilityServiceAsset        = "capsula.service.asset"
	CapabilityServiceBluetooth    = "capsula.service.bluetooth-media"
)

var supportedCapabilities = map[string]struct{}{
	CapabilityABIV1:               {},
	CapabilityTransportUnixGRPC:   {},
	CapabilityReflectJSONProto:    {},
	CapabilityMessagesRawProto:    {},
	CapabilityTransportUnary:      {},
	CapabilityTransportServer:     {},
	CapabilityTransportClient:     {},
	CapabilityTransportBidi:       {},
	CapabilityBusConnect:          {},
	CapabilityBusEvents:           {},
	CapabilityBusNotifications:    {},
	CapabilityBusRoutedCalls:      {},
	CapabilityAudioCapture:        {},
	CapabilityAudioPlayback:       {},
	CapabilityAudioPCMFastPath:    {},
	CapabilityDescriptorsRegister: {},
	CapabilityDeadlines:           {},
	CapabilityCancel:              {},
	CapabilityConcurrentHandles:   {},
	CapabilityServiceDevice:       {},
	CapabilityServiceSettings:     {},
	CapabilityServiceRouting:      {},
	CapabilityServicePresentation: {},
	CapabilityServiceAudio:        {},
	CapabilityServiceInput:        {},
	CapabilityServiceAssistant:    {},
	CapabilityServiceApplication:  {},
	CapabilityServiceAppConfig:    {},
	CapabilityServiceAppInterface: {},
	CapabilityServiceAppBus:       {},
	CapabilityServiceSensor:       {},
	CapabilityServiceSystem:       {},
	CapabilityServiceDiagnostics:  {},
	CapabilityServiceAsset:        {},
	CapabilityServiceBluetooth:    {},
}

func missingCapabilities(required []string) []string {
	var missing []string
	for _, capability := range required {
		if _, ok := supportedCapabilities[capability]; !ok {
			missing = append(missing, capability)
		}
	}
	return missing
}
