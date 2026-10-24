package server

import (
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

// TestReadTransactionReuse checks that reads sharing a read-only transaction
// see each other's context variables cleared, that errors don't poison the
// transaction, and that transactions are recycled.
func TestReadTransactionReuse(t *testing.T) {
	old := maxReadTxUses
	maxReadTxUses = 3
	defer func() { maxReadTxUses = old }()
	// one connection per role, so every request of a role reuses the same transaction
	ts := testServerWith(t, func(c *config.Config) { c.DBPool = 1 })
	ana := "Bearer " + token(t, jwt.MapClaims{"role": "web_user", "sub": "ana@example.com"})
	bob := "Bearer " + token(t, jwt.MapClaims{"role": "web_user", "sub": "bob@example.com"})

	for round := 0; round < 8; round++ { // past the recycling limit several times
		r := do(t, ts, "GET", "/rpc/whoami", "", "Authorization", ana)
		if !strings.HasPrefix(r.body, `"ana@example.com`) {
			t.Fatalf("round %d ana: %s", round, r.body)
		}
		r = do(t, ts, "GET", "/rpc/whoami", "", "Authorization", bob)
		if !strings.HasPrefix(r.body, `"bob@example.com`) {
			t.Fatalf("round %d bob: %s", round, r.body)
		}
		// a token without sub on the same transaction: the old sub must be gone
		r = do(t, ts, "GET", "/rpc/whoami", "", "Authorization", "Bearer "+token(t, jwt.MapClaims{"role": "web_user"}))
		if !strings.HasPrefix(r.body, `"anonymous`) {
			t.Fatalf("round %d leftover claim: %s", round, r.body)
		}
		// a statement error inside the shared transaction doesn't affect the next read
		if r = do(t, ts, "GET", "/rpc/add_vat?amount=x", "", "Authorization", ana); r.status != 400 {
			t.Fatalf("round %d bad argument: %d %s", round, r.status, r.body)
		}
		r = do(t, ts, "GET", "/books?select=id&id=eq.1", "", "Authorization", ana)
		if r.status != 200 || r.body != `[{"id":1}]` {
			t.Fatalf("round %d after error: %d %s", round, r.status, r.body)
		}
	}
	// the anonymous role has its own pool and transaction
	if r := do(t, ts, "GET", "/rpc/whoami", ""); !strings.HasPrefix(r.body, `"anonymous`) {
		t.Errorf("anonymous: %s", r.body)
	}
	// a write after reads, then a read sees it (read committed)
	user := ana
	r := do(t, ts, "POST", "/tags?select=id", `{"name":"reuse-tag"}`, "Authorization", user, "Prefer", "return=representation")
	if r.status != 201 {
		t.Fatalf("insert: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/tags?select=name&name=eq.reuse-tag", "", "Authorization", user); r.body != `[{"name":"reuse-tag"}]` {
		t.Errorf("read after write: %s", r.body)
	}
	if r = do(t, ts, "DELETE", "/tags?name=eq.reuse-tag", "", "Authorization", user); r.status != 204 {
		t.Errorf("cleanup: %d %s", r.status, r.body)
	}
	if r = do(t, ts, "GET", "/tags?select=name&name=eq.reuse-tag", "", "Authorization", user); r.body != "[]" {
		t.Errorf("read after delete: %s", r.body)
	}

	// with reuse off, the same requests work the old way
	fresh := testServerWith(t, func(c *config.Config) { c.DBPool = 1; c.DBReadTxFresh = true })
	if r = do(t, fresh, "GET", "/rpc/whoami", "", "Authorization", ana); !strings.HasPrefix(r.body, `"ana@example.com`) {
		t.Errorf("fresh transactions: %s", r.body)
	}
}
