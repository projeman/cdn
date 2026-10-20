package handler

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/minio/minio-go/v7"
	"github.com/mstgnz/cdn/pkg/bucket"
	"github.com/mstgnz/cdn/service"
)

// QuotaHandler manages per-bucket storage quotas. Mounted behind the
// general-token middleware: a scoped token must not be able to raise its own cap.
type QuotaHandler interface {
	Set(c *fiber.Ctx) error
	Get(c *fiber.Ctx) error
	Clear(c *fiber.Ctx) error
	List(c *fiber.Ctx) error
}

type quotaHandler struct {
	store service.ObjectStore
	quota *service.QuotaStore
}

func NewQuotaHandler(store service.ObjectStore, quota *service.QuotaStore) QuotaHandler {
	return &quotaHandler{store: store, quota: quota}
}

type quotaView struct {
	Bucket     string  `json:"bucket"`
	Limit      int64   `json:"limit"`
	LimitHuman string  `json:"limit_human"`
	Used       *int64  `json:"used,omitempty"`
	UsedHuman  string  `json:"used_human,omitempty"`
	Percent    float64 `json:"percent,omitempty"`
}

func (h *quotaHandler) usage(ctx context.Context, name string, refresh bool) (int64, error) {
	return h.quota.Used(name, func() (int64, error) {
		var total int64
		for obj := range h.store.ListObjects(ctx, name, minio.ListObjectsOptions{Recursive: true}) {
			if obj.Err != nil {
				return 0, obj.Err
			}
			total += obj.Size
		}
		return total, nil
	}, refresh)
}

// Set handles PUT /minio/:bucket/quota with {"limit":"500MB"} (or a byte count).
func (h *quotaHandler) Set(c *fiber.Ctx) error {
	name := c.Params("bucket")
	if err := bucket.Validate(name); err != nil {
		return service.Response(c, fiber.StatusBadRequest, false, err.Error(), nil)
	}
	var req struct {
		Limit any `json:"limit"`
	}
	if err := c.BodyParser(&req); err != nil || req.Limit == nil {
		return service.Response(c, fiber.StatusBadRequest, false, `body must be JSON like {"limit":"500MB"}`, nil)
	}
	var raw string
	switch v := req.Limit.(type) {
	case string:
		raw = v
	case float64:
		raw = strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return service.Response(c, fiber.StatusBadRequest, false, "limit must be a string such as 500MB or a byte count", nil)
	}
	limit, err := service.ParseSize(raw)
	if err != nil {
		return service.Response(c, fiber.StatusBadRequest, false, err.Error(), nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exists, err := h.store.BucketExists(ctx, name)
	if err != nil {
		return service.Response(c, fiber.StatusBadGateway, false, "could not reach storage", nil)
	}
	if !exists {
		return service.Response(c, fiber.StatusNotFound, false, "bucket not found", nil)
	}
	if err := h.quota.SetLimit(name, limit); err != nil {
		return service.Response(c, fiber.StatusBadGateway, false, "could not save quota", nil)
	}
	return service.Response(c, fiber.StatusOK, true, "quota set", quotaView{
		Bucket: name, Limit: limit, LimitHuman: service.FormatSize(limit),
	})
}

// Get handles GET /minio/:bucket/quota. ?refresh=true recounts instead of using
// the cached usage.
func (h *quotaHandler) Get(c *fiber.Ctx) error {
	name := c.Params("bucket")
	if err := bucket.Validate(name); err != nil {
		return service.Response(c, fiber.StatusBadRequest, false, err.Error(), nil)
	}
	limit, ok, err := h.quota.Limit(name)
	if err != nil {
		return service.Response(c, fiber.StatusBadGateway, false, "could not read quota", nil)
	}
	if !ok {
		return service.Response(c, fiber.StatusNotFound, false, "no quota set for this bucket", nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	used, err := h.usage(ctx, name, strings.EqualFold(c.Query("refresh"), "true"))
	if err != nil {
		return service.Response(c, fiber.StatusBadGateway, false, "could not measure usage", nil)
	}
	v := quotaView{
		Bucket: name, Limit: limit, LimitHuman: service.FormatSize(limit),
		Used: &used, UsedHuman: service.FormatSize(used),
		Percent: float64(int(float64(used)/float64(limit)*1000)) / 10,
	}
	return service.Response(c, fiber.StatusOK, true, "quota read", v)
}

// Clear handles DELETE /minio/:bucket/quota: the bucket becomes unlimited.
func (h *quotaHandler) Clear(c *fiber.Ctx) error {
	name := c.Params("bucket")
	if err := bucket.Validate(name); err != nil {
		return service.Response(c, fiber.StatusBadRequest, false, err.Error(), nil)
	}
	if err := h.quota.ClearLimit(name); err != nil {
		return service.Response(c, fiber.StatusBadGateway, false, "could not clear quota", nil)
	}
	return service.Response(c, fiber.StatusOK, true, "quota removed", nil)
}

// List handles GET /minio/quotas: every configured limit, without measuring usage.
func (h *quotaHandler) List(c *fiber.Ctx) error {
	all, err := h.quota.Limits()
	if err != nil {
		return service.Response(c, fiber.StatusBadGateway, false, "could not list quotas", nil)
	}
	out := make([]quotaView, 0, len(all))
	for name, limit := range all {
		out = append(out, quotaView{Bucket: name, Limit: limit, LimitHuman: service.FormatSize(limit)})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Bucket < out[b].Bucket })
	return service.Response(c, fiber.StatusOK, true, "quotas listed", out)
}
