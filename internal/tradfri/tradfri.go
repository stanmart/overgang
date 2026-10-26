// Package tradfri holds the IKEA TRÅDFRI gateway's CoAP resource paths, JSON attribute
// keys and value conventions. None of this is documented by IKEA; it follows what
// pytradfri and node-tradfri-client have reverse-engineered from the real gateway.
package tradfri

// Top-level resource paths.
const (
	PathDevices       = "15001"
	PathGroups        = "15004"
	PathMoods         = "15005"
	PathNotifications = "15006"
	PathSmartTasks    = "15010"
	PathGateway       = "15011"

	// Below PathGateway.
	PathGatewayInfo = "15012"
	PathAuth        = "9063"
	PathUnknown9124 = "9124" // watched by the app, meaning unknown
)

// The identity every app uses for its first connection, keyed with the security code
// printed on the gateway. It may only call PathAuth to obtain a real identity.
const BootstrapIdentity = "Client_identity"

// Attribute keys shared by devices, groups and moods.
const (
	AttrName           = "9001"
	AttrCreatedAt      = "9002"
	AttrID             = "9003"
	AttrReachable      = "9019"
	AttrLastSeen       = "9020"
	AttrOTAUpdateState = "9054"
	AttrDeviceType     = "5750"
	AttrDeviceInfo     = "3"
)

// Device info (AttrDeviceInfo) keys.
const (
	InfoManufacturer = "0"
	InfoModel        = "1"
	InfoSerial       = "2"
	InfoFirmware     = "3"
	InfoPowerSource  = "6"
	InfoImageType    = "7" // OTA image type, e.g. 16900 for WS bulbs
	InfoProfile      = "8" // 5 for WS bulbs, 3 for CWS, 0 otherwise
	InfoBattery      = "9"
)

// Power sources (InfoPowerSource).
const (
	PowerInternal = 1
	PowerBattery  = 3
)

// Device types (AttrDeviceType).
const (
	TypeRemote = 0
	TypeLight  = 2
	TypePlug   = 3
	TypeBlind  = 7
)

// Control lists on a device. Each holds a one-element list of settings.
const (
	AttrLightControl = "3311"
	AttrPlugControl  = "3312"
	AttrBlindControl = "15015"
	AttrSwitch       = "15009" // a remote's buttons
)

// Light, plug and blind settings.
const (
	AttrOnOff         = "5850"
	AttrDimmer        = "5851"
	AttrColorHex      = "5706"
	AttrHue           = "5707"
	AttrSaturation    = "5708"
	AttrColorX        = "5709"
	AttrColorY        = "5710"
	AttrColorTemp     = "5711" // mireds
	AttrTransition    = "5712" // tenths of a second
	AttrUnknown5849   = "5849" // meaning unknown; 2 on bulbs, 0 on plugs on the real gateway
	AttrUnknown5717   = "5717" // meaning unknown; 0 on white-spectrum bulbs
	AttrBlindPosition = "5536" // 0 is open, 100 closed
	AttrBlindTrigger  = "5523" // any write stops the blind
)

// Group and mood attributes.
const (
	AttrGroupMembers   = "9018"
	AttrHSLink         = "15002"
	AttrGroupMood      = "9039"
	AttrGroupType      = "9108"
	AttrMoodPredefined = "9068" // mood type: 2 for a user scene, 3 for "All off"
	AttrMoodLights     = "15013"
	AttrMoodPlugs      = "15021"
	AttrSceneIndex     = "9057"
	AttrSceneIcon      = "9109"
)

// Gateway attributes.
const (
	AttrIdentity          = "9090"
	AttrPSK               = "9091"
	AttrFirmware          = "9029"
	AttrNTPServer         = "9023"
	AttrCurrentTimeUnix   = "9059"
	AttrCurrentTimeISO    = "9060"
	AttrCommissioningMode = "9061"
	AttrUpdateProgress    = "9055"
	AttrOTAType           = "9066"
	AttrFirstSetup        = "9069"
	AttrTimeSource        = "9071"
	AttrGatewayID         = "9081"
	AttrHomekitID         = "9083"
	AttrGatewayUUID       = "9200"
)

// Value ranges.
const (
	MaxDimmer     = 254
	MaxColor      = 65535 // full scale of AttrColorX, AttrColorY and AttrHue
	MaxSaturation = 65279
	MinMireds     = 250 // coolest white the IKEA app offers
	MaxMireds     = 454 // warmest
)

// First IDs the gateway hands out. Real gateways number from these upwards too.
const (
	DeviceIDBase = 65536
	GroupIDBase  = 131073
	MoodIDBase   = 196608
)

// The white presets the app sends as AttrColorHex for white-spectrum bulbs, with the
// colour temperature each stands for, coolest first. pytradfri documents only three;
// the app on gateway firmware 1.21 uses five, and a real gateway reports f2eccf for
// 333 mireds. The two new steps are placed to make an even 4000–2200 K scale.
var WhitePresets = []struct {
	Hex    string
	Mireds int
}{
	{"f5faf6", 250}, // 4000 K, cool white
	{"f3f3e3", 286}, // 3500 K
	{"f2eccf", 333}, // 3000 K
	{"f1e0b5", 370}, // 2700 K, warm white
	{"efd275", 454}, // 2200 K, warm glow
}
