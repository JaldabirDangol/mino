package apitokens_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mino/internal/apitokens"
	"mino/internal/testutil"
)

func TestTokens(t *testing.T) {
	pool := testutil.DB(t)
	svc := apitokens.NewService(pool)
	ctx := context.Background()
	siteA := testutil.Site(t, pool, "a")
	siteB := testutil.Site(t, pool, "b")

	secret, tok, err := svc.Create(ctx, &apitokens.Token{SiteID: siteA, Name: "Frontend", Type: apitokens.ReadOnly}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "mino_") || tok.Prefix != secret[:12] {
		t.Fatalf("secret %q prefix %q", secret, tok.Prefix)
	}
	got, err := svc.Authenticate(ctx, secret)
	if err != nil || got.ID != tok.ID || got.SiteID != siteA {
		t.Fatalf("authenticate: %v", err)
	}
	if _, err := svc.Authenticate(ctx, secret[:len(secret)-1]+"x"); !errors.Is(err, apitokens.ErrInvalid) {
		t.Fatal("tampered secret accepted")
	}

	if _, _, err := svc.Create(ctx, &apitokens.Token{SiteID: siteA, Name: "Frontend", Type: apitokens.ReadOnly}, 0); err == nil {
		t.Fatal("duplicate name within a site accepted")
	}
	if _, _, err := svc.Create(ctx, &apitokens.Token{SiteID: siteB, Name: "Frontend", Type: apitokens.ReadOnly}, 0); err != nil {
		t.Fatalf("same name on another site: %v", err)
	}

	past := time.Now().Add(-time.Hour)
	expired, _, _ := svc.Create(ctx, &apitokens.Token{SiteID: siteA, Name: "Old", Type: apitokens.FullAccess, ExpiresAt: &past}, 0)
	if _, err := svc.Authenticate(ctx, expired); !errors.Is(err, apitokens.ErrInvalid) {
		t.Fatal("expired token accepted")
	}

	newSecret, err := svc.Regenerate(ctx, siteA, tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, secret); !errors.Is(err, apitokens.ErrInvalid) {
		t.Fatal("old secret works after regenerate")
	}
	if _, err := svc.Authenticate(ctx, newSecret); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, siteB, tok.ID); !errors.Is(err, apitokens.ErrNotFound) {
		t.Fatal("token deleted through another site")
	}
	if err := svc.Delete(ctx, siteA, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, newSecret); !errors.Is(err, apitokens.ErrInvalid) {
		t.Fatal("revoked token accepted")
	}

	if _, _, err := svc.Create(ctx, &apitokens.Token{SiteID: siteA, Name: "Bad", Type: apitokens.Custom, Permissions: []string{"drop:all"}}, 0); err == nil {
		t.Fatal("unknown permission accepted")
	}
}

func TestPrincipal(t *testing.T) {
	cases := []struct {
		name string
		p    apitokens.Principal
		perm string
		want bool
	}{
		{"public allowed", apitokens.Principal{Public: []string{"find:article"}}, "find:article", true},
		{"public denied", apitokens.Principal{Public: []string{"find:article"}}, "findOne:article", false},
		{"read-only find", apitokens.Principal{Token: &apitokens.Token{Type: apitokens.ReadOnly}}, "findOne:page", true},
		{"read-only write", apitokens.Principal{Token: &apitokens.Token{Type: apitokens.ReadOnly}}, "create:page", false},
		{"read-only upload read", apitokens.Principal{Token: &apitokens.Token{Type: apitokens.ReadOnly}}, apitokens.UploadRead, true},
		{"read-only upload write", apitokens.Principal{Token: &apitokens.Token{Type: apitokens.ReadOnly}}, apitokens.UploadWrite, false},
		{"full access", apitokens.Principal{Token: &apitokens.Token{Type: apitokens.FullAccess}}, "delete:page", true},
		{"custom listed", apitokens.Principal{Token: &apitokens.Token{Type: apitokens.Custom, Permissions: []string{"create:page"}}}, "create:page", true},
		{"custom unlisted", apitokens.Principal{Token: &apitokens.Token{Type: apitokens.Custom, Permissions: []string{"create:page"}}}, "find:page", false},
	}
	for _, c := range cases {
		if got := c.p.Allowed(c.perm); got != c.want {
			t.Errorf("%s: Allowed(%s) = %v", c.name, c.perm, got)
		}
	}
}

func TestPublicPermissions(t *testing.T) {
	pool := testutil.DB(t)
	svc := apitokens.NewService(pool)
	ctx := context.Background()
	site := testutil.Site(t, pool, "a")
	if err := svc.SetPublicPermissions(ctx, site, []string{"find:article", "findOne:article"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetPublicPermissions(ctx, site, []string{"delete:article"}); err == nil {
		t.Fatal("public write permission accepted")
	}
	got, _ := svc.PublicPermissions(ctx, site)
	if len(got) != 2 {
		t.Fatalf("public perms: %v", got)
	}
}
