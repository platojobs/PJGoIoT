// Package devicecatalog 是设备类型目录的唯一真值来源。
//
// 目录中的 GATT 服务、可解析指标、来源特征 UUID 与限制说明都只在这里维护：
//   - `go run ./cmd/cataloggen` 会据此生成迁移 000015 的 seed；
//   - `/api/v1/catalog/device-types` 返回的内容由迁移落库后读取；
//   - 一致性测试把接口返回的 metrics / 特征 UUID 与本包逐项比对，任何手改迁移或
//     文档漂移都会被测试拦下。
//
// 客户端解析器（Android/iOS BLE 编解码）以本目录公布的指标 key 与特征 UUID 为准，
// 不再各自在客户端里硬编码一份。
package devicecatalog

// Compatibility 描述设备类型所需的平台与系统能力。
// 字段与既有 device_types.compatibility 保持兼容，可被客户端直接消费。
type Compatibility struct {
	Platforms     []string `json:"platforms"`
	MinAppVersion string   `json:"minAppVersion,omitempty"`
	Protocol      string   `json:"protocol,omitempty"`
	Features      []string `json:"features,omitempty"`
}

// Service 是一个 GATT 服务（UUID 与可读名称）。
type Service struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// Metric 是某设备类型可解析的一个指标。
// CharacteristicUUID 是该指标来源的 GATT 特征（不是服务）UUID，客户端据此定位订阅目标。
type Metric struct {
	Key                string `json:"key"`
	Unit               string `json:"unit"`
	CharacteristicUUID string `json:"characteristicUuid"`
}

// Type 是单个设备类型的完整目录项。
type Type struct {
	Type          string        `json:"type"`
	DisplayName   string        `json:"displayName"`
	Description   string        `json:"description"`
	GATTServices  []Service     `json:"gattServices"`
	Metrics       []Metric      `json:"metrics"`
	Limitations   []string      `json:"limitations"`
	Compatibility Compatibility `json:"compatibility"`
}

// 常用 GATT 服务 UUID（Bluetooth SIG Assigned Numbers）。
const (
	ServiceGenericAccess        = "00001800-0000-1000-8000-00805f9b34fb"
	ServiceDeviceInformation    = "0000180a-0000-1000-8000-00805f9b34fb"
	ServiceHeartRate            = "0000180d-0000-1000-8000-00805f9b34fb"
	ServiceBattery              = "0000180f-0000-1000-8000-00805f9b34fb"
	ServiceCyclingSpeedCadence  = "00001816-0000-1000-8000-00805f9b34fb"
	ServiceEnvironmentalSensing = "0000181a-0000-1000-8000-00805f9b34fb"
	ServiceBodyComposition      = "0000181b-0000-1000-8000-00805f9b34fb"
	ServiceWeightScale          = "0000181d-0000-1000-8000-00805f9b34fb"
	ServiceNordicUART           = "6e400001-b5a3-f393-e0a9-e50e24dcca9e"
)

// 常用 GATT 特征 UUID。
const (
	CharTemperatureMeasurement     = "00002a6e-0000-1000-8000-00805f9b34fb"
	CharHumidity                   = "00002a6f-0000-1000-8000-00805f9b34fb"
	CharPressure                   = "00002a6d-0000-1000-8000-00805f9b34fb"
	CharHeartRateMeasurement       = "00002a37-0000-1000-8000-00805f9b34fb"
	CharCyclingSpeedCadenceMeasure = "00002a5b-0000-1000-8000-00805f9b34fb"
	CharWeightMeasurement          = "00002a9d-0000-1000-8000-00805f9b34fb"
	CharBodyCompositionMeasurement = "00002a9c-0000-1000-8000-00805f9b34fb"
)

// Types 按 type 字母序返回全部目录项（返回副本，调用方可安全修改切片元素）。
func Types() []Type {
	out := make([]Type, len(types))
	copy(out, types)
	for i := range out {
		out[i].GATTServices = append([]Service{}, types[i].GATTServices...)
		out[i].Metrics = append([]Metric{}, types[i].Metrics...)
		out[i].Limitations = append([]string{}, types[i].Limitations...)
	}
	return out
}

// Find 按 type 查找目录项。
func Find(deviceType string) (Type, bool) {
	for _, t := range types {
		if t.Type == deviceType {
			return copyType(t), true
		}
	}
	return Type{}, false
}

func copyType(t Type) Type {
	t.GATTServices = append([]Service{}, t.GATTServices...)
	t.Metrics = append([]Metric{}, t.Metrics...)
	t.Limitations = append([]string{}, t.Limitations...)
	return t
}

// types 是唯一真值表。新增型号或调整指标只改这里，再运行 go run ./cmd/cataloggen。
var types = []Type{
	{
		Type:        "cycling_speed_cadence",
		DisplayName: "骑行速度/踏频传感器",
		Description: "基于 Bluetooth SIG Cycling Speed and Cadence 标准服务的骑行传感器，可读取踏频与轮速。",
		GATTServices: []Service{
			{UUID: ServiceCyclingSpeedCadence, Name: "Cycling Speed and Cadence"},
			{UUID: ServiceBattery, Name: "Battery Service"},
			{UUID: ServiceDeviceInformation, Name: "Device Information"},
		},
		Metrics: []Metric{
			{Key: "cadence_rpm", Unit: "rpm", CharacteristicUUID: CharCyclingSpeedCadenceMeasure},
			{Key: "wheel_speed_kmh", Unit: "km/h", CharacteristicUUID: CharCyclingSpeedCadenceMeasure},
		},
		Limitations: []string{
			"踏频与轮速来自同一个 CSC Measurement 特征，需要按标志位区分字段。",
			"轮速换算依赖客户端配置的车轮周长；未配置时只保留原始圈数。",
		},
		Compatibility: Compatibility{
			Platforms:     []string{"android", "ios"},
			MinAppVersion: "1.0.0",
			Protocol:      "ble-gatt",
			Features:      []string{"cycling-speed", "cycling-cadence"},
		},
	},
	{
		Type:        "environmental_sensor",
		DisplayName: "环境传感器",
		Description: "基于 Bluetooth SIG Environmental Sensing 标准服务的温湿度、气压等环境数据采集设备。",
		GATTServices: []Service{
			{UUID: ServiceEnvironmentalSensing, Name: "Environmental Sensing"},
			{UUID: ServiceBattery, Name: "Battery Service"},
			{UUID: ServiceDeviceInformation, Name: "Device Information"},
		},
		Metrics: []Metric{
			{Key: "temperature_celsius", Unit: "celsius", CharacteristicUUID: CharTemperatureMeasurement},
			{Key: "relative_humidity", Unit: "%", CharacteristicUUID: CharHumidity},
			{Key: "pressure_hpa", Unit: "hectopascal", CharacteristicUUID: CharPressure},
		},
		Limitations: []string{
			"仅解析标准 Environmental Sensing 特征；厂商私有格式需要客户端自行适配。",
			"温度特征可能同时包含华氏/摄氏标志位，客户端上报前统一归一化为摄氏度。",
		},
		Compatibility: Compatibility{
			Platforms:     []string{"android", "ios"},
			MinAppVersion: "1.0.0",
			Protocol:      "ble-gatt",
			Features:      []string{"temperature", "humidity", "pressure"},
		},
	},
	{
		Type:        "heart_rate_monitor",
		DisplayName: "心率传感器",
		Description: "基于 Bluetooth SIG Heart Rate 标准服务的心率带或心率传感器。",
		GATTServices: []Service{
			{UUID: ServiceHeartRate, Name: "Heart Rate"},
			{UUID: ServiceBattery, Name: "Battery Service"},
			{UUID: ServiceDeviceInformation, Name: "Device Information"},
		},
		Metrics: []Metric{
			{Key: "heart_rate", Unit: "bpm", CharacteristicUUID: CharHeartRateMeasurement},
		},
		Limitations: []string{
			"只解析 Heart Rate Measurement 的即时心率；RR 间期等派生指标由客户端计算后另行上报。",
			"部分设备需要先订阅 Battery Service 才会开启心率通知，以厂商说明为准。",
		},
		Compatibility: Compatibility{
			Platforms:     []string{"android", "ios"},
			MinAppVersion: "1.0.0",
			Protocol:      "ble-gatt",
			Features:      []string{"heart-rate"},
		},
	},
	{
		Type:        "nordic_uart",
		DisplayName: "Nordic UART 透传设备",
		Description: "使用 Nordic UART Service（NUS）进行双向串口透传的 BLE 设备。",
		GATTServices: []Service{
			{UUID: ServiceNordicUART, Name: "Nordic UART Service"},
			{UUID: ServiceGenericAccess, Name: "Generic Access"},
			{UUID: ServiceDeviceInformation, Name: "Device Information"},
		},
		// NUS 是透传通道，载荷格式由设备固件定义，服务端没有可声明的标准指标。
		Metrics: []Metric{},
		Limitations: []string{
			"NUS 仅提供透传通道，载荷格式由设备固件决定；服务端不解析原始字节。",
			"指标 key 与单位由客户端按固件协议解析后上报，服务端只做校验与归档。",
		},
		Compatibility: Compatibility{
			Platforms:     []string{"android", "ios"},
			MinAppVersion: "1.0.0",
			Protocol:      "ble-gatt",
			Features:      []string{"uart-transparent"},
		},
	},
	{
		Type:        "smart_scale",
		DisplayName: "智能体脂秤",
		Description: "基于 Bluetooth SIG Body Composition 与 Weight Scale 标准服务的体脂秤。",
		GATTServices: []Service{
			{UUID: ServiceBodyComposition, Name: "Body Composition"},
			{UUID: ServiceWeightScale, Name: "Weight Scale"},
			{UUID: ServiceBattery, Name: "Battery Service"},
			{UUID: ServiceDeviceInformation, Name: "Device Information"},
		},
		Metrics: []Metric{
			{Key: "weight_kg", Unit: "kg", CharacteristicUUID: CharWeightMeasurement},
			{Key: "body_fat_percent", Unit: "%", CharacteristicUUID: CharBodyCompositionMeasurement},
			{Key: "bmi", Unit: "kg/m2", CharacteristicUUID: CharBodyCompositionMeasurement},
		},
		Limitations: []string{
			"体重单位可能为 kg 或 lb，客户端需按 Weight Scale Feature 归一化为千克后上报。",
			"体脂率与 BMI 依赖厂商算法与用户身高配置，不同型号结果不可直接横向比较。",
		},
		Compatibility: Compatibility{
			Platforms:     []string{"android", "ios"},
			MinAppVersion: "1.0.0",
			Protocol:      "ble-gatt",
			Features:      []string{"weight", "body-composition"},
		},
	},
}
