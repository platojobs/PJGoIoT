package httpapi_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/platojobs/PJGoIoT/internal/httpapi"
	"github.com/platojobs/PJGoIoT/internal/model"
)

// TestOpenAPIFeedbackContract 校验 openapi 中的反馈契约与服务端实现（模型 JSON 字段、
// 枚举与错误码）一致，避免文档与代码各自漂移。
func TestOpenAPIFeedbackContract(t *testing.T) {
	doc := loadOpenAPIDoc(t)

	info := digMap(t, doc, "info")
	if got := info["version"]; got != "0.18.0" {
		t.Fatalf("openapi info.version=%v，期望 0.18.0", got)
	}

	paths := digMap(t, doc, "paths")
	postOp := digMap(t, asMapAny(t, paths["/feedback"]), "post")
	digMap(t, asMapAny(t, postOp["requestBody"]), "content", "application/json", "schema")
	listOp := digMap(t, asMapAny(t, paths["/feedback/mine"]), "get")
	if listOp["operationId"] != "listMyFeedback" {
		t.Fatalf("operationId=%v", listOp["operationId"])
	}

	// POST /feedback 的成功响应分别是 201（新建）与 200（幂等重放）。
	assertRef(t, postOp, "201", "#/components/schemas/FeedbackCreated")
	assertRef(t, postOp, "200", "#/components/schemas/FeedbackCreated")

	schemas := digMap(t, digMap(t, doc, "components"), "schemas")

	create := digMap(t, schemas, "FeedbackCreate")
	if got := stringSet(asStrings(t, create["required"])); !reflect.DeepEqual(got, stringSet([]string{"kind", "body", "context"})) {
		t.Fatalf("FeedbackCreate required=%v", got)
	}
	createProps := digMap(t, create, "properties")
	if got := stringSet(propsOf(createProps)); !reflect.DeepEqual(got,
		stringSet([]string{"kind", "body", "contact", "context", "diagnostics"})) {
		t.Fatalf("FeedbackCreate properties=%v", got)
	}
	kindEnum := stringSet(enumOf(t, digMap(t, createProps, "kind")))
	if !reflect.DeepEqual(kindEnum, stringSet([]string{"bug", "feature", "experience", "other"})) {
		t.Fatalf("kind enum=%v", kindEnum)
	}

	ctx := digMap(t, schemas, "FeedbackClientContext")
	if got := stringSet(asStrings(t, ctx["required"])); !reflect.DeepEqual(got,
		stringSet([]string{"platform", "appVersion", "buildType", "osVersion", "deviceModel"})) {
		t.Fatalf("FeedbackClientContext required=%v", got)
	}
	if got := stringSet(enumOf(t, digMap(t, digMap(t, ctx, "properties"), "platform"))); !reflect.DeepEqual(got,
		stringSet([]string{"android", "ios"})) {
		t.Fatalf("platform enum=%v", got)
	}

	// FeedbackItem 的 required/properties 必须与 model.Feedback 的 JSON 字段完全一致。
	item := digMap(t, schemas, "FeedbackItem")
	itemProps := digMap(t, item, "properties")
	wantFields := jsonFieldSet(t, model.Feedback{})
	if got := stringSet(asStrings(t, item["required"])); !reflect.DeepEqual(got, wantFields) {
		t.Fatalf("FeedbackItem required=%v，模型字段=%v", got, wantFields)
	}
	if got := stringSet(propsOf(itemProps)); !reflect.DeepEqual(got, wantFields) {
		t.Fatalf("FeedbackItem properties=%v，模型字段=%v", got, wantFields)
	}
	// 联系方式与诊断文本绝不出现在本人列表的契约里。
	for _, forbidden := range []string{"contact", "diagnostics"} {
		if _, ok := itemProps[forbidden]; ok {
			t.Fatalf("FeedbackItem 不应暴露 %s", forbidden)
		}
	}
	if got := stringSet(enumOf(t, digMap(t, itemProps, "status"))); !reflect.DeepEqual(got,
		stringSet([]string{"submitted", "received", "closed"})) {
		t.Fatalf("status enum=%v", got)
	}

	// 错误码枚举必须包含本轮新增的反馈限流码。
	errorSchema := digMap(t, schemas, "Error")
	codeEnum := stringSet(enumOf(t, digMap(t, digMap(t, errorSchema, "properties"), "error", "properties", "code")))
	if _, ok := codeEnum[httpapi.CodeFeedbackRateLimited]; !ok {
		t.Fatalf("错误码枚举缺少 %s: %v", httpapi.CodeFeedbackRateLimited, codeEnum)
	}
	if _, ok := codeEnum[httpapi.CodeIdempotencyConflict]; !ok {
		t.Fatalf("错误码枚举缺少 %s", httpapi.CodeIdempotencyConflict)
	}
	// 状态流转新增的错误码必须出现在契约枚举中。
	if _, ok := codeEnum[httpapi.CodeFeedbackInvalidTransition]; !ok {
		t.Fatalf("错误码枚举缺少 %s: %v", httpapi.CodeFeedbackInvalidTransition, codeEnum)
	}

	// GET /feedback/mine 支持按状态过滤，枚举需与实现常量一致。
	statusParam := findParam(t, listOp, "status")
	if got := stringSet(enumOf(t, statusParam)); !reflect.DeepEqual(got,
		stringSet([]string{"submitted", "received", "closed"})) {
		t.Fatalf("/feedback/mine status enum=%v", got)
	}

	// 用户侧单条详情：字段与 model.FeedbackDetail 完全一致，且不得出现个人信息。
	detailPath := digMap(t, paths, "/feedback/{feedbackId}")
	getOp := digMap(t, detailPath, "get")
	if getOp["operationId"] != "getMyFeedback" {
		t.Fatalf("operationId=%v", getOp["operationId"])
	}
	patchOp := digMap(t, detailPath, "patch")
	if patchOp["operationId"] != "closeMyFeedback" {
		t.Fatalf("operationId=%v", patchOp["operationId"])
	}
	detail := digMap(t, schemas, "FeedbackDetail")
	detailProps := digMap(t, detail, "properties")
	wantDetail := jsonFieldSet(t, model.FeedbackDetail{})
	if got := stringSet(asStrings(t, detail["required"])); !reflect.DeepEqual(got, wantDetail) {
		t.Fatalf("FeedbackDetail required=%v，模型字段=%v", got, wantDetail)
	}
	if got := stringSet(propsOf(detailProps)); !reflect.DeepEqual(got, wantDetail) {
		t.Fatalf("FeedbackDetail properties=%v，模型字段=%v", got, wantDetail)
	}
	for _, forbidden := range []string{"contact", "diagnostics"} {
		if _, ok := detailProps[forbidden]; ok {
			t.Fatalf("FeedbackDetail 不应暴露 %s", forbidden)
		}
	}
	if got := stringSet(enumOf(t, digMap(t, detailProps, "status"))); !reflect.DeepEqual(got,
		stringSet([]string{"submitted", "received", "closed"})) {
		t.Fatalf("FeedbackDetail status enum=%v", got)
	}

	// 用户侧关闭请求只能提交 closed。
	closeReq := digMap(t, schemas, "FeedbackCloseRequest")
	if got := stringSet(enumOf(t, digMap(t, digMap(t, closeReq, "properties"), "status"))); !reflect.DeepEqual(got,
		stringSet([]string{"closed"})) {
		t.Fatalf("FeedbackCloseRequest status enum=%v", got)
	}
	assertRef(t, getOp, "200", "#/components/schemas/FeedbackDetail")
	assertRef(t, patchOp, "200", "#/components/schemas/FeedbackDetail")

	// 运营侧：跨用户列表与状态推进，标注为内部接口且使用独立凭据。
	adminListPath := digMap(t, paths, "/admin/feedback")
	adminListOp := digMap(t, adminListPath, "get")
	adminPatchPath := digMap(t, paths, "/admin/feedback/{feedbackId}")
	adminPatchOp := digMap(t, adminPatchPath, "patch")
	for name, op := range map[string]map[string]any{"adminList": adminListOp, "adminPatch": adminPatchOp} {
		if op["x-audience"] != "internal" || op["x-client-sdk"] != false {
			t.Fatalf("%s 未标注为内部接口（不在公开客户端 SDK）", name)
		}
		if !hasSecurityScheme(t, op, "adminToken") {
			t.Fatalf("%s 未使用 adminToken 凭据", name)
		}
	}
	if got := stringSet(enumOf(t, findParam(t, adminListOp, "status"))); !reflect.DeepEqual(got,
		stringSet([]string{"submitted", "received", "closed"})) {
		t.Fatalf("/admin/feedback status enum=%v", got)
	}
	adminItem := digMap(t, schemas, "AdminFeedbackItem")
	adminProps := digMap(t, adminItem, "properties")
	allAdminFields := jsonFieldSet(t, model.AdminFeedback{})
	if got := stringSet(propsOf(adminProps)); !reflect.DeepEqual(got, allAdminFields) {
		t.Fatalf("AdminFeedbackItem properties=%v，模型字段=%v", got, allAdminFields)
	}
	if got := stringSet(asStrings(t, adminItem["required"])); !reflect.DeepEqual(got, jsonRequiredSet(t, model.AdminFeedback{})) {
		t.Fatalf("AdminFeedbackItem required=%v，模型必填字段=%v", got, jsonRequiredSet(t, model.AdminFeedback{}))
	}
	// 运营视图必须能看到个人信息与内部备注（与用户侧相反）。
	for _, required := range []string{"contact", "diagnostics", "internalNote"} {
		if _, ok := adminProps[required]; !ok {
			t.Fatalf("AdminFeedbackItem 缺少 %s", required)
		}
	}
	adminPatchReq := digMap(t, schemas, "AdminFeedbackUpdateRequest")
	if got := stringSet(enumOf(t, digMap(t, digMap(t, adminPatchReq, "properties"), "status"))); !reflect.DeepEqual(got,
		stringSet([]string{"submitted", "received", "closed"})) {
		t.Fatalf("AdminFeedbackUpdateRequest status enum=%v", got)
	}
	assertRef(t, adminListOp, "200", "#/components/schemas/AdminFeedbackPage")
	assertRef(t, adminPatchOp, "200", "#/components/schemas/AdminFeedbackItem")
}

// TestOpenAPICatalogContract 校验设备类型目录契约：模型字段、必填集合、匿名可读、
// 304 协商缓存与运营侧内部接口标注。
func TestOpenAPICatalogContract(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	paths := digMap(t, doc, "paths")

	catalogOp := digMap(t, asMapAny(t, paths["/catalog/device-types"]), "get")
	if catalogOp["operationId"] != "getDeviceTypeCatalog" {
		t.Fatalf("operationId=%v", catalogOp["operationId"])
	}
	// 公开只读：security 必须为空数组，匿名请求不被拦。
	sec, ok := catalogOp["security"].([]any)
	if !ok || len(sec) != 0 {
		t.Fatalf("/catalog/device-types 必须允许匿名读取（security 为空数组），实际 %v", catalogOp["security"])
	}
	assertRef(t, catalogOp, "200", "#/components/schemas/DeviceTypeCatalog")
	digMap(t, catalogOp, "responses", "304")

	schemas := digMap(t, digMap(t, doc, "components"), "schemas")
	for _, tc := range []struct {
		schema string
		model  any
	}{
		{"CatalogMetric", model.CatalogMetric{}},
		{"CatalogDeviceType", model.CatalogDeviceType{}},
		{"DeviceTypeCatalog", model.DeviceTypeCatalog{}},
		{"CatalogDeviceTypeInput", model.CatalogDeviceTypeInput{}},
	} {
		schema := digMap(t, schemas, tc.schema)
		wantFields := jsonFieldSet(t, tc.model)
		if got := stringSet(propsOf(digMap(t, schema, "properties"))); !reflect.DeepEqual(got, wantFields) {
			t.Fatalf("%s properties=%v，模型字段=%v", tc.schema, got, wantFields)
		}
		if got := stringSet(asStrings(t, schema["required"])); !reflect.DeepEqual(got, jsonRequiredSet(t, tc.model)) {
			t.Fatalf("%s required=%v，模型必填字段=%v", tc.schema, got, jsonRequiredSet(t, tc.model))
		}
	}
	// 指标必须声明来源特征 UUID，避免客户端把服务 UUID 当成特征 UUID。
	metricProps := digMap(t, schemas, "CatalogMetric", "properties")
	if _, ok := metricProps["characteristicUuid"]; !ok {
		t.Fatalf("CatalogMetric 缺少 characteristicUuid")
	}

	adminPath := digMap(t, paths, "/admin/catalog/device-types/{type}")
	putOp := digMap(t, adminPath, "put")
	deleteOp := digMap(t, adminPath, "delete")
	for name, op := range map[string]map[string]any{"adminPut": putOp, "adminDelete": deleteOp} {
		if op["x-audience"] != "internal" || op["x-client-sdk"] != false {
			t.Fatalf("%s 未标注为内部接口（不在公开客户端 SDK）", name)
		}
		if !hasSecurityScheme(t, op, "adminToken") {
			t.Fatalf("%s 未使用 adminToken 凭据", name)
		}
	}
	assertRef(t, putOp, "200", "#/components/schemas/CatalogDeviceType")
	if _, ok := digMap(t, deleteOp, "responses")["204"]; !ok {
		t.Fatalf("DELETE /admin/catalog/device-types/{type} 缺少 204 响应")
	}
}

// TestOpenAPIDeletionExportContract 校验账号删除与个人数据导出契约：
// 字段与模型逐一对齐、删除接口只用一次性令牌、下载接口允许匿名、错误码齐全。
func TestOpenAPIDeletionExportContract(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	paths := digMap(t, doc, "paths")
	schemas := digMap(t, digMap(t, doc, "components"), "schemas")

	requestOp := digMap(t, asMapAny(t, paths["/me/deletion-request"]), "post")
	if requestOp["operationId"] != "createDeletionRequest" || !hasSecurityScheme(t, requestOp, "bearerAuth") {
		t.Fatalf("deletion-request 契约异常: %v", requestOp)
	}
	assertRef(t, requestOp, "201", "#/components/schemas/DeletionRequest")

	// DELETE /me 只用 X-Deletion-Token：security 必须为空，且必须有必填的删除令牌头。
	deleteOp := digMap(t, asMapAny(t, paths["/me"]), "delete")
	if sec, ok := deleteOp["security"].([]any); !ok || len(sec) != 0 {
		t.Fatalf("DELETE /me 不应使用 Bearer（security 必须为空数组），实际 %v", deleteOp["security"])
	}
	list, ok := deleteOp["parameters"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("DELETE /me 缺少 X-Deletion-Token 参数")
	}
	header := asMapAny(t, list[0])
	if header["name"] != "X-Deletion-Token" || header["in"] != "header" || header["required"] != true {
		t.Fatalf("DELETE /me 删除令牌参数异常: %v", header)
	}
	for _, status := range []string{"400", "403", "404", "409", "410", "204"} {
		if _, ok := digMap(t, deleteOp, "responses")[status]; !ok {
			t.Fatalf("DELETE /me 缺少 %s 响应", status)
		}
	}

	// 导出：POST 创建 + GET 状态都要 Bearer；下载链接本身即凭据，不使用 Bearer。
	createExportOp := digMap(t, asMapAny(t, paths["/me/export"]), "post")
	if !hasSecurityScheme(t, createExportOp, "bearerAuth") {
		t.Fatalf("/me/export POST 应使用 bearerAuth")
	}
	assertRef(t, createExportOp, "202", "#/components/schemas/ExportTask")
	getExportOp := digMap(t, asMapAny(t, paths["/me/export/{id}"]), "get")
	if !hasSecurityScheme(t, getExportOp, "bearerAuth") {
		t.Fatalf("/me/export/{id} 应使用 bearerAuth")
	}
	assertRef(t, getExportOp, "200", "#/components/schemas/ExportTask")
	downloadOp := digMap(t, asMapAny(t, paths["/me/export/{id}/download"]), "get")
	if sec, ok := downloadOp["security"].([]any); !ok || len(sec) != 0 {
		t.Fatalf("下载接口不应使用 Bearer，实际 %v", downloadOp["security"])
	}
	if _, ok := digMap(t, downloadOp, "responses")["403"]; !ok {
		t.Fatalf("下载接口缺少 403")
	}
	if _, ok := digMap(t, downloadOp, "responses")["410"]; !ok {
		t.Fatalf("下载接口缺少 410")
	}
	assertRef(t, downloadOp, "200", "#/components/schemas/PersonalDataExport")

	// 新 schema 的 required/properties 必须与模型 JSON 标签逐项一致。
	for _, tc := range []struct {
		schema string
		model  any
	}{
		{"DeletionRequest", model.DeletionRequest{}},
		{"ExportTask", model.ExportTask{}},
		{"PersonalDataExport", model.PersonalDataExport{}},
		{"ExportReadingAggregate", model.ExportReadingAggregate{}},
		{"ExportHousehold", model.ExportHousehold{}},
		{"ExportRoom", model.ExportRoom{}},
		{"ExportFeedback", model.ExportFeedback{}},
	} {
		schema := digMap(t, schemas, tc.schema)
		wantFields := jsonFieldSet(t, tc.model)
		if got := stringSet(propsOf(digMap(t, schema, "properties"))); !reflect.DeepEqual(got, wantFields) {
			t.Fatalf("%s properties=%v，模型字段=%v", tc.schema, got, wantFields)
		}
		if got := stringSet(asStrings(t, schema["required"])); !reflect.DeepEqual(got, jsonRequiredSet(t, tc.model)) {
			t.Fatalf("%s required=%v，模型必填字段=%v", tc.schema, got, jsonRequiredSet(t, tc.model))
		}
	}

	// 导出产物不得包含他人联系人与运营内部备注；家庭不得暴露其他成员字段。
	for _, forbidden := range []string{"internalNote", "internal_note", "members", "ownerUserId"} {
		if _, ok := digMap(t, digMap(t, schemas, "ExportHousehold"), "properties")[forbidden]; ok {
			t.Fatalf("ExportHousehold 不应暴露 %s", forbidden)
		}
	}
	if _, ok := digMap(t, digMap(t, schemas, "ExportFeedback"), "properties")["internalNote"]; ok {
		t.Fatalf("ExportFeedback 不应暴露运营内部备注")
	}

	if got := stringSet(enumOf(t, digMap(t, digMap(t, schemas, "ExportTask"), "properties", "status"))); !reflect.DeepEqual(got,
		stringSet([]string{"pending", "ready", "failed"})) {
		t.Fatalf("ExportTask status enum=%v", got)
	}

	codeEnum := stringSet(enumOf(t, digMap(t, digMap(t, schemas, "Error"), "properties", "error", "properties", "code")))
	for _, code := range []string{
		httpapi.CodeDeletionTokenRequired,
		httpapi.CodeDeletionTokenInvalid,
		httpapi.CodeDeletionTokenExpired,
		httpapi.CodeAccountDeletionBlocked,
		httpapi.CodeExportNotReady,
		httpapi.CodeExportDownloadInvalid,
		httpapi.CodeExportDownloadExpired,
	} {
		if _, ok := codeEnum[code]; !ok {
			t.Fatalf("错误码枚举缺少 %s: %v", code, codeEnum)
		}
	}
}

// TestOpenAPISceneContract 校验契约 0.18.0 的场景上云：
// 路径、鉴权、schema 与模型字段逐项对齐，并确认 change_log 的 scene entity 在契约中有定义。
func TestOpenAPISceneContract(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	if got := digMap(t, doc, "info")["version"]; got != "0.18.0" {
		t.Fatalf("openapi info.version=%v，期望 0.18.0", got)
	}
	paths := digMap(t, doc, "paths")
	schemas := digMap(t, digMap(t, doc, "components"), "schemas")

	for _, path := range []string{"/scenes", "/scenes/{sceneId}"} {
		if _, ok := paths[path]; !ok {
			t.Fatalf("openapi 缺少路径 %s", path)
		}
	}

	listOp := digMap(t, asMapAny(t, paths["/scenes"]), "get")
	createOp := digMap(t, asMapAny(t, paths["/scenes"]), "post")
	getOp := digMap(t, asMapAny(t, paths["/scenes/{sceneId}"]), "get")
	updateOp := digMap(t, asMapAny(t, paths["/scenes/{sceneId}"]), "put")
	deleteOp := digMap(t, asMapAny(t, paths["/scenes/{sceneId}"]), "delete")
	for name, op := range map[string]map[string]any{
		"GET /scenes": listOp, "POST /scenes": createOp,
		"GET /scenes/{id}": getOp, "PUT /scenes/{id}": updateOp, "DELETE /scenes/{id}": deleteOp,
	} {
		if !hasSecurityScheme(t, op, "bearerAuth") {
			t.Fatalf("%s 应使用 bearerAuth", name)
		}
	}
	assertRef(t, listOp, "200", "#/components/schemas/SceneList")
	assertRef(t, createOp, "201", "#/components/schemas/Scene")
	assertRef(t, getOp, "200", "#/components/schemas/Scene")
	assertRef(t, updateOp, "200", "#/components/schemas/Scene")
	if _, ok := digMap(t, deleteOp, "responses")["204"]; !ok {
		t.Fatalf("DELETE /scenes/{id} 缺少 204 响应")
	}
	for _, op := range []map[string]any{createOp, updateOp} {
		if _, ok := digMap(t, op, "responses")["400"]; !ok {
			t.Fatalf("场景写入接口缺少 400 响应")
		}
	}

	// Scene 与模型逐项对齐（含 required 集合）。
	for _, tc := range []struct {
		schema string
		model  any
	}{
		{"Scene", model.Scene{}},
		{"SceneInput", model.SceneInput{}},
		{"ScenePatch", model.ScenePatch{}},
		{"SceneTrigger", model.SceneTrigger{}},
		{"SceneDeviceRef", model.SceneDeviceRef{}},
		{"SceneMetricCondition", model.SceneMetricCondition{}},
		{"SceneAction", model.SceneAction{}},
	} {
		schema := digMap(t, schemas, tc.schema)
		if got := stringSet(propsOf(digMap(t, schema, "properties"))); !reflect.DeepEqual(got, jsonFieldSet(t, tc.model)) {
			t.Fatalf("%s properties=%v，模型字段=%v", tc.schema, got, jsonFieldSet(t, tc.model))
		}
		var gotRequired map[string]struct{}
		if raw, ok := schema["required"]; ok {
			gotRequired = stringSet(asStrings(t, raw))
		} else {
			gotRequired = map[string]struct{}{}
		}
		if !reflect.DeepEqual(gotRequired, jsonRequiredSet(t, tc.model)) {
			t.Fatalf("%s required=%v，模型必填字段=%v", tc.schema, gotRequired, jsonRequiredSet(t, tc.model))
		}
	}

	// 触发条件/动作的种类枚举必须与实现常量一致。
	if got := stringSet(enumOf(t, digMap(t, digMap(t, schemas, "SceneTrigger"), "properties", "kind"))); !reflect.DeepEqual(got,
		stringSet([]string{"device_offline", "rssi_below", "metric_threshold", "schedule"})) {
		t.Fatalf("SceneTrigger kind enum=%v", got)
	}
	if got := stringSet(enumOf(t, digMap(t, digMap(t, schemas, "SceneAction"), "properties", "kind"))); !reflect.DeepEqual(got,
		stringSet([]string{"notify", "log", "mark_device"})) {
		t.Fatalf("SceneAction kind enum=%v", got)
	}

	// change_log 的 scene entity 必须在契约中可发现：实体说明与 payload oneOf 都包含 Scene。
	changeSchema := digMap(t, schemas, "Change")
	entityDesc, _ := digMap(t, changeSchema, "properties", "entity")["description"].(string)
	if !strings.Contains(entityDesc, "`scene`") {
		t.Fatalf("Change.entity 未说明 scene entity")
	}
	oneOf, ok := digMap(t, digMap(t, changeSchema, "properties", "payload"))["oneOf"].([]any)
	if !ok {
		t.Fatalf("Change.payload 缺少 oneOf")
	}
	found := false
	for _, raw := range oneOf {
		if asMapAny(t, raw)["$ref"] == "#/components/schemas/Scene" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Change.payload oneOf 未包含 Scene")
	}

	codeEnum := stringSet(enumOf(t, digMap(t, digMap(t, schemas, "Error"), "properties", "error", "properties", "code")))
	for _, code := range []string{
		httpapi.CodeSceneNameInvalid,
		httpapi.CodeSceneCooldownOutOfRange,
		httpapi.CodeSceneTriggerInvalid,
		httpapi.CodeSceneActionInvalid,
		httpapi.CodeSceneDeviceNotFound,
		httpapi.CodeSceneMetricNotSupported,
	} {
		if _, ok := codeEnum[code]; !ok {
			t.Fatalf("错误码枚举缺少 %s: %v", code, codeEnum)
		}
	}
}

// findParam 按 name 定位查询/路径参数，返回其 schema。
func findParam(t *testing.T, op map[string]any, name string) map[string]any {
	t.Helper()
	list, ok := op["parameters"].([]any)
	if !ok {
		t.Fatalf("operation 缺少 parameters")
	}
	for _, raw := range list {
		param := asMapAny(t, raw)
		if param["name"] == name {
			return digMap(t, param, "schema")
		}
	}
	t.Fatalf("缺少参数 %s", name)
	return nil
}

// TestOpenAPIPushContract 校验契约 0.18.0 的推送通道：路径、鉴权、schema 与模型字段逐项对齐。
func TestOpenAPIPushContract(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	paths := digMap(t, doc, "paths")
	schemas := digMap(t, digMap(t, doc, "components"), "schemas")

	for _, path := range []string{"/push/devices", "/push/devices/{deviceId}", "/admin/alerts"} {
		if _, ok := paths[path]; !ok {
			t.Fatalf("openapi 缺少路径 %s", path)
		}
	}
	registerOp := digMap(t, asMapAny(t, paths["/push/devices"]), "post")
	deleteOp := digMap(t, asMapAny(t, paths["/push/devices/{deviceId}"]), "delete")
	alertOp := digMap(t, asMapAny(t, paths["/admin/alerts"]), "post")
	if !hasSecurityScheme(t, registerOp, "bearerAuth") || !hasSecurityScheme(t, deleteOp, "bearerAuth") {
		t.Fatal("/push/devices 应使用 bearerAuth")
	}
	if !hasSecurityScheme(t, alertOp, "adminToken") {
		t.Fatal("/admin/alerts 应使用 adminToken")
	}
	if alertOp["x-audience"] != "internal" || alertOp["x-client-sdk"] != false {
		t.Fatalf("/admin/alerts 应标注 internal 且不进 SDK: x-audience=%v x-client-sdk=%v",
			alertOp["x-audience"], alertOp["x-client-sdk"])
	}
	assertRef(t, registerOp, "200", "#/components/schemas/PushRegistration")
	assertRef(t, alertOp, "201", "#/components/schemas/AlertCreated")

	for _, tc := range []struct {
		schema string
		value  any
	}{
		{"PushRegistration", model.PushRegistration{}},
		{"PushRegistrationInput", model.PushRegistrationInput{}},
		{"Alert", model.Alert{}},
		{"AlertInput", model.AlertInput{}},
	} {
		s := digMap(t, schemas, tc.schema)
		if got := stringSet(propsOf(digMap(t, s, "properties"))); !reflect.DeepEqual(got, jsonFieldSet(t, tc.value)) {
			t.Fatalf("%s 字段与模型不一致: %v", tc.schema, got)
		}
		if got := stringSet(asStrings(t, s["required"])); !reflect.DeepEqual(got, jsonRequiredSet(t, tc.value)) {
			t.Fatalf("%s required 与模型不一致: %v", tc.schema, got)
		}
	}

	// GET /scenes 的可选分页参数必须存在，且不改变成功响应结构。
	listOp := digMap(t, asMapAny(t, paths["/scenes"]), "get")
	params, ok := listOp["parameters"].([]any)
	if !ok || len(params) != 2 {
		t.Fatalf("GET /scenes 应有 limit 与 cursor 两个可选参数，实际 %v", listOp["parameters"])
	}
	names := map[string]bool{}
	for _, raw := range params {
		p := asMapAny(t, raw)
		names[p["name"].(string)] = true
	}
	if !names["limit"] || !names["cursor"] {
		t.Fatalf("GET /scenes 分页参数缺失: %v", names)
	}
}

func hasSecurityScheme(t *testing.T, op map[string]any, scheme string) bool {
	t.Helper()
	list, ok := op["security"].([]any)
	if !ok {
		return false
	}
	for _, raw := range list {
		if _, ok := asMapAny(t, raw)[scheme]; ok {
			return true
		}
	}
	return false
}

func loadOpenAPIDoc(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("读取 openapi.yaml 失败: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析 openapi.yaml 失败: %v", err)
	}
	return doc
}

func digMap(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := any(m)
	for _, key := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("openapi 路径 %v 在 %q 处不是对象", keys, key)
		}
		cur, ok = mm[key]
		if !ok {
			t.Fatalf("openapi 缺少字段 %v", keys)
		}
	}
	return asMapAny(t, cur)
}

func asMapAny(t *testing.T, v any) map[string]any {
	t.Helper()
	mm, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("期望对象，实际 %T", v)
	}
	return mm
}

func asStrings(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("期望数组，实际 %T", v)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("期望字符串，实际 %T", item)
		}
		out = append(out, s)
	}
	return out
}

func enumOf(t *testing.T, schema map[string]any) []string {
	t.Helper()
	return asStrings(t, schema["enum"])
}

func propsOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func stringSet(list []string) map[string]struct{} {
	out := make(map[string]struct{}, len(list))
	for _, s := range list {
		out[s] = struct{}{}
	}
	return out
}

// jsonRequiredSet 提取不带 omitempty 的 JSON 字段，对应契约中的 required 集合。
func jsonRequiredSet(t *testing.T, v any) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	typ := reflect.TypeOf(v)
	for i := 0; i < typ.NumField(); i++ {
		parts := strings.Split(typ.Field(i).Tag.Get("json"), ",")
		name := parts[0]
		if name == "" || name == "-" {
			continue
		}
		optional := false
		for _, p := range parts[1:] {
			if p == "omitempty" {
				optional = true
			}
		}
		if !optional {
			out[name] = struct{}{}
		}
	}
	return out
}

// jsonFieldSet 从结构体的 json tag 提取对外字段集合。
func jsonFieldSet(t *testing.T, v any) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	typ := reflect.TypeOf(v)
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out[name] = struct{}{}
	}
	return out
}

func assertRef(t *testing.T, op map[string]any, status, want string) {
	t.Helper()
	got := digMap(t, op, "responses", status, "content", "application/json", "schema")
	if got["$ref"] != want {
		t.Fatalf("responses.%s 的 schema=%v，期望 %s", status, got["$ref"], want)
	}
}
