// Package identity 管理本地匿名标识与采集者画像。
//
// 隐私约束（需求第 10 条）是本包存在的全部理由：
//
//	collector_id 必须是**随机生成**的匿名标识，绝不由硬件信息
//	（MAC、CPU 序列号、磁盘 ID）、公网 IP、主机名等可识别信息推导。
//	否则它就变成了设备指纹，"匿名众测"这个前提就不成立了。
//
// 它唯一的用途是把同一个匿名节点的历史数据关联起来：
//
//	换机器、重装系统、删除本地文件后都会得到新的 ID。
//	这是可接受的代价——我们不追求跨设备追踪，也不希望做到。
//
// 因此本文件里**不允许**出现任何读取硬件 / 网络标识的代码。
// 如果有人将来想"更稳定地识别设备"，那正是这个设计要阻止的事。
package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// DefaultPath 是本地标识文件的默认路径。
//
// 放在 data/ 下与数据库同级：两者是同一条本地数据链，
// 一起备份、一起删除、一起被 .gitignore 排除。
const DefaultPath = "data/collector.json"

// Local 是本地保存的匿名标识与采集者画像。
//
// 文件里只有两类内容：随机 ID、以及用户/检测得到的粗粒度地区与运营商。
// 不含任何可识别信息。
type Local struct {
	// CollectorID 是随机生成的匿名标识（形如 c-<32 位十六进制>）。
	CollectorID string `json:"collector_id"`

	// Profile 是采集者的地区 / 运营商画像。
	//
	// 它允许被手动修改（Phase 6 的 detect 会写入自动检测结果，
	// 用户也可以手工改正）：公网 ASN 不一定等于用户实际感知的
	// 接入线路，因此手动配置优先。
	Profile Profile `json:"profile"`

	// LastSessionID 是最近一次扫描会话的 ID（可能为空）。
	//
	// 它让 `scan --resume` 不必每次手工指定会话 ID。
	// 记在身份文件里而不是只存在数据库里是刻意的：
	// 身份文件与数据库是同一条本地数据链，一起备份、一起删除。
	//
	// 它不承载任何可识别信息，只是"我上次跑到哪一次扫描"。
	LastSessionID string `json:"last_session_id,omitempty"`
}

// Profile 是持久化的采集者画像。
//
// 与 model.CollectorProfile 分开：这里要带 JSON 标签（它是文件格式），
// 而 model 层的类型刻意不带任何序列化标签，避免公开数据格式被
// 结构体定义悄悄决定。
type Profile struct {
	Country   string `json:"country"`
	Province  string `json:"province"`
	City      string `json:"city"`
	ISP       string `json:"isp"`
	ASN       string `json:"asn"`
	IPVersion string `json:"ip_version"`
}

// ToModel 把持久化画像转成业务模型。
func (p Profile) ToModel() model.CollectorProfile {
	return model.CollectorProfile{
		Country:   p.Country,
		Province:  p.Province,
		City:      p.City,
		ISP:       p.ISP,
		ASN:       p.ASN,
		IPVersion: model.IPVersion(p.IPVersion),
	}
}

// FromModel 把业务模型转成持久化画像。
func FromModel(profile model.CollectorProfile) Profile {
	profile.Normalize()
	return Profile{
		Country:   profile.Country,
		Province:  profile.Province,
		City:      profile.City,
		ISP:       profile.ISP,
		ASN:       profile.ASN,
		IPVersion: string(profile.IPVersion),
	}
}

// Load 读取本地标识；文件不存在时**创建**一个新的并写回。
//
// 返回的 Local 里 CollectorID 一定非空且合法。
// 自动创建是刻意的：用户第一次运行不该被要求先手工生成 ID。
//
// 文件损坏时返回错误而不是静默重建——静默重建会让同一个节点
// 在数据里变成两个节点，而用户完全不会察觉。
func Load(path string) (*Local, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = DefaultPath
	}

	blob, err := os.ReadFile(path)
	switch {
	case err == nil:
		return decode(path, blob)
	case errors.Is(err, os.ErrNotExist):
		return create(path)
	default:
		return nil, fmt.Errorf("read identity file %q: %w", path, err)
	}
}

// decode 解析身份文件并校验其中的 ID。
func decode(path string, blob []byte) (*Local, error) {
	var local Local
	if err := json.Unmarshal(blob, &local); err != nil {
		return nil, fmt.Errorf("identity file %q is not valid json: %w (删除该文件会生成新的匿名标识)", path, err)
	}

	local.CollectorID = strings.TrimSpace(local.CollectorID)
	if !model.ValidCollectorID(local.CollectorID) {
		return nil, fmt.Errorf("identity file %q has an invalid collector_id %q (删除该文件会生成新的匿名标识)",
			path, local.CollectorID)
	}
	local.LastSessionID = strings.TrimSpace(local.LastSessionID)
	return &local, nil
}

// SetLastSession 记住最近一次扫描会话。
//
// 它只是给 `--resume` 提供一个默认值，因此写失败不应影响扫描本身：
// 调用方可以选择忽略错误，但这里仍然如实返回，让调用方决定。
func SetLastSession(path, sessionID string) error {
	local, err := Load(path)
	if err != nil {
		return err
	}
	if local.LastSessionID == sessionID {
		return nil
	}
	local.LastSessionID = sessionID
	return Save(path, local)
}

// create 生成新的身份文件。
func create(path string) (*Local, error) {
	id, err := model.NewCollectorID()
	if err != nil {
		return nil, err
	}

	local := &Local{CollectorID: id}
	if err := Save(path, local); err != nil {
		return nil, err
	}
	return local, nil
}

// Save 原子地写入身份文件。
//
// 原子性是必要的：这个文件一旦写坏，用户就丢失了历史数据的关联，
// 而且只能通过删除文件换一个新 ID 来恢复。
func Save(path string, local *Local) error {
	if local == nil {
		return errors.New("identity: nil local")
	}
	if !model.ValidCollectorID(local.CollectorID) {
		return fmt.Errorf("identity: refusing to save invalid collector_id %q", local.CollectorID)
	}

	// 归一化后再保存，避免把 " cn " 这种形式写进文件。
	local.Profile = FromModel(local.Profile.ToModel())

	blob, err := json.MarshalIndent(local, "", "  ")
	if err != nil {
		return fmt.Errorf("encode identity: %w", err)
	}
	blob = append(blob, '\n')

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create identity dir %q: %w", dir, err)
		}
	}

	tmp, err := os.CreateTemp(dir, ".collector-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp identity file: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(blob); err != nil {
		cleanup()
		return fmt.Errorf("write temp identity: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp identity: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp identity: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace identity file %q: %w", path, err)
	}
	return nil
}
