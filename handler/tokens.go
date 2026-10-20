package handler

import (
	"context"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/minio/minio-go/v7"
	"github.com/mstgnz/cdn/pkg/bucket"
	"github.com/mstgnz/cdn/service"
)

// TokenHandler manages bucket-scoped tokens at runtime. Every route is mounted
// behind the general-token middleware: a scoped token must never be able to mint
// or list credentials.
type TokenHandler interface {
	Create(c *fiber.Ctx) error
	List(c *fiber.Ctx) error
	Revoke(c *fiber.Ctx) error
}

type tokenHandler struct {
	store       *service.TokenStore
	minioClient *minio.Client
}

func NewTokenHandler(store *service.TokenStore, minioClient *minio.Client) TokenHandler {
	return &tokenHandler{store: store, minioClient: minioClient}
}

type createTokenRequest struct {
	Label     string `json:"label"`
	ExpiresAt string `json:"expires_at"`
}

// Create mints a token for :bucket. The secret appears in this response only.
func (h *tokenHandler) Create(c *fiber.Ctx) error {
	name := c.Params("bucket")
	if err := bucket.Validate(name); err != nil {
		return service.Response(c, fiber.StatusBadRequest, false, err.Error(), nil)
	}

	var req createTokenRequest
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return service.Response(c, fiber.StatusBadRequest, false, "invalid JSON body", nil)
		}
	}
	label := strings.TrimSpace(req.Label)
	if len(label) > 100 {
		return service.Response(c, fiber.StatusBadRequest, false, "label must be at most 100 characters", nil)
	}
	var expiresAt time.Time
	if raw := strings.TrimSpace(req.ExpiresAt); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return service.Response(c, fiber.StatusBadRequest, false, "expires_at must be an RFC 3339 timestamp such as 2026-12-31T00:00:00Z", nil)
		}
		expiresAt = parsed
	}

	exists, err := h.minioClient.BucketExists(context.Background(), name)
	if err != nil {
		return service.Response(c, fiber.StatusBadGateway, false, "could not check bucket", nil)
	}
	if !exists {
		return service.Response(c, fiber.StatusNotFound, false, "bucket not found", nil)
	}

	secret, info, err := h.store.Create(name, label, expiresAt)
	if err != nil {
		return service.Response(c, fiber.StatusBadRequest, false, err.Error(), nil)
	}
	return service.Response(c, fiber.StatusCreated, true, "token created; store it now, it cannot be shown again", fiber.Map{
		"token":  name + ":" + secret,
		"secret": secret,
		"info":   info,
	})
}

// List returns token metadata, optionally filtered with ?bucket=. No secrets.
func (h *tokenHandler) List(c *fiber.Ctx) error {
	infos, err := h.store.List(c.Query("bucket"))
	if err != nil {
		return service.Response(c, fiber.StatusBadGateway, false, "could not list tokens", nil)
	}
	return service.Response(c, fiber.StatusOK, true, "tokens listed", infos)
}

// Revoke deletes one token by id.
func (h *tokenHandler) Revoke(c *fiber.Ctx) error {
	ok, err := h.store.Revoke(c.Params("id"))
	if err != nil {
		return service.Response(c, fiber.StatusBadGateway, false, "could not revoke token", nil)
	}
	if !ok {
		return service.Response(c, fiber.StatusNotFound, false, "token not found", nil)
	}
	return service.Response(c, fiber.StatusOK, true, "token revoked", nil)
}
