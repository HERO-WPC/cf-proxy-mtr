package query

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/aggregate"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
)

// 本文件把两个数据源适配成同一套 Row 流：
//
//	SQLite  -> 本地历史（可以用 --since/--until/--session 收窄）
//	JSONL   -> 公开数据（本地导出物，或从别处下载的批次）
//
// 两条路径共用同一套索引与统计逻辑，因此"查数据库得到的数字"
// 与"聚合 JSONL 得到的数字"必须一致——这一点由测试守着。

// LoadFromStore 从本地数据库载入。
//
// 用 storage 的导出读取（StreamMeasurements / StreamTraces）而不是
// QueryMeasurements：前者带出了目标元数据与 Collector 画像，
// 而线路画像正需要按地区/运营商分组。
func LoadFromStore(ctx context.Context, store *storage.Store, filter storage.ExportFilter, opts IndexOptions) (*Dataset, error) {
	if store == nil {
		return nil, fmt.Errorf("query: nil store")
	}

	dataset := NewDataset()

	// 测量。
	if _, err := store.StreamMeasurements(ctx, filter, func(item storage.ExportMeasurement) error {
		dataset.IndexRows([]Row{measurementRow(item)}, opts)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("load measurements: %w", err)
	}

	// 跟踪。
	if _, err := store.StreamTraces(ctx, filter, func(item storage.ExportTrace) error {
		dataset.IndexRows([]Row{traceRow(item)}, opts)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("load traces: %w", err)
	}

	return dataset, nil
}

// measurementRow 把数据库行转成索引行。
//
// 与 export 包各自做一份转换是刻意的：
//
//	export  产出**公开数据**，因此要过隐私过滤、字段名即公开 Schema；
//	query   只是本地分析，需要保留原始地址（内网目标也要能查），
//	        而且不该因为隐私过滤而丢掉本地测试数据。
func measurementRow(item storage.ExportMeasurement) Row {
	row := Row{
		SchemaVersion: 1,
		Kind:          "measurement",
		ClientVersion: item.ClientVersion,
		TargetID:      item.TargetID,
		IP:            item.IP,
		Port:          item.Port,
		TimestampUTC:  item.Timestamp.UTC().Format(time.RFC3339Nano),
		SessionID:     item.SessionID,
		CollectorID:   item.CollectorAID,
	}
	row.Collector = &aggregate.CollectorRef{
		Country: item.CollectorCountry, Province: item.CollectorProvince,
		City: item.CollectorCity, ISP: item.CollectorISP,
		ASN: item.CollectorASN, IPVersion: item.CollectorIPVersion,
	}
	row.TargetMeta = targetMetaRef(item)
	row.Measurement = &aggregate.MeasurementRef{
		Success: item.Success, LatencyMS: item.LatencyMS,
		ErrorType: item.ErrorType, ErrorMessage: item.ErrorMessage,
	}
	return row
}

// traceRow 把数据库跟踪行转成索引行。
func traceRow(item storage.ExportTrace) Row {
	row := Row{
		SchemaVersion: 1,
		Kind:          "trace",
		ClientVersion: item.ClientVersion,
		TargetID:      item.TargetID,
		IP:            item.IP,
		Port:          item.Port,
		TimestampUTC:  item.Timestamp.UTC().Format(time.RFC3339Nano),
		SessionID:     item.SessionID,
		CollectorID:   item.CollectorAID,
	}
	row.Collector = &aggregate.CollectorRef{
		Country: item.CollectorCountry, Province: item.CollectorProvince,
		City: item.CollectorCity, ISP: item.CollectorISP,
		ASN: item.CollectorASN, IPVersion: item.CollectorIPVersion,
	}
	row.TargetMeta = targetMetaRef(storage.ExportMeasurement{
		Country: item.Country, CCA2: item.CCA2, Region: item.Region, City: item.City,
		CountryEN: item.CountryEN,
		ColoIATA:  item.ColoIATA, ColoCCA2: item.ColoCCA2, ColoCity: item.ColoCity,
	})

	hops, err := decodeHops(item.TraceJSON)
	if err != nil {
		// 解析失败时仍然保留这一行的"成功/失败"事实，
		// 只是没有路径可看——与导出层的处理保持一致。
		hops = nil
	}

	row.Trace = &aggregate.TraceRef{
		Success: item.Success, Engine: item.Engine, EngineVersion: item.EngineVersion,
		Mode: item.Mode, DurationMS: item.DurationMS, HopCount: item.HopCount,
		ErrorType: item.ErrorType, Hops: hops,
	}
	return row
}

// targetMetaRef 构造目标元数据。
func targetMetaRef(item storage.ExportMeasurement) *aggregate.TargetMetaRef {
	meta := &aggregate.TargetMetaRef{
		Country: item.Country, CCA2: item.CCA2, Region: item.Region, City: item.City,
	}
	if item.ColoIATA != "" || item.ColoCCA2 != "" || item.ColoCity != "" {
		meta.Colo = &aggregate.ColoRef{
			IATA: item.ColoIATA, CCA2: item.ColoCCA2, City: item.ColoCity,
		}
	}
	return meta
}

// decodeHops 解析数据库里保存的归一化跳列表。
//
// 与 export 包同样各自持有一份：数据库里的内部形状（大写字段名）
// 属于 storage 的实现细节，两处都显式声明"我依赖哪些字段"，
// 比从别处 import 一个结构更能暴露耦合。
func decodeHops(traceJSON string) ([]aggregate.HopView, error) {
	trimmed := strings.TrimSpace(traceJSON)
	if trimmed == "" {
		return nil, nil
	}

	var internal []struct {
		TTL            int       `json:"TTL"`
		IP             string    `json:"IP"`
		RTTMS          []float64 `json:"RTTMS"`
		Timeout        bool      `json:"Timeout"`
		ASN            string    `json:"ASN"`
		ASOrganization string    `json:"ASOrganization"`
		Country        string    `json:"Country"`
		Province       string    `json:"Province"`
		City           string    `json:"City"`
	}
	if err := json.Unmarshal([]byte(trimmed), &internal); err != nil {
		return nil, fmt.Errorf("decode hops: %w", err)
	}

	hops := make([]aggregate.HopView, 0, len(internal))
	for _, h := range internal {
		hops = append(hops, aggregate.HopView{
			TTL: h.TTL, IP: h.IP, RTTMS: h.RTTMS, Timeout: h.Timeout,
			ASN: h.ASN, ASOrganization: h.ASOrganization,
			Country: h.Country, City: h.City,
		})
	}
	return hops, nil
}

// LoadFromJSONL 从 JSONL 文件/目录载入（公开数据路径）。
//
// 逐行索引而不是先全部读进内存：大文件（几十万行）必须能处理。
func LoadFromJSONL(inputs []string, opts IndexOptions) (*Dataset, *aggregate.LoadStats, error) {
	sources, err := aggregate.DiscoverSources(inputs)
	if err != nil {
		return nil, nil, err
	}

	dataset := NewDataset()
	stats := &aggregate.LoadStats{}

	for _, source := range sources {
		reader, closeFn, err := source.Open()
		if err != nil {
			// 与 aggregate 一致：单个坏文件不终止整批，
			// 但必须让调用方知道（统计里体现在 Sources 与实际文件数之差）。
			_ = closeFn
			continue
		}

		stats.Sources++
		loadErr := aggregate.Load(reader, stats, func(row Row) error {
			dataset.IndexRows([]Row{row}, opts)
			return nil
		})
		if closeErr := closeFn(); closeErr != nil && loadErr == nil {
			loadErr = closeErr
		}
		if loadErr != nil {
			return nil, stats, fmt.Errorf("%s: %w", source.Path, loadErr)
		}
	}

	return dataset, stats, nil
}
