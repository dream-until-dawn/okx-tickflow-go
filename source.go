package tickflow

import "context"

// FetchRequest 是一次拉取请求，区间为左闭右开的 [From, To)，单位毫秒。
type FetchRequest struct {
	InstID string
	Bar    string
	From   int64
	To     int64
}

// Source 是 K 线的数据来源。
//
// 默认实现是 source/okxsource，它包住 okx-api-v5-go 并消化上游的几个粗糙面：
// 返回是倒序的、分页语义与直觉相反（After 是更旧、Before 是更新）、单次条数有
// 上限、近端与远端是两个不同的接口、以及结果里含一根【未完结】的当前 K 线。
//
// 实现【必须】保证：
//
//   - 返回按 ts 升序；
//   - 只含已完结的 K 线（丢弃上游 Confirm == false 的那一根）；
//   - 只含落在 [From, To) 内的 K 线；
//   - 区间内 OKX 确实没有数据时返回空切片而非错误——小币种与维护期是会缺根的，
//     那不是故障。
//
// 实现会把整个请求区间缓冲在内存里返回，所以【调用方负责控制单次区间的大小】。
// Syncer 已经按 chunk 分好了，直接用它即可。
type Source interface {
	Fetch(ctx context.Context, req FetchRequest) ([]Candle, error)
}
