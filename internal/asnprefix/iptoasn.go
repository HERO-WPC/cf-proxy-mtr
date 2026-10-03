package asnprefix

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// 本文件负责"一次下载拿到全部线路的前缀"。
//
// == 为什么不是"每个 ASN 查一次" ==
//
// 最初用的是 RouteViews 的 `asn/<n>?af=4` 接口：每个 ASN 一个请求，
// 30 个 ASN 就是 60 次。它**匿名可用但配额有限**——实测连续跑几轮
// 之后开始返回：
//
//	http 429: Please register for a token: https://api.routeviews.org/docs/#access
//
// 于是"无限、无需账号"这个前提不成立：抓不到的 ASN 会让线路名
// 静默缺失，而使用者只看到"线路名少了几条"。
//
// iptoasn.com 提供**一张全表**（前缀区间 → ASN），公有领域，
// 不需要账号，一次 HTTP GET 就拿到所有 ASN 的段：
//
//	1.71.103.0	1.71.103.255	4809	CN	CHINATELECOM-...-CN2
//	103.11.109.0	103.11.109.255	58453	HK	CMI-INT-HK ...
//
// 于是请求数从 60 降到 1，而且下载下来的是**完整快照**：
// 要么整份都有、要么整份都没有，不会出现"30 个里成功 18 个"
// 这种半成品状态。

// DefaultBulkURL 是 iptoasn.com 的全量映射表（gzip 压缩的 TSV）。
//
// 选它的原因：公有领域、不需要账号、单次下载即完整快照。
const DefaultBulkURL = "https://iptoasn.com/data/ip2asn-combined.tsv.gz"

// bulkMaxBytes 限制下载体积。
//
// 实测约 8.6 MB（解压后约 30 MB），给 64 MB 的余量足够，
// 又能挡住"对端返回了一个 HTML 错误页被当成数据"这类情况。
const bulkMaxBytes = 64 << 20

// fetchBulk 下载全表并抽取出我们关心的那些 ASN 的前缀。
//
// 只保留 wanted 里出现的 ASN：一张表里有几十万个 ASN，
// 而我们只认 asnmap 那 30 条线路，没必要把它们全部留在内存里。
func (r *Resolver) fetchBulk(ctx context.Context, wanted map[string]bool) (map[string][]string, error) {
	url := r.opts.BulkURL
	if strings.TrimSpace(url) == "" {
		url = DefaultBulkURL
	}

	var lastErr error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if attempt > 0 {
			delay := fetchBackoff << uint(attempt-1)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		result, err := r.fetchBulkOnce(ctx, url, wanted)
		if err == nil {
			return result, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("after %d attempts: %w", fetchAttempts, lastErr)
}

// fetchBulkOnce 下载并解析一次。
func (r *Resolver) fetchBulkOnce(ctx context.Context, url string, wanted map[string]bool) (map[string][]string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, r.opts.BulkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	// 数据是 gzip 的。用流式解压 + 逐行读取：整份解压后
	// 有几十 MB，没必要全部读进内存再解析。
	gz, err := gzip.NewReader(io.LimitReader(resp.Body, bulkMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	return parseBulk(gz, wanted)
}

// parseBulk 逐行解析 TSV，收集 wanted 里那些 ASN 的前缀。
//
// 行格式（制表符分隔）：
//
//	range_start  range_end  asn  country  description
//
// asn 为 0 表示"未路由"，直接跳过。
func parseBulk(reader io.Reader, wanted map[string]bool) (map[string][]string, error) {
	out := make(map[string][]string, len(wanted))
	scanner := bufio.NewScanner(reader)

	// 单行最长可能很长（描述里带组织全名），把缓冲调大。
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			// 坏行不该让整份数据作废：跳过它，
			// 因为其余几十万行仍然有用。
			continue
		}

		asnKey := normalizeASN(fields[2])
		if asnKey == "" || !wanted[asnKey] {
			continue
		}

		start, startErr := netip.ParseAddr(strings.TrimSpace(fields[0]))
		end, endErr := netip.ParseAddr(strings.TrimSpace(fields[1]))
		if startErr != nil || endErr != nil {
			continue
		}

		out[asnKey] = append(out[asnKey], rangeToPrefixes(start, end)...)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read bulk data: %w", err)
	}
	return out, nil
}

// normalizeASN 把表里的 ASN 字段变成 "AS<number>" 形式。
//
// 表里是裸数字（"4809"），而本项目的其余部分用 "AS4809"。
// 0 与非法值返回空串（表示"未路由"，调用方会跳过）。
func normalizeASN(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	number, err := strconv.Atoi(trimmed)
	if err != nil || number <= 0 {
		return ""
	}
	return "AS" + trimmed
}

// rangeToPrefixes 把一个地址区间转成一个或多个 CIDR。
//
// == 为什么要转换 ==
//
// 全量表给的是区间（起始地址、结束地址），而项目的其余部分、
// 以及磁盘上的缓存，都用 CIDR 表示。转换让缓存保持人类可读
// （"1.71.103.0/24" 一眼能看出是什么），排查时省事。
//
// == 算法（标准的区间转 CIDR）==
//
// 从区间起点开始重复：
//
//	block = min(起点末尾连续零比特数, floor(log2(剩余地址数)))
//	输出 起点/block
//	起点 += 2^block
//
// "起点末尾连续零比特数"决定这个地址最大能对齐到多大的块，
// "剩余地址数"保证不越过区间末尾；两者取小。
//
// 实现用 big.Int 而不是自己移位：IPv6 是 128 位，手写 hi/lo
// 的进位与位数计算容易出错，而**错了不会报错**——只会让某段
// IP 悄悄落在集合外，表现成"线路名偶尔少一条"。
// 这里一次解析要处理几万个区间，big.Int 的开销完全可以接受，
// 而且结果会被缓存，不是每次运行都算。
func rangeToPrefixes(start, end netip.Addr) []string {
	if !start.IsValid() || !end.IsValid() || start.Is4() != end.Is4() {
		return nil
	}
	if end.Less(start) {
		return nil
	}

	bits := 128
	if start.Is4() {
		bits = 32
	}

	cur := addrToInt(start)
	last := addrToInt(end)

	out := make([]string, 0, 4)
	one := big.NewInt(1)

	for {
		// 起点允许的最大对齐宽度。
		align := bits
		if cur.Sign() != 0 {
			align = trailingZeroBits(cur, bits)
		}

		// 剩余地址数（含两端）允许的最大块。
		remaining := new(big.Int).Sub(last, cur)
		remaining.Add(remaining, one)
		remainingBlock := remaining.BitLen() - 1 // floor(log2(remaining))

		block := align
		if remainingBlock < block {
			block = remainingBlock
		}
		if block < 0 {
			block = 0
		}

		prefix := netip.PrefixFrom(intToAddr(cur, bits), bits-block)
		out = append(out, prefix.String())

		// 这一块覆盖 [cur, cur+2^block-1]。若已到区间末尾则结束。
		step := new(big.Int).Lsh(one, uint(block))
		next := new(big.Int).Add(cur, step)
		if next.Cmp(last) > 0 {
			break
		}
		cur = next
	}

	return out
}

// addrToInt 把 IP 转成整数（IPv4 用 32 位，IPv6 用 128 位）。
func addrToInt(addr netip.Addr) *big.Int {
	if addr.Is4() {
		b := addr.As4()
		return new(big.Int).SetBytes(b[:])
	}
	b := addr.As16()
	return new(big.Int).SetBytes(b[:])
}

// intToAddr 把整数还原成 IP。
func intToAddr(value *big.Int, bits int) netip.Addr {
	size := bits / 8
	raw := value.Bytes()

	buf := make([]byte, size)
	copy(buf[size-len(raw):], raw)

	if bits == 32 {
		var b [4]byte
		copy(b[:], buf)
		return netip.AddrFrom4(b)
	}
	var b [16]byte
	copy(b[:], buf)
	return netip.AddrFrom16(b)
}

// trailingZeroBits 返回 x 的末尾连续零比特数，上限为 bits。
//
// x 为 0 时返回 bits（"整个地址空间"）。
func trailingZeroBits(x *big.Int, bits int) int {
	count := 0
	for count < bits && x.Bit(count) == 0 {
		count++
	}
	return count
}
