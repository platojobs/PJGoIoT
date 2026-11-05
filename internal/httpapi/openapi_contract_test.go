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
	if got := info["version"]; got != "0.12.0" {
		t.Fatalf("openapi info.version=%v，期望 0.12.0", got)
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
