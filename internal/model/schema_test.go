package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestModelTypesHaveNoSerializationTags 是一条刻意的架构约束。
//
// 公开数据的字段名必须由**导出层**显式决定，而不是由核心模型的
// struct tag 悄悄决定。原因：
//
//   - 需求第 17 条要求"不要把业务数据结构直接绑定到外部格式"；
//   - 一旦模型带上 json/yaml 标签，改一个字段名就会无意间改变
//     公开数据的 Schema，而 review 时几乎不会有人注意到；
//   - 导出层（Phase 9）需要同时输出多种格式（JSONL / gzip / zstd），
//     各自字段集可能不同，模型不应该替它们做决定。
//
// 这条测试会在有人给模型加标签时立即失败，从而把"无意的格式变更"
// 变成一次必须显式处理的代码修改。
func TestModelTypesHaveNoSerializationTags(t *testing.T) {
	// 只有 struct 类型能带 tag；IPVersion 是字符串别名，
	// 因此这里只列出结构体，并在循环里再次确认。
	types := []any{
		Target{},
		Location{},
		ColoInfo{},
		CollectorProfile{},
	}

	const tagPrefix = "json:"

	for _, value := range types {
		rt := reflect.TypeOf(value)
		if rt.Kind() != reflect.Struct {
			t.Fatalf("%T is not a struct; keep this list to struct types", value)
		}
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if strings.Contains(string(field.Tag), tagPrefix) {
				t.Errorf("%s.%s has a json tag (%q); serialization belongs to the export layer",
					rt.Name(), field.Name, field.Tag)
			}
		}
	}
}

// TestTargetFieldSetIsStable 记录 Target 的字段集合。
//
// 目的不是"禁止改动"，而是让任何字段增删都必须显式改一次这个列表：
// Target 是数据库、导出、聚合三层的公共契约，
// 字段变化的影响面很大，值得一个显式的确认点。
func TestTargetFieldSetIsStable(t *testing.T) {
	want := []string{"ID", "IP", "Port", "IPVersion", "Location", "Colo"}

	rt := reflect.TypeOf(Target{})
	got := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		got = append(got, rt.Field(i).Name)
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Target fields = %v, want %v\n"+
			"如果这是有意的字段变更，请同步更新：模型文档、README 的数据库/导出说明，以及本测试。",
			got, want)
	}
}

// TestLocationFieldSetIsStable 记录 Location 的字段集合。
//
// 特别说明：Location 里**不应**再出现 IATA 或坐标回退标记。
// 接入点信息属于 ColoInfo，混进 Location 会让"目标在哪"变得不可信。
func TestLocationFieldSetIsStable(t *testing.T) {
	want := []string{
		"Country", "CCA2", "Region", "City", "Latitude", "Longitude",
		"CountryEN", "HasCoordinates",
	}

	rt := reflect.TypeOf(Location{})
	got := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		got = append(got, rt.Field(i).Name)
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Location fields = %v, want %v", got, want)
	}
	for _, forbidden := range []string{"IATA", "FromColoFallback"} {
		if _, ok := rt.FieldByName(forbidden); ok {
			t.Errorf("Location must not contain %s: colo information belongs to ColoInfo", forbidden)
		}
	}
}

// TestCollectorProfileHasNoIdentifyingFields 是一条隐私约束测试。
//
// 需求第 14 条：不保存公网 IP、MAC、局域网 IP 等隐私信息。
// 这里用"禁止字段名"的方式把约束固化下来：
// 只要有人往 CollectorProfile 里加入这类字段，测试就会失败。
func TestCollectorProfileHasNoIdentifyingFields(t *testing.T) {
	forbidden := []string{
		"PublicIP", "PublicIp", "IP", "IPAddress", "LocalIP", "LocalIp",
		"MAC", "MACAddress", "Hostname", "HostName", "DeviceID", "DeviceId",
		"MachineID", "MachineId", "Serial", "SerialNumber", "UUID", "Uuid",
		"Address", "HomeAddress", "Latitude", "Longitude", "Geo",
	}

	rt := reflect.TypeOf(CollectorProfile{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		for _, f := range forbidden {
			if name == f {
				t.Errorf("CollectorProfile.%s is a privacy risk: it must not carry identifying data", name)
			}
		}
	}

	// CollectorProfile 必须能被 JSON 序列化到"不含隐私字段"的形状。
	// 这里只验证不会 panic 且字段数量稳定。
	blob, err := json.Marshal(map[string]string{
		"country": "CN", "province": "Zhejiang", "city": "Hangzhou",
		"isp": "China Mobile", "asn": "AS9808", "ip_version": "ipv4",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "ip\":\"") {
		t.Errorf("serialized collector data looks like it contains an ip field: %s", blob)
	}
}
