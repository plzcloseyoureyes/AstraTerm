package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

// DefaultBodyLimit bounds request bodies: Bind reads at most this much JSON, and handlers reading the raw body are
// capped at it unless their route sets BodyLimit.
const DefaultBodyLimit = 2 << 20

const contentTypeJSON = "application/json; charset=utf-8"

// jsonSerializer is the Echo JSONSerializer: c.JSON writes Content-Type "application/json; charset=utf-8",
// Cache-Control "no-store" (API responses are never cached) unless the handler set one, without HTML escaping.
type jsonSerializer struct{}

// Serialize implements echo.JSONSerializer.
func (jsonSerializer) Serialize(c *echo.Context, v any, indent string) error {
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, contentTypeJSON)
	if h.Get(echo.HeaderCacheControl) == "" {
		h.Set(echo.HeaderCacheControl, "no-store")
	}
	enc := json.NewEncoder(c.Response())
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	return enc.Encode(v)
}

// Deserialize implements echo.JSONSerializer with Bind's semantics.
func (jsonSerializer) Deserialize(c *echo.Context, v any) error { return Bind(c, v) }

// jsonBinder makes c.Bind(&v) equivalent to httpx.Bind(c, &v) (JSON body only, whatever the Content-Type).
type jsonBinder struct{}

// Bind implements echo.Binder.
func (jsonBinder) Bind(c *echo.Context, v any) error { return Bind(c, v) }

var okBody = map[string]bool{"ok": true}

// OK responds 200 {"ok": true}: the reply of mutations that return no resource.
func OK(c *echo.Context) error { return c.JSON(http.StatusOK, okBody) }

// Bind decodes the JSON request body (at most DefaultBodyLimit bytes) into v. Unknown fields are ignored. An empty
// body, malformed JSON, trailing data or a type mismatch is a 400 {code:"bad_request"}; an oversized body is a 413
// {code:"too_large"}. The Content-Type is not checked.
func Bind(c *echo.Context, v any) error { return BindLimit(c, v, DefaultBodyLimit) }

// BindOptional is like Bind but treats an empty body as "no fields" (v is left untouched).
func BindOptional(c *echo.Context, v any) error {
	if err := BindLimit(c, v, DefaultBodyLimit); err != errEmptyBody {
		return err
	}
	return nil
}

// BindLimit is Bind with a body limit of limit bytes instead of DefaultBodyLimit (a smaller BodyLimit set on the
// route still applies).
func BindLimit(c *echo.Context, v any, limit int64) error { return decodeJSON(c.Request(), v, limit) }

var errEmptyBody = BadRequest("request body required")

func decodeJSON(r *http.Request, v any, limit int64) error {
	if r.Body == nil || r.Body == http.NoBody {
		return errEmptyBody
	}
	body := r.Body
	if lb, ok := body.(*limitedBody); ok && !lb.explicit {
		body = lb.orig // the default cap is replaced by limit
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, body, limit))
	if err := dec.Decode(v); err != nil {
		return decodeError(err)
	}
	// Reject trailing garbage (but allow trailing whitespace).
	if _, err := dec.Token(); err != io.EOF {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return BadRequest("invalid JSON: unexpected data after the top-level value")
	}
	return nil
}

func decodeError(err error) error {
	var (
		mbe  *http.MaxBytesError
		se   *json.SyntaxError
		ute  *json.UnmarshalTypeError
		inve *json.InvalidUnmarshalError
	)
	switch {
	case errors.Is(err, io.EOF):
		return errEmptyBody
	case errors.As(err, &mbe):
		return err
	case errors.As(err, &se):
		return BadRequest(fmt.Sprintf("invalid JSON at offset %d", se.Offset))
	case errors.As(err, &ute):
		field := ute.Field
		if field == "" {
			return BadRequest("invalid JSON value: expected " + ute.Type.String())
		}
		return BadRequest(fmt.Sprintf("invalid value for field %q: expected %s", field, ute.Type.String()))
	case errors.As(err, &inve):
		return Internal(err)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return BadRequest("invalid JSON: unexpected end of input")
	}
	if strings.HasPrefix(err.Error(), "json:") {
		return BadRequest("invalid JSON: " + strings.TrimPrefix(err.Error(), "json: "))
	}
	return BadRequest("invalid request body")
}
