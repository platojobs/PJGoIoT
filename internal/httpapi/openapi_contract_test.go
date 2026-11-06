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
	if got := info["version"]; got != "0.14.0" {
		t.Fatalf("openapi info.version=%v，期望 0.14.0", got)
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

// hasSecurityScheme 判断 operation 的 security 列表是否包含指定 scheme。
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
