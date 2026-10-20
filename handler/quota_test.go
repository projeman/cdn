package handler

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/mstgnz/cdn/service"
)

func quotaApp(t *testing.T) (*fiber.App, *service.QuotaStore) {
	t.Helper()
	q := service.NewQuotaStore(service.NewMemoryTokenBackend())
	h := NewQuotaHandler(fakeLister{exists: true, keys: []string{"a", "b"}}, q)
	app := fiber.New()
	app.Put("/minio/:bucket/quota", h.Set)
	app.Get("/minio/:bucket/quota", h.Get)
	app.Delete("/minio/:bucket/quota", h.Clear)
	app.Get("/minio/quotas", h.List)
	return app, q
}

func call(t *testing.T, app *fiber.App, method, url, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestQuotaSetGetClear(t *testing.T) {
	app, _ := quotaApp(t)

	if code, _ := call(t, app, "PUT", "/minio/deneme/quota", `{"limit":"500MB"}`); code != 200 {
		t.Fatalf("set: %d", code)
	}
	code, body := call(t, app, "GET", "/minio/deneme/quota", "")
	data := body["data"].(map[string]any)
	if code != 200 || data["limit"].(float64) != 500<<20 || data["used"].(float64) != 6 {
		t.Fatalf("get: %d %+v", code, data) // fakeLister: 2 objects x 3 bytes
	}
	if code, _ := call(t, app, "DELETE", "/minio/deneme/quota", ""); code != 200 {
		t.Fatalf("clear: %d", code)
	}
	if code, _ := call(t, app, "GET", "/minio/deneme/quota", ""); code != 404 {
		t.Fatalf("after clear want 404, got %d", code)
	}
}

func TestQuotaSetRejectsBadInput(t *testing.T) {
	app, _ := quotaApp(t)
	for _, b := range []string{``, `{}`, `{"limit":"abc"}`, `{"limit":"-5M"}`, `{"limit":0}`} {
		if code, _ := call(t, app, "PUT", "/minio/deneme/quota", b); code != 400 {
			t.Errorf("body %q: got %d, want 400", b, code)
		}
	}
	for _, m := range []string{"PUT", "GET", "DELETE"} {
		if code, _ := call(t, app, m, "/minio/BAD_NAME/quota", `{"limit":"1G"}`); code != 400 {
			t.Errorf("%s bad bucket name: got %d, want 400", m, code)
		}
	}
}

func TestQuotaList(t *testing.T) {
	app, _ := quotaApp(t)
	call(t, app, "PUT", "/minio/yyy/quota", `{"limit":"500M"}`)
	call(t, app, "PUT", "/minio/xxx/quota", `{"limit":"100M"}`)
	_, body := call(t, app, "GET", "/minio/quotas", "")
	items := body["data"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["bucket"] != "xxx" {
		t.Fatalf("list: %+v", items)
	}
}
