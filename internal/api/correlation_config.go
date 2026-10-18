package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	appmw "github.com/yu/iip/internal/middleware"

	"github.com/yu/iip/internal/correlation"
)

// correlationConfigMaxBytes 是配置请求体上限。配置只有几个数字，16 KiB 绰绰有余；
// 设上限是为了不给这个可写端点留下"超大 body 吃内存"的口子。
const correlationConfigMaxBytes int64 = 16 << 10 // 16 KiB

// CorrelationConfigStore 是配置 handler 所需的最小能力，定义在消费方（本包）。
// *repository.CorrelationConfig 满足它。
type CorrelationConfigStore interface {
	GetConfig(ctx context.Context) (correlation.Config, error)
	PutConfig(ctx context.Context, cfg correlation.Config) error
}

// CorrelationConfigGet 返回 GET /api/v1/correlation/config 的 handler。
//
// 读当前权重/阈值/开关；存储故障回 503，让前端可重试而非误以为"配置为空"。
func CorrelationConfigGet(store CorrelationConfigStore, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		cfg, err := store.GetConfig(r.Context())
		if err != nil {
			logger.Error("读取关联配置失败",
				slog.String("error", err.Error()),
				slog.String("peer_addr", appmw.PeerAddr(r)))
			writeError(w, logger, http.StatusServiceUnavailable, msgStorageUnavailable)
			return
		}
		writeJSON(w, logger, http.StatusOK, cfg)
	}
}

// CorrelationConfigPut 返回 PUT /api/v1/correlation/config 的 handler。
//
// 语义为「以当前配置为底、用请求体覆盖所提供的字段」（支持部分更新）：
// 例如只提交 {"thresholds":{"merge":0.9}} 不会重置权重。
//
// 校验失败回 422（附可读原因，如「五维权重之和必须为 1.0」）；
// 请求体非法 JSON 回 400；存储故障回 503。
//
// 写入即生效：关联每轮都会重新读配置，无需重启（手册验收项）。
func CorrelationConfigPut(store CorrelationConfigStore, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		base, err := store.GetConfig(r.Context())
		if err != nil {
			logger.Error("读取关联配置失败（PUT 前置）", slog.String("error", err.Error()))
			writeError(w, logger, http.StatusServiceUnavailable, msgStorageUnavailable)
			return
		}

		// 读取业务统一交给 readBody（413 / 400 / 非 UTF-8 的语义与其余入口一致）。
		body, ok := readBody(w, r, correlationConfigMaxBytes, logger)
		if !ok {
			return
		}

		// 覆盖式解析：只改请求体中出现的字段，其余保留现值（部分更新）。
		//
		// 必须 DisallowUnknownFields：否则 `{"threshods":{"merge":0.9}}` 这类
		// 键名打错会被静默忽略，接口回 200 而实际什么都没改——
		// 对配置接口来说这是最难排查的一类故障（看着成功，行为不变）。
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&base); err != nil {
			writeError(w, logger, http.StatusBadRequest, "请求体无法解析为关联配置: "+err.Error())
			return
		}
		if err := base.Validate(); err != nil {
			writeError(w, logger, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := store.PutConfig(r.Context(), base); err != nil {
			logger.Error("写入关联配置失败", slog.String("error", err.Error()))
			writeError(w, logger, http.StatusServiceUnavailable, msgStorageUnavailable)
			return
		}

		logger.Info("关联配置已更新",
			slog.Bool("enabled", base.Enabled),
			slog.Float64("merge_threshold", base.Thresholds.Merge))
		writeJSON(w, logger, http.StatusOK, base)
	}
}
