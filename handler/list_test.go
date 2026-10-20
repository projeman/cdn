package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/minio/minio-go/v7"
	"github.com/mstgnz/cdn/service"
)

// fakeLister serves a fixed key list and honours StartAfter, like MinIO does.
type fakeLister struct {
	service.ObjectStore
	keys   []string
	exists bool
}

func (f fakeLister) BucketExists(context.Context, string) (bool, error) { return f.exists, nil }

func (f fakeLister) ListObjects(_ context.Context, _ string, o minio.ListObjectsOptions) <-chan minio.ObjectInfo {
	ch := make(chan minio.ObjectInfo, len(f.keys))
	for _, k := range f.keys {
		if k > o.StartAfter && len(k) >= len(o.Prefix) && k[:len(o.Prefix)] == o.Prefix {
			ch <- minio.ObjectInfo{Key: k, Size: 3, LastModified: time.Unix(0, 0)}
		}
	}
	close(ch)
	return ch
}

func listApp(p service.Principal, f fakeLister) *fiber.App {
	app := fiber.New()
	h := NewListHandler(f)
	app.Get("/list", func(c *fiber.Ctx) error { service.StorePrincipal(c, p); return h.ListObjects(c) })
	app.Get("/list/:bucket", func(c *fiber.Ctx) error { service.StorePrincipal(c, p); return h.ListObjects(c) })
	return app
}

func get(t *testing.T, app *fiber.App, url string) (int, map[string]any) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", url, nil))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestListPaginates(t *testing.T) {
	app := listApp(service.Principal{}, fakeLister{exists: true, keys: []string{"a.jpg", "b.jpg", "c.jpg"}})

	code, body := get(t, app, "/list/test?limit=2")
	data := body["data"].(map[string]any)
	if code != 200 || data["count"].(float64) != 2 || data["truncated"] != true || data["next_after"] != "b.jpg" {
		t.Fatalf("page 1: %d %+v", code, data)
	}
	_, body = get(t, app, "/list/test?limit=2&after=b.jpg")
	data = body["data"].(map[string]any)
	if data["count"].(float64) != 1 || data["truncated"] != false {
		t.Fatalf("page 2: %+v", data)
	}
}

func TestListScopedTokenCannotReadOtherBucket(t *testing.T) {
	app := listApp(service.Principal{Scoped: true, Bucket: "test"}, fakeLister{exists: true, keys: []string{"a"}})

	if code, _ := get(t, app, "/list/picup"); code != 403 {
		t.Fatalf("other bucket: got %d, want 403", code)
	}
	if code, _ := get(t, app, "/list"); code != 200 {
		t.Fatalf("own bucket via /list: got %d, want 200", code)
	}
}

func TestListValidation(t *testing.T) {
	app := listApp(service.Principal{}, fakeLister{exists: true})
	for _, u := range []string{"/list", "/list/test?limit=0", "/list/test?limit=5000", "/list/test?prefix=../x"} {
		if code, _ := get(t, app, u); code != 400 {
			t.Fatalf("%s: got %d, want 400", u, code)
		}
	}
	app = listApp(service.Principal{}, fakeLister{exists: false})
	if code, _ := get(t, app, "/list/nope-bucket"); code != 404 {
		t.Fatalf("missing bucket: got %d, want 404", code)
	}
}
