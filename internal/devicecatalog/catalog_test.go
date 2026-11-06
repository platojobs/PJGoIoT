package devicecatalog

import (
	"testing"

	"github.com/google/uuid"
)

// TestTypesIntegrity 保证唯一真值表自身合法：type 唯一、UUID 为 128 位、
// 指标 key 在单个类型内不重复。迁移 seed 与一致性测试都以这张表为基准。
func TestTypesIntegrity(t *testing.T) {
	seen := map[string]struct{}{}
	for _, typ := range Types() {
		if typ.Type == "" || typ.DisplayName == "" || typ.Description == "" {
			t.Fatalf("%q 基础字段不完整", typ.Type)
		}
		if _, dup := seen[typ.Type]; dup {
			t.Fatalf("type 重复: %s", typ.Type)
		}
		seen[typ.Type] = struct{}{}

		for i, svc := range typ.GATTServices {
			if _, err := uuid.Parse(svc.UUID); err != nil {
				t.Fatalf("%s gattServices[%d].uuid 非法: %v", typ.Type, i, err)
			}
			if svc.Name == "" {
				t.Fatalf("%s gattServices[%d].name 为空", typ.Type, i)
			}
		}
		metrics := map[string]struct{}{}
		for i, m := range typ.Metrics {
			if m.Key == "" || m.Unit == "" {
				t.Fatalf("%s metrics[%d] 字段不完整", typ.Type, i)
			}
			if _, dup := metrics[m.Key]; dup {
				t.Fatalf("%s metrics key 重复: %s", typ.Type, m.Key)
			}
			metrics[m.Key] = struct{}{}
			if _, err := uuid.Parse(m.CharacteristicUUID); err != nil {
				t.Fatalf("%s metrics[%d].characteristicUuid 非法: %v", typ.Type, i, err)
			}
		}
	}
}
