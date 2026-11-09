package server

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/query"
)

// authenticate validates the bearer token (if any) and selects the Firebird role.
func (s *Server) authenticate(rc *reqCtx) error {
	h := rc.r.Header.Get("Authorization")
	if h == "" {
		if s.cfg.DBAnonRole == "" {
			return &query.Error{Status: 401, Code: "FBRST302",
				Message: "anonymous access is disabled", Hint: "send an Authorization: Bearer <JWT> header"}
		}
		rc.role, rc.anon = s.cfg.DBAnonRole, true
		return nil
	}
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(token) == "" {
		return jwtError("expected Authorization: Bearer <token>")
	}
	if !s.jwt.enabled() {
		return &query.Error{Status: 500, Code: "FBRST300", Message: "server lacks JWT secret",
			Hint: "set jwt-secret or jwt-jwks-url"}
	}
	claims, err := s.jwt.verify(strings.TrimSpace(token))
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return jwtError("JWT expired")
		case errors.Is(err, jwt.ErrTokenNotValidYet):
			return jwtError("JWT not valid yet")
		case errors.Is(err, jwt.ErrTokenInvalidAudience):
			return jwtError("JWT not in audience")
		case errors.Is(err, jwt.ErrTokenInvalidIssuer):
			return jwtError("JWT not from an accepted issuer")
		}
		return jwtError(err.Error())
	}
	rc.claims = claims // shared with the JWT cache: read only
	role := claimPath(claims, s.cfg.JWTRoleClaim)
	if role == "" {
		if s.cfg.DBAnonRole == "" {
			return jwtError(fmt.Sprintf("JWT has no %q claim and no anonymous role is configured", s.cfg.JWTRoleClaim))
		}
		role = s.cfg.DBAnonRole
	}
	if !roleName.MatchString(role) {
		return jwtError(fmt.Sprintf("invalid role name %q", role))
	}
	rc.role = role
	return nil
}

// claimPath reads the role from the claims along path, PostgREST's
// jwt-role-claim-key syntax: dotted keys ("app_metadata.role"), keys in
// double quotes for names with dots (."https://example.com/role"), and
// array indexes ("realm_access.roles[0]", negative from the end). A path
// that is itself the name of a claim is read as that claim.
func claimPath(c map[string]any, path string) string {
	if v, ok := c[strings.TrimPrefix(path, ".")].(string); ok {
		return v
	}
	var cur any = c
	rest := strings.TrimPrefix(strings.TrimSpace(path), ".")
	for rest != "" {
		var key string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				return ""
			}
			key, rest = rest[1:end+1], rest[end+2:]
		} else {
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			key, rest = rest[:end], rest[end:]
		}
		if key != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return ""
			}
			cur = m[key]
		}
		for strings.HasPrefix(rest, "[") {
			end := strings.Index(rest, "]")
			if end < 0 {
				return ""
			}
			i, err := strconv.Atoi(strings.TrimSpace(rest[1:end]))
			arr, ok := cur.([]any)
			if err != nil || !ok {
				return ""
			}
			if i < 0 {
				i += len(arr)
			}
			if i < 0 || i >= len(arr) {
				return ""
			}
			cur, rest = arr[i], rest[end+1:]
		}
		rest = strings.TrimPrefix(rest, ".")
	}
	s, _ := cur.(string)
	return s
}

func jwtError(msg string) error {
	return &query.Error{Status: 401, Code: "FBRST301", Message: msg}
}
