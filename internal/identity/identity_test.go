package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

func TestLoadCreatesIdentityOnFirstUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.json")

	local, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !model.ValidCollectorID(local.CollectorID) {
		t.Fatalf("collector_id %q is not valid", local.CollectorID)
	}
	if !strings.HasPrefix(local.CollectorID, "c-") {
		t.Errorf("collector_id %q should carry the c- prefix", local.CollectorID)
	}

	// 文件必须真的落盘。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("identity file not created: %v", err)
	}

	// 再次读取必须得到同一个 ID（跨运行关联同一节点）。
	again, err := Load(path)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if again.CollectorID != local.CollectorID {
		t.Errorf("collector_id changed between loads: %q -> %q", local.CollectorID, again.CollectorID)
	}
}

func TestLoadCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "collector.json")
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("identity file not created in nested dir: %v", err)
	}
}

// TestLoadRefusesCorruptFile 是一条刻意的设计断言。
//
// 身份文件损坏时**不**静默重建：静默重建会让同一个节点在数据里
// 变成两个节点，而用户完全不会察觉，历史数据的关联也就断了。
// 宁可报错并要求用户显式删除文件。
func TestLoadRefusesCorruptFile(t *testing.T) {
	dir := t.TempDir()

	cases := map[string]string{
		"invalid json":  `{"collector_id": "c-`,
		"missing id":    `{"profile":{"country":"CN"}}`,
		"empty id":      `{"collector_id": ""}`,
		"wrong prefix":  `{"collector_id": "x-0123456789abcdef0123456789abcdef"}`,
		"too short":     `{"collector_id": "c-0123"}`,
		"not an object": `[]`,
		"uppercase hex": `{"collector_id": "c-0123456789ABCDEF0123456789abcdef"}`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := Load(path)
			if err == nil {
				t.Fatal("Load accepted a corrupted identity file; it must refuse instead of silently rebuilding")
			}
			// 错误信息必须告诉用户怎么恢复。
			if !strings.Contains(err.Error(), "删除该文件") {
				t.Errorf("error = %v, want a hint about deleting the file", err)
			}
		})
	}
}

func TestSaveRejectsInvalidID(t *testing.T) {
	dir := t.TempDir()

	for name, id := range map[string]string{
		"empty":     "",
		"no prefix": "0123456789abcdef0123456789abcdef",
		"too short": "c-1234",
	} {
		t.Run(name, func(t *testing.T) {
			err := Save(filepath.Join(dir, "x.json"), &Local{CollectorID: id})
			if err == nil {
				t.Errorf("Save accepted invalid collector_id %q", id)
			}
		})
	}

	if err := Save(filepath.Join(dir, "nil.json"), nil); err == nil {
		t.Error("Save accepted a nil Local")
	}
}

func TestSaveNormalizesProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.json")

	local := &Local{
		CollectorID: "c-0123456789abcdef0123456789abcdef",
		Profile: Profile{
			Country:   " cn ",
			Province:  " Zhejiang ",
			City:      "Hangzhou",
			ISP:       "China Mobile",
			ASN:       "9808",
			IPVersion: "IPv4",
		},
	}
	if err := Save(path, local); err != nil {
		t.Fatalf("Save: %v", err)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var raw struct {
		Profile Profile `json:"profile"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("saved file is not valid json: %v", err)
	}
	if raw.Profile.Country != "CN" {
		t.Errorf("country = %q, want CN (normalized)", raw.Profile.Country)
	}
	if raw.Profile.ASN != "AS9808" {
		t.Errorf("asn = %q, want AS9808 (normalized)", raw.Profile.ASN)
	}
	if raw.Profile.IPVersion != "ipv4" {
		t.Errorf("ip_version = %q, want ipv4", raw.Profile.IPVersion)
	}
	if raw.Profile.Province != "Zhejiang" {
		t.Errorf("province = %q, want trimmed", raw.Profile.Province)
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "collector.json")

	local := &Local{CollectorID: "c-0123456789abcdef0123456789abcdef"}
	for i := 0; i < 3; i++ {
		if err := Save(path, local); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "collector.json" {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}

func TestProfileRoundTrip(t *testing.T) {
	profile := model.CollectorProfile{
		Country:   "CN",
		Province:  "Zhejiang",
		City:      "Hangzhou",
		ISP:       "China Mobile",
		ASN:       "AS9808",
		IPVersion: model.IPVersionIPv4,
	}

	persisted := FromModel(profile)
	back := persisted.ToModel()

	if back != profile {
		t.Errorf("round trip changed the profile:\n want %+v\n got  %+v", profile, back)
	}
}

// TestGeneratedIDsAreDistinctAndOpaque 验证 ID 的生成方式。
//
// "opaque"在这里的含义：ID 只是随机十六进制，
// 不含任何可识别信息（不是 MAC、不是 IP、不是主机名哈希）。
// 这个测试同时也是"不要改成硬件指纹"的守卫：
// 一旦有人把它换成基于设备信息推导，长度或字符集会立刻变化。
func TestGeneratedIDsAreDistinctAndOpaque(t *testing.T) {
	const n = 128

	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		local := &Local{}
		id, err := model.NewCollectorID()
		if err != nil {
			t.Fatalf("NewCollectorID: %v", err)
		}
		local.CollectorID = id

		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate collector_id generated: %q", id)
		}
		seen[id] = struct{}{}

		if !model.ValidCollectorID(id) {
			t.Fatalf("generated id %q is not valid", id)
		}
		// c- + 32 位小写十六进制，没有别的花样。
		if len(id) != 34 {
			t.Fatalf("id %q has unexpected length %d", id, len(id))
		}
	}
}
