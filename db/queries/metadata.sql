-- name: GetPublicDevice :one
SELECT mac, vendor, model, device_type, firmware_version, compatibility
FROM public_devices
WHERE mac = $1;

-- name: ListGATTServicesByDeviceType :many
SELECT uuid, name
FROM gatt_services
WHERE device_type = $1
ORDER BY position, uuid;

-- name: ListDeviceTypes :many
SELECT type, name, vendor, description
FROM device_types
ORDER BY type;

-- name: GetDeviceType :one
SELECT type, name, vendor, description
FROM device_types
WHERE type = $1;

-- name: ListAllGATTServices :many
SELECT device_type, uuid, name
FROM gatt_services
ORDER BY device_type, position, uuid;

-- name: DeviceTypeExists :one
SELECT EXISTS (SELECT 1 FROM device_types WHERE type = $1) AS exists;

-- name: ListFirmwaresByDeviceType :many
SELECT id, device_type, version, release_notes, released_at
FROM firmwares
WHERE device_type = $1
ORDER BY released_at DESC, version DESC;

-- name: ListAccessoriesByDeviceType :many
SELECT id, device_type, name, sku, description
FROM accessories
WHERE device_type = $1
ORDER BY id;
