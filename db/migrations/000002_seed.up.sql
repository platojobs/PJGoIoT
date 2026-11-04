-- 000002_seed：公共元数据 seed。
-- GATT UUID 来源：Bluetooth SIG Assigned Numbers（标准服务）与 Nordic Semiconductor 官方 NUS 文档。
-- 固件版本仅收录官方公开的开发板/SDK 参考版本，未编造量产设备固件号。

INSERT INTO device_types (type, name, vendor, description) VALUES
    ('cycling_speed_cadence', '骑行速度/踏频传感器', 'Bluetooth SIG', '基于 Bluetooth SIG Cycling Speed and Cadence 标准服务的骑行传感器，可读取踏频与轮速。'),
    ('environmental_sensor', '环境传感器', 'Bluetooth SIG', '基于 Bluetooth SIG Environmental Sensing 标准服务的温湿度、气压等环境数据采集设备。'),
    ('heart_rate_monitor', '心率传感器', 'Bluetooth SIG', '基于 Bluetooth SIG Heart Rate 标准服务的心率带或心率传感器。'),
    ('nordic_uart', 'Nordic UART 透传设备', 'Nordic Semiconductor', '使用 Nordic UART Service（NUS）进行双向串口透传的 BLE 设备。'),
    ('smart_scale', '智能体脂秤', 'Bluetooth SIG', '基于 Bluetooth SIG Body Composition 与 Weight Scale 标准服务的体脂秤。')
ON CONFLICT (type) DO UPDATE
SET name = EXCLUDED.name, vendor = EXCLUDED.vendor, description = EXCLUDED.description;

INSERT INTO gatt_services (device_type, uuid, name, position) VALUES
    ('cycling_speed_cadence', '00001816-0000-1000-8000-00805f9b34fb', 'Cycling Speed and Cadence', 0),
    ('cycling_speed_cadence', '0000180f-0000-1000-8000-00805f9b34fb', 'Battery Service', 1),
    ('cycling_speed_cadence', '0000180a-0000-1000-8000-00805f9b34fb', 'Device Information', 2),

    ('environmental_sensor', '0000181a-0000-1000-8000-00805f9b34fb', 'Environmental Sensing', 0),
    ('environmental_sensor', '0000180f-0000-1000-8000-00805f9b34fb', 'Battery Service', 1),
    ('environmental_sensor', '0000180a-0000-1000-8000-00805f9b34fb', 'Device Information', 2),

    ('heart_rate_monitor', '0000180d-0000-1000-8000-00805f9b34fb', 'Heart Rate', 0),
    ('heart_rate_monitor', '0000180f-0000-1000-8000-00805f9b34fb', 'Battery Service', 1),
    ('heart_rate_monitor', '0000180a-0000-1000-8000-00805f9b34fb', 'Device Information', 2),

    ('nordic_uart', '6e400001-b5a3-f393-e0a9-e50e24dcca9e', 'Nordic UART Service', 0),
    ('nordic_uart', '00001800-0000-1000-8000-00805f9b34fb', 'Generic Access', 1),
    ('nordic_uart', '0000180a-0000-1000-8000-00805f9b34fb', 'Device Information', 2),

    ('smart_scale', '0000181b-0000-1000-8000-00805f9b34fb', 'Body Composition', 0),
    ('smart_scale', '0000181d-0000-1000-8000-00805f9b34fb', 'Weight Scale', 1),
    ('smart_scale', '0000180f-0000-1000-8000-00805f9b34fb', 'Battery Service', 2),
    ('smart_scale', '0000180a-0000-1000-8000-00805f9b34fb', 'Device Information', 3)
ON CONFLICT (device_type, uuid) DO UPDATE
SET name = EXCLUDED.name, position = EXCLUDED.position;

INSERT INTO public_devices (mac, vendor, model, device_type, firmware_version, compatibility) VALUES
    (
        'aa:bb:cc:00:00:01', 'Nordic Semiconductor', 'nRF52840 DK', 'nordic_uart',
        'nRF Connect SDK v2.9.0',
        '{"platforms":["android","ios"],"minAppVersion":"1.0.0","protocol":"ble-gatt","features":["uart-transparent"]}'::jsonb
    ),
    (
        'aa:bb:cc:00:00:02', 'Espressif Systems', 'ESP32-C3-DevKitM-1', 'environmental_sensor',
        'ESP-IDF v5.2.2',
        '{"platforms":["android","ios"],"minAppVersion":"1.0.0","protocol":"ble-gatt","features":["temperature","humidity","pressure"]}'::jsonb
    ),
    (
        'aa:bb:cc:00:00:03', 'Nordic Semiconductor', 'nRF5340 DK', 'nordic_uart',
        'nRF Connect SDK v2.9.0',
        '{"platforms":["android","ios"],"minAppVersion":"1.0.0","protocol":"ble-gatt","features":["uart-transparent"]}'::jsonb
    )
ON CONFLICT (mac) DO UPDATE
SET vendor = EXCLUDED.vendor,
    model = EXCLUDED.model,
    device_type = EXCLUDED.device_type,
    firmware_version = EXCLUDED.firmware_version,
    compatibility = EXCLUDED.compatibility;

INSERT INTO firmwares (id, device_type, version, release_notes, released_at) VALUES
    (
        'fw-nrf-ncs-2.9.0', 'nordic_uart', 'nRF Connect SDK v2.9.0',
        'Nordic Semiconductor 官方 nRF Connect SDK 发布版本。此处作为开发板参考固件版本，量产设备的具体固件版本以厂商发布为准。',
        ''
    ),
    (
        'fw-esp-idf-5.2.2', 'environmental_sensor', 'ESP-IDF v5.2.2',
        'Espressif 官方 ESP-IDF 发布版本。此处作为开发板参考固件版本，量产设备的具体固件版本以厂商发布为准。',
        ''
    )
ON CONFLICT (id) DO UPDATE
SET version = EXCLUDED.version, release_notes = EXCLUDED.release_notes, released_at = EXCLUDED.released_at;

INSERT INTO accessories (id, device_type, name, sku, description) VALUES
    (
        'acc-hrm-polar-pro-strap', 'heart_rate_monitor', 'Polar Pro 心率带胸带', '',
        'Polar 官方在售的可替换心率带胸带；具体 SKU 与尺码以厂商渠道为准。'
    ),
    (
        'acc-esp-usb-c-cable', 'environmental_sensor', 'USB-C 数据线', '',
        'ESP32-C3-DevKitM-1 开发板的供电与串口调试线缆。'
    ),
    (
        'acc-scale-aa-battery', 'smart_scale', 'AA 碱性电池', '',
        '通用 1.5V AA 电池；实际电池型号以设备说明书为准。'
    )
ON CONFLICT (id) DO UPDATE
SET name = EXCLUDED.name, sku = EXCLUDED.sku, description = EXCLUDED.description;
