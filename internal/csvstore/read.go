package csvstore

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 本文件是 CSV 的**读取**侧。
//
// 为什么写在这里而不是各调用方自己解析：列的顺序与含义是本包的
// 契约（见 Header）。如果界面、按国家选目标、导出各写一份解析，
// 加一列就会有三处一起错，而且错法还不一样。
//
// 读取刻意**按列名定位**而不是按下标：老文件没有 cca2 列，
// 按下标读会把它的缺失错位成别的字段。

// 读取相关的错误。
var (
	// ErrEmptyFile 表示文件不存在或没有内容。
	ErrEmptyFile = errors.New("csvstore: file is empty")

	// ErrNoHeader 表示第一行不是表头。
	ErrNoHeader = errors.New("csvstore: missing header row")
)

// ReadAll 读回一个 CSV 文件里的全部行。
//
// 对**缺列**宽容：老文件（没有 cca2）照样能读，缺失的字段留零值。
// 对**坏行**也宽容：一行解析不了就跳过它，而不是让整个文件读不出来——
// 一份 1.5 万行的测量结果不该因为最后一行被写坏就完全打不开。
// 跳过的行数通过 error 之外的方式无法得知，因此这里只在完全读不到时
// 才返回错误。
func ReadAll(path string) ([]Row, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	reader := csv.NewReader(file)
	// 列数可以不一致（老文件少一列），因此不强制字段数。
	reader.FieldsPerRecord = -1
	// 单行可能很长（hops 明细），放宽缓冲。
	reader.ReuseRecord = false

	header, err := reader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrEmptyFile
		}
		return nil, err
	}
	index := indexColumns(header)
	if _, ok := index["target"]; !ok {
		return nil, fmt.Errorf("%w: first row is %v", ErrNoHeader, header)
	}

	var rows []Row
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// 坏行：跳过。CSV 是流式追加写的，进程被杀时最后一行
			// 可能只写了一半，而那不该让整份结果读不出来。
			var parseErr *csv.ParseError
			if errors.As(err, &parseErr) {
				continue
			}
			return rows, err
		}
		rows = append(rows, rowFromRecord(record, index))
	}

	return rows, nil
}

// indexColumns 把表头映射成"列名 -> 下标"。
//
// 容忍 BOM 与首尾空白：文件可能是别的工具（或 Excel）另存过的。
func indexColumns(header []string) map[string]int {
	index := make(map[string]int, len(header))
	for i, name := range header {
		name = strings.TrimSpace(strings.TrimPrefix(name, "\ufeff"))
		if name == "" {
			continue
		}
		index[name] = i
	}
	return index
}

// rowFromRecord 按列名把一条记录转成 Row。
func rowFromRecord(record []string, index map[string]int) Row {
	at := func(name string) string {
		i, ok := index[name]
		if !ok || i >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[i])
	}

	row := Row{
		Target:        at("target"),
		IP:            at("ip"),
		ErrorType:     at("error_type"),
		ErrorMessage:  at("error_message"),
		ASPath:        at("as_path"),
		Hops:          at("hops"),
		ClientVersion: at("client_version"),
		CCA2:          strings.ToUpper(at("cca2")),
	}

	if raw := at("timestamp_utc"); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			row.Timestamp = parsed.UTC()
		}
	}

	if raw := at("port"); raw != "" {
		if port, err := strconv.Atoi(raw); err == nil {
			row.Port = port
		}
	}

	row.Success = strings.EqualFold(at("success"), "true")

	if raw := at("latency_ms"); raw != "" {
		if ms, err := strconv.ParseFloat(raw, 64); err == nil && ms > 0 {
			row.LatencyMS = ms
		}
	}

	if raw := at("hop_count"); raw != "" {
		if count, err := strconv.Atoi(raw); err == nil {
			row.HopCount = count
		}
	}

	return row
}

// SortBy 是排序依据。
type SortBy string

const (
	// SortLatency 按延迟升序，没测到的排最后。
	SortLatency SortBy = "latency"

	// SortRoute 按线路分组：同线路相邻，组间按组内最快延迟排。
	SortRoute SortBy = "route"

	// SortTarget 按目标名，便于查找特定 IP。
	SortTarget SortBy = "target"
)

// Sort 按依据排序（原地）。
//
// 三种排序都把"没有数据的行"放到最后：
//
//   - 按延迟：没测到的（超时/失败）排在所有有延迟的后面，因为
//     "多快"这个问题它们没有答案，混在快的那批里会让人误以为可用。
//   - 按线路：没有线路信息的排在最后，同理。
//   - 按目标：纯字典序，没有"缺失"的概念。
func Sort(rows []Row, by SortBy) {
	switch by {
	case SortRoute:
		// 组内最快延迟**先算一次**。
		//
		// 原来的写法是在比较函数里现算（每次比较都扫一遍全表），
		// 那是 O(n² log n)：两千行就是几千万次扫描。这里顺手改掉，
		// 因为下面新增的优先线路判断会让比较次数再涨。
		bestLatency := make(map[string]float64, len(rows))
		for _, row := range rows {
			group := routeGroupKey(row)
			if group == "" || row.LatencyMS <= 0 {
				continue
			}
			if current, ok := bestLatency[group]; !ok || row.LatencyMS < current {
				bestLatency[group] = row.LatencyMS
			}
		}

		sort.SliceStable(rows, func(i, j int) bool {
			left, right := rows[i], rows[j]

			// 优先线路（走 CMIN2 / CN2 / 9929 的那些）置顶，
			// 次序按 premiumRank 的定义。
			//
			// 放在"有没有线路信息"之前判断：优先线路本身就是有线路的，
			// 两种规则不会互相干扰，先判它更直观。
			leftRank, rightRank := premiumRank(left), premiumRank(right)
			if leftRank != rightRank {
				if leftRank < 0 {
					return false
				}
				if rightRank < 0 {
					return true
				}
				return leftRank < rightRank
			}

			leftGroup, rightGroup := routeGroupKey(left), routeGroupKey(right)

			// 没有线路信息的排最后。
			if (leftGroup == "") != (rightGroup == "") {
				return rightGroup == ""
			}
			if leftGroup != rightGroup {
				// 组间按组内最快延迟排，而不是按组名字典序：
				// 使用者关心的是"哪条线路更快"。
				return bestLatency[leftGroup] < bestLatency[rightGroup]
			}
			return latencyLess(left, right)
		})

	case SortTarget:
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Target < rows[j].Target })

	default: // SortLatency
		sort.SliceStable(rows, func(i, j int) bool { return latencyLess(rows[i], rows[j]) })
	}
}

// premiumRouteGroups 是"优先线路"，按优先级从高到低排列。
//
// 名字必须与 asnmap 里的线路名**逐字一致**：CSV 的 as_path 写的就是
// 那些短名（例如 "CMNET > CMI > CN2"），匹配靠的是切分后的整段相等，
// 而不是子串包含——子串会让 CN2 命中 CMIN2 之类的东西（本例正好不会，
// 但换个名字就会），那种错误只会表现为"排序看起来不太对"，很难查。
//
// 每组的多个写法是别名：同一个 ASN 在不同数据里可能只写编号
// （例如 "9929" 与 "9929/CUII"）。
var premiumRouteGroups = [][]string{
	{"CMIN2"},
	{"CN2"},
	{"9929/CUII", "9929"},
}

// premiumRank 返回该行所属的优先线路等级，-1 表示不是优先线路。
//
// 一条路径可能同时经过多条优先线路（例如 "CN2 > CMIN2"），
// 这时取**等级最高**的那个——它确实走了那条更好的线路。
func premiumRank(row Row) int {
	segments := routeSegments(row.ASPath)
	if len(segments) == 0 {
		return -1
	}

	for rank, names := range premiumRouteGroups {
		for _, segment := range segments {
			for _, name := range names {
				if strings.EqualFold(segment, name) {
					return rank
				}
			}
		}
	}
	return -1
}

// routeSegments 把 as_path 拆成线路名。
//
// 分隔符是 " > "（写入时就是 ShortPath 拼的），这里按分隔符切开并
// 去掉首尾空白。刻意不按单个字符切：线路名里本来就可能带 "/"（9929/CUII）。
func routeSegments(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}

	parts := strings.Split(path, ">")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// latencyLess 是"按延迟升序、没测到的最后"的单一实现点。
func latencyLess(left, right Row) bool {
	leftKnown := left.LatencyMS > 0
	rightKnown := right.LatencyMS > 0
	if leftKnown != rightKnown {
		return leftKnown
	}
	if !leftKnown {
		// 都没测到：用目标名保证顺序稳定，否则每次刷新表格
		// 的行序都可能不同，看起来像数据在跳。
		return left.Target < right.Target
	}
	if left.LatencyMS != right.LatencyMS {
		return left.LatencyMS < right.LatencyMS
	}
	return left.Target < right.Target
}

// routeGroupKey 是线路分组用的键。
//
// 空串表示"没有线路信息"（没跟踪，或跟踪失败）。
func routeGroupKey(row Row) string {
	return strings.TrimSpace(row.ASPath)
}

// Countries 返回行里出现过的国家及计数，按数量倒序。
//
// 供界面做国家筛选：数字直接来自**这份结果**，因此不会出现
// "下拉框说 1388 个、实际筛出 1580 个"那种对不上的情况。
func Countries(rows []Row) []CountryCount {
	counts := make(map[string]int)
	for _, row := range rows {
		code := strings.ToUpper(strings.TrimSpace(row.CCA2))
		if code == "" {
			continue
		}
		counts[code]++
	}

	out := make([]CountryCount, 0, len(counts))
	for code, count := range counts {
		out = append(out, CountryCount{CCA2: code, Count: count})
	}
	// 数量相同的按国家码排，保证顺序稳定。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].CCA2 < out[j].CCA2
	})
	return out
}

// CountryCount 是一个国家及其目标数。
type CountryCount struct {
	CCA2  string `json:"cca2"`
	Count int    `json:"count"`
}

// CollapseByTarget 把同一个目标的多个行合并成一行。
//
// == 为什么需要 ==
//
// 一个目标在 CSV 里会有多行：探测写一行（有延迟、没有线路），
// 跟踪再写一行（有线路、通常没有延迟）。这是**流式追加**的必然结果，
// 对文件来说是好事——每次测量都留了痕；但直接展示会让人看到
// 同一条目标出现两次，其中一次"线路是空的"，看起来像跟踪失败了。
//
// 合并规则（每一列都取"有信息的那一份"）：
//
//   - 延迟：取最小正值。一个目标测了多次时，使用者想知道的是它有多快，
//     而不是最后一次碰巧多慢。
//   - 线路 / 跳数：取第一个非空值。线路来自跟踪行，探测行没有。
//   - 时间：取最新的一次，让"这是什么时候测的"反映最后那次动作。
//   - 成功：任一行成功即成功。探测成功但跟踪失败，目标本身是通的。
//   - 失败原因：只在**没有任何一行成功**时才留，否则会在一行成功的
//     结果旁边挂一句失败原因，自相矛盾。
//
// CSV 文件本身不变：这是展示层的视图，原始记录一行不少。
func CollapseByTarget(rows []Row) []Row {
	order := make([]string, 0, len(rows))
	merged := make(map[string]Row, len(rows))

	for _, row := range rows {
		key := row.Target
		if key == "" {
			key = row.IP
		}
		if key == "" {
			continue
		}

		existing, seen := merged[key]
		if !seen {
			order = append(order, key)
			merged[key] = row
			continue
		}

		// 延迟取最快的一次。
		if row.LatencyMS > 0 && (existing.LatencyMS <= 0 || row.LatencyMS < existing.LatencyMS) {
			existing.LatencyMS = row.LatencyMS
		}
		// 线路信息取第一个有内容的。
		if existing.ASPath == "" {
			existing.ASPath = row.ASPath
		}
		if existing.Hops == "" {
			existing.Hops = row.Hops
		}
		if existing.HopCount == 0 {
			existing.HopCount = row.HopCount
		}
		if existing.CCA2 == "" {
			existing.CCA2 = row.CCA2
		}
		if existing.IP == "" {
			existing.IP = row.IP
		}
		if existing.Port == 0 {
			existing.Port = row.Port
		}
		// 时间取最新。
		if row.Timestamp.After(existing.Timestamp) {
			existing.Timestamp = row.Timestamp
		}
		// 任一行成功即成功。
		existing.Success = existing.Success || row.Success

		merged[key] = existing
	}

	out := make([]Row, 0, len(order))
	for _, key := range order {
		row := merged[key]
		// 只在彻底没成功过的时候留下失败原因：一行成功的结果旁边
		// 挂一句失败原因会自相矛盾。
		if row.Success {
			row.ErrorType = ""
			row.ErrorMessage = ""
		}
		out = append(out, row)
	}
	return out
}
