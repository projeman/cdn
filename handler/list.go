package handler

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/minio/minio-go/v7"
	"github.com/mstgnz/cdn/pkg/bucket"
	"github.com/mstgnz/cdn/pkg/config"
	"github.com/mstgnz/cdn/service"
)

const (
	listDefaultLimit = 100
	listMaxLimit     = 1000
)

// ListHandler lists the objects of a bucket. Unlike object reads, which are
// public, listing is authenticated: a name list lets anyone enumerate every file
// in a bucket, which is exactly what unguessable object keys are there to prevent.
type ListHandler interface {
	ListObjects(c *fiber.Ctx) error
}

type listHandler struct {
	store service.ObjectStore
}

func NewListHandler(store service.ObjectStore) ListHandler {
	return &listHandler{store: store}
}

type listItem struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	LastModified string `json:"last_modified,omitempty"`
	IsDir        bool   `json:"is_dir,omitempty"`
	URL          string `json:"url,omitempty"`
}

// ListObjects handles GET /list and GET /list/:bucket.
//
// Query parameters:
//
//	prefix     only keys starting with this
//	limit      page size, 1-1000 (default 100)
//	after      resume after this key (use next_after from the previous page)
//	recursive  "false" lists one folder level and returns sub-folders as is_dir
//
// A bucket-scoped token may omit the bucket and lists its own; naming another
// bucket is a 403. The general token must name one.
func (h *listHandler) ListObjects(c *fiber.Ctx) error {
	name, err := resolveBucket(c, c.Params("bucket"))
	if err != nil {
		return bucketForbidden(c)
	}
	if name == "" {
		return service.Response(c, fiber.StatusBadRequest, false, "bucket is required", nil)
	}
	if err := bucket.Validate(name); err != nil {
		return service.Response(c, fiber.StatusBadRequest, false, err.Error(), nil)
	}

	limit := listDefaultLimit
	if raw := c.Query("limit"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 || n > listMaxLimit {
			return service.Response(c, fiber.StatusBadRequest, false, "limit must be between 1 and 1000", nil)
		}
		limit = n
	}
	prefix := c.Query("prefix")
	if service.HasUnsafeObjectKey(prefix + "x") {
		return service.Response(c, fiber.StatusBadRequest, false, "invalid prefix", nil)
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

	recursive := !strings.EqualFold(c.Query("recursive"), "false")
	baseURL := strings.TrimRight(config.GetEnvOrDefault("APP_URL", ""), "/")

	items := make([]listItem, 0, limit)
	more := false
	for obj := range h.store.ListObjects(ctx, name, minio.ListObjectsOptions{
		Prefix:     prefix,
		Recursive:  recursive,
		StartAfter: c.Query("after"),
	}) {
		if obj.Err != nil {
			return service.Response(c, fiber.StatusBadGateway, false, "could not list objects", nil)
		}
		if len(items) == limit {
			more = true
			cancel() // stop the listing; one extra entry is all we needed
			break
		}
		it := listItem{Key: obj.Key, Size: obj.Size, IsDir: strings.HasSuffix(obj.Key, "/")}
		if !obj.LastModified.IsZero() {
			it.LastModified = obj.LastModified.UTC().Format(time.RFC3339)
		}
		if baseURL != "" && !it.IsDir {
			it.URL = baseURL + "/" + name + "/" + obj.Key
		}
		items = append(items, it)
	}

	out := fiber.Map{"bucket": name, "count": len(items), "items": items, "truncated": more}
	if more {
		out["next_after"] = items[len(items)-1].Key
	}
	return service.Response(c, fiber.StatusOK, true, "objects listed", out)
}
