package server

import (
	"errors"
	"fmt"
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

// claimPath reads a dotted claim path such as "app_metadata.role".
func claimPath(c map[string]any, path string) string {
	var cur any = c
	for _, p := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}

func jwtError(msg string) error {
	return &query.Error{Status: 401, Code: "FBRST301", Message: msg}
}
