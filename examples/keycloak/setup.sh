#!/bin/bash
# Creates the bookstore realm for the Keycloak tutorial. Run it inside the
# Keycloak container (see compose.yml); it fails if the realm already exists.
set -e
kc=/opt/keycloak/bin/kcadm.sh
$kc config credentials --server http://localhost:8080 --realm master --user admin --password admin

# a realm for the application; users may have attributes not declared in the user profile
$kc create realms -s realm=bookstore -s enabled=true
$kc update realms/bookstore/users/profile -f - <<'JSON'
{"unmanagedAttributePolicy": "ADMIN_EDIT",
 "attributes": [
  {"name":"username","displayName":"${username}","validations":{"length":{"min":3,"max":255},"username-prohibited-characters":{},"up-username-not-idn-homograph":{}},"permissions":{"view":["admin","user"],"edit":["admin","user"]},"multivalued":false},
  {"name":"email","displayName":"${email}","validations":{"email":{},"length":{"max":255}},"permissions":{"view":["admin","user"],"edit":["admin","user"]},"multivalued":false}
 ]}
JSON

# the client your application logs users in with
CID=$($kc create clients -r bookstore -i -f - <<'JSON'
{"clientId": "bookstore-app", "publicClient": true, "directAccessGrantsEnabled": true,
 "standardFlowEnabled": true, "redirectUris": ["http://localhost:5173/*"], "webOrigins": ["+"]}
JSON
)

# two claims in the access token: role (from the user's firebird_role attribute)
# and aud = firebirdrest
$kc create clients/$CID/protocol-mappers/models -r bookstore -f - <<'JSON'
{"name": "firebird role", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-attribute-mapper",
 "config": {"user.attribute": "firebird_role", "claim.name": "role", "jsonType.label": "String",
            "access.token.claim": "true", "id.token.claim": "false", "userinfo.token.claim": "false"}}
JSON
$kc create clients/$CID/protocol-mappers/models -r bookstore -f - <<'JSON'
{"name": "firebirdrest audience", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
 "config": {"included.custom.audience": "firebirdrest", "access.token.claim": "true", "id.token.claim": "false"}}
JSON

# a user with the web_user role, and one without a role
$kc create users -r bookstore -s username=ana -s email=ana@example.com -s emailVerified=true -s enabled=true \
  -s firstName=Ana -s lastName=Popescu -s 'attributes.firebird_role=["web_user"]'
$kc set-password -r bookstore --username ana --new-password ana-password
$kc create users -r bookstore -s username=bob -s email=bob@example.com -s emailVerified=true -s enabled=true \
  -s firstName=Bob -s lastName=Ionescu
$kc set-password -r bookstore --username bob --new-password bob-password
echo "realm bookstore ready: users ana (web_user) and bob (no role), client bookstore-app"
