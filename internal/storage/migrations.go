package storage

// migration 是一次版本化的结构变更。
//
// 约定：
//
//   - Statements 按顺序在**同一个事务**里执行；
//   - 迁移一旦发布就必须保持内容不变（否则不同机器上同一版本号
//     可能对应不同结构）。需要调整结构时新增一个版本；
//   - 不做破坏性变更（DROP COLUMN / DROP TABLE）：历史数据
//     是本项目的核心资产，宁可留下不再使用的列；
//   - 注释里写清"为什么这样设计"，因为列的含义往往比列本身重要。
type migration struct {
	// Version 是递增的版本号，从 1 开始。
	Version int

	// Comment 是一句话说明，会写入 schema_migrations 便于排查。
	Comment string

	// Statements 是要执行的 SQL。
	Statements []string
}

// migrations 是全部迁移，按 Version 升序。
//
// 新增迁移时追加到末尾，绝不修改已有的项。
var migrations = []migration{
	{
		Version: 1,
		Comment: "core tables: targets, collectors, scan_sessions, measurements, traces",
		Statements: []string{
			// -----------------------------------------------------------------
			// targets：测量目标（IP × Port）
			// -----------------------------------------------------------------
			//
			// id 采用 "1.2.3.4:443" / "[2001:db8::1]:443" 形式，
			// 与 model.Target.ID 完全一致，因此不需要额外的映射表。
			//
			// source_* 是 all.json 对该目标 IP 的元数据（目标自身的位置），
			// 与采集者的位置毫无关系。两套字段分开存放，避免混淆。
			//
			// colo_* 是 Cloudflare 接入点信息：实测约 19% 的目标上
			// source_country 与 colo_cca2 不同，因此必须分列保存。
			//
			// first_seen / last_seen 让"这个目标是什么时候出现在数据源里的"
			// 可追溯，也便于发现上游下架的目标。
			`CREATE TABLE targets (
				id                TEXT    PRIMARY KEY,
				ip                TEXT    NOT NULL,
				port              INTEGER NOT NULL,
				ip_version        TEXT    NOT NULL DEFAULT 'unknown',

				source_country    TEXT    NOT NULL DEFAULT '',
				source_cca2       TEXT    NOT NULL DEFAULT '',
				source_region     TEXT    NOT NULL DEFAULT '',
				source_city       TEXT    NOT NULL DEFAULT '',
				source_latitude   REAL,
				source_longitude  REAL,
				source_country_en TEXT    NOT NULL DEFAULT '',

				colo_iata         TEXT    NOT NULL DEFAULT '',
				colo_cca2         TEXT    NOT NULL DEFAULT '',
				colo_region       TEXT    NOT NULL DEFAULT '',
				colo_city         TEXT    NOT NULL DEFAULT '',
				colo_latitude     REAL,
				colo_longitude    REAL,

				first_seen        INTEGER NOT NULL,
				last_seen         INTEGER NOT NULL,

				CHECK (port BETWEEN 1 AND 65535),
				CHECK (ip_version IN ('ipv4','ipv6','unknown'))
			)`,

			// 按国家/地区筛选目标（例如"只看日本的目标"）会用到。
			`CREATE INDEX idx_targets_source_cca2 ON targets (source_cca2)`,
			`CREATE INDEX idx_targets_ip_version ON targets (ip_version)`,

			// -----------------------------------------------------------------
			// collectors：匿名测量者
			// -----------------------------------------------------------------
			//
			// 只允许粗粒度的地区 / 运营商信息。
			//
			// 刻意**没有**这些列（需求第 14 条）：
			//   public_ip, local_ip, mac, hostname, device_id, 精确经纬度
			// 任何"能定位到具体设备或人"的信息都不入库。
			//
			// collector_id 由 crypto/rand 生成，不由硬件信息推导，
			// 因此它只能关联同一个匿名节点的历史数据，无法反向识别。
			`CREATE TABLE collectors (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				collector_id  TEXT NOT NULL UNIQUE,
				country       TEXT NOT NULL DEFAULT '',
				province      TEXT NOT NULL DEFAULT '',
				city          TEXT NOT NULL DEFAULT '',
				isp           TEXT NOT NULL DEFAULT '',
				asn           TEXT NOT NULL DEFAULT '',
				ip_version    TEXT NOT NULL DEFAULT '',
				client_version TEXT NOT NULL DEFAULT '',
				created_at    INTEGER NOT NULL,
				updated_at    INTEGER NOT NULL,
				CHECK (ip_version IN ('','ipv4','ipv6','unknown'))
			)`,

			`CREATE INDEX idx_collectors_group ON collectors (country, province, city, isp, asn)`,

			// -----------------------------------------------------------------
			// scan_sessions：一次测量会话
			// -----------------------------------------------------------------
			//
			// 为什么必须有它（需求第 20、21 条）：
			// 判断"某个目标是否已经测过"不能只看数据库里有没有历史结果，
			// 否则同一个采集者永远无法重新测量全部目标。
			// 正确判据是 (target, collector, session) 三元组，
			// 所以"2026-10-03 10:00 的完整扫描"与"20:00 的完整扫描"
			// 必须是两个不同的 session。
			//
			// finished_at 为空表示会话未结束：这正是断点续测的入口。
			`CREATE TABLE scan_sessions (
				id              TEXT    PRIMARY KEY,
				collector_id    INTEGER NOT NULL REFERENCES collectors(id) ON DELETE CASCADE,
				started_at      INTEGER NOT NULL,
				finished_at     INTEGER,
				target_count    INTEGER NOT NULL DEFAULT 0,
				completed_count INTEGER NOT NULL DEFAULT 0,
				client_version  TEXT    NOT NULL DEFAULT '',
				created_at      INTEGER NOT NULL
			)`,

			`CREATE INDEX idx_sessions_collector ON scan_sessions (collector_id, started_at DESC)`,
			`CREATE INDEX idx_sessions_open ON scan_sessions (finished_at)`,

			// -----------------------------------------------------------------
			// measurements：TCP 探测结果（时间序列，只追加）
			// -----------------------------------------------------------------
			//
			// 这张表**只追加**，永远不 UPDATE 既有行：
			// "10:00 是 42ms、20:00 是 86ms"必须同时保留，
			// 否则后续无法回答"什么时候变差"。
			//
			// latency_us 用整数微秒存延迟，而不是浮点毫秒：
			// 避免浮点累积误差，也让"同一测量在不同格式下比较"更可靠。
			// 读出时再换算成毫秒。
			//
			// 失败结果同样入库（error_type != ''）：失败也是线路信息，
			// 例如"Sample Province移动对该目标成功率 12%"本身就有价值。
			//
			// dedup_key 是 (collector,session,target,timestamp) 的内容哈希，
			// 用于让"同一个 batch 被重复导入"变成幂等操作而不会让统计翻倍。
			//
			// schema_version / client_version 随每条记录保存：
			// 公开数据必须能追溯到"哪个版本的程序、按哪个数据格式产生的"。
			`CREATE TABLE measurements (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				target_id      TEXT    NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
				collector_id   INTEGER NOT NULL REFERENCES collectors(id) ON DELETE CASCADE,
				session_id     TEXT REFERENCES scan_sessions(id) ON DELETE SET NULL,

				measured_at    INTEGER NOT NULL,
				success        INTEGER NOT NULL,
				latency_us     INTEGER NOT NULL DEFAULT 0,
				error_type     TEXT    NOT NULL DEFAULT '',
				error_message  TEXT    NOT NULL DEFAULT '',

				schema_version INTEGER NOT NULL DEFAULT 1,
				client_version TEXT    NOT NULL DEFAULT '',

				dedup_key      TEXT    NOT NULL,
				created_at     INTEGER NOT NULL,

				CHECK (success IN (0,1)),
				CHECK (success = 1 OR error_type <> '')
			)`,

			// 时间序列查询的主力索引：按目标 + 分组 + 时间取样本。
			`CREATE INDEX idx_measurements_target_time ON measurements (target_id, measured_at DESC)`,
			`CREATE INDEX idx_measurements_collector_time ON measurements (collector_id, measured_at DESC)`,
			`CREATE INDEX idx_measurements_session ON measurements (session_id)`,
			// 去重：同一个 dedup_key 只允许存在一条。
			// 用 UNIQUE 而不是"先查后插"，因为后者在并发下有竞态。
			`CREATE UNIQUE INDEX idx_measurements_dedup ON measurements (dedup_key)`,

			// -----------------------------------------------------------------
			// traces：线路跟踪（时间序列，只追加）
			// -----------------------------------------------------------------
			//
			// trace_json 保存的是**归一化后**的内部 Trace 结构
			// （internal/trace 的 TraceResult），不是 NextTrace 的原始 JSON，
			// 因此上游输出格式变化不会波及本表。
			//
			// raw_json 保存清洗后的原始输出，仅用于诊断，
			// 业务逻辑不依赖它。允许为空。
			//
			// port 是本次跟踪使用的端口：项目测的是 IP:Port，
			// 把端口固定成 443 会让这条记录失去意义。
			//
			// local_filtered 记录"上传前是否过滤过本地地址"，
			// 让隐私过滤这件事可审计（本地库可存完整路径，
			// 公开数据必须是过滤后的）。
			`CREATE TABLE traces (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				target_id      TEXT    NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
				collector_id   INTEGER NOT NULL REFERENCES collectors(id) ON DELETE CASCADE,
				session_id     TEXT REFERENCES scan_sessions(id) ON DELETE SET NULL,

				traced_at      INTEGER NOT NULL,
				engine         TEXT    NOT NULL DEFAULT '',
				engine_version TEXT    NOT NULL DEFAULT '',
				mode           TEXT    NOT NULL DEFAULT '',
				protocol       TEXT    NOT NULL DEFAULT '',
				port           INTEGER NOT NULL,

				success        INTEGER NOT NULL,
				duration_us    INTEGER NOT NULL DEFAULT 0,
				hop_count      INTEGER NOT NULL DEFAULT 0,
				trace_json     TEXT    NOT NULL DEFAULT '',
				raw_json       TEXT    NOT NULL DEFAULT '',
				error_type     TEXT    NOT NULL DEFAULT '',
				error_message  TEXT    NOT NULL DEFAULT '',

				local_filtered INTEGER NOT NULL DEFAULT 0,
				schema_version INTEGER NOT NULL DEFAULT 1,
				client_version TEXT    NOT NULL DEFAULT '',

				dedup_key      TEXT    NOT NULL,
				created_at     INTEGER NOT NULL,

				CHECK (success IN (0,1)),
				CHECK (port BETWEEN 1 AND 65535),
				CHECK (local_filtered IN (0,1)),
				CHECK (success = 1 OR error_type <> '')
			)`,

			`CREATE INDEX idx_traces_target_time ON traces (target_id, traced_at DESC)`,
			`CREATE INDEX idx_traces_collector_time ON traces (collector_id, traced_at DESC)`,
			`CREATE INDEX idx_traces_session ON traces (session_id)`,
			`CREATE UNIQUE INDEX idx_traces_dedup ON traces (dedup_key)`,
		},
	},
}
