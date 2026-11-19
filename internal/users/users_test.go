package users_test

import (
	"context"
	"errors"
	"testing"

	"mino/internal/schema"
	"mino/internal/testutil"
	"mino/internal/users"
)

func roleByName(t *testing.T, svc *users.Service, name string) *users.Role {
	t.Helper()
	roles, err := svc.ListRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("role %s not seeded", name)
	return nil
}

func TestUsersAndAuthentication(t *testing.T) {
	pool := testutil.DB(t)
	svc := users.NewService(pool)
	ctx := context.Background()

	admin, err := svc.CreateFirst(ctx, users.Input{Email: "Admin@Example.com", Name: "Admin", Password: "super secret pw"})
	if err != nil {
		t.Fatal(err)
	}
	if !admin.SuperAdmin || admin.Email != "admin@example.com" {
		t.Fatalf("first user: %+v", admin)
	}
	if _, err := svc.CreateFirst(ctx, users.Input{Email: "x@example.com", Name: "X", Password: "super secret pw"}); err == nil {
		t.Fatal("setup must only run once")
	}

	var ve *schema.ValidationError
	if _, err := svc.Create(ctx, users.Input{Email: "ADMIN@example.com", Name: "Dup", Password: "super secret pw", Active: true}); !errors.As(err, &ve) || ve.For("email") == "" {
		t.Fatalf("duplicate email (case-insensitive): %v", err)
	}
	if _, err := svc.Create(ctx, users.Input{Email: "bad", Name: "", Password: "short"}); !errors.As(err, &ve) || len(ve.Errors) != 3 {
		t.Fatalf("validation: %v", err)
	}

	if _, err := svc.Authenticate(ctx, "admin@EXAMPLE.com", "super secret pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	for _, c := range [][2]string{{"admin@example.com", "nope"}, {"ghost@example.com", "super secret pw"}} {
		if _, err := svc.Authenticate(ctx, c[0], c[1]); !errors.Is(err, users.ErrInvalidCredentials) {
			t.Fatalf("bad login %v: %v", c, err)
		}
	}

	// The last active super admin cannot be demoted, deactivated or deleted.
	if _, err := svc.Update(ctx, admin.ID, users.Input{Email: admin.Email, Name: "Admin", SuperAdmin: false, Active: true}); !errors.Is(err, users.ErrLastSuperAdmin) {
		t.Fatalf("demote last super admin: %v", err)
	}
	if err := svc.Delete(ctx, admin.ID); !errors.Is(err, users.ErrLastSuperAdmin) {
		t.Fatalf("delete last super admin: %v", err)
	}

	ed, _ := svc.Create(ctx, users.Input{Email: "ed@example.com", Name: "Ed", Password: "editor password", Active: true})
	if _, err := svc.Update(ctx, ed.ID, users.Input{Email: ed.Email, Name: "Ed", Active: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, "ed@example.com", "editor password"); !errors.Is(err, users.ErrInvalidCredentials) {
		t.Fatal("inactive user logged in")
	}
	if err := svc.ChangePassword(ctx, admin.ID, "wrong", "another long password"); !errors.As(err, &ve) {
		t.Fatal("password change without current password")
	}
	if err := svc.ChangePassword(ctx, admin.ID, "super secret pw", "another long password"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, admin.Email, "another long password"); err != nil {
		t.Fatal("new password rejected")
	}
}

func TestSiteAccess(t *testing.T) {
	pool := testutil.DB(t)
	svc := users.NewService(pool)
	ctx := context.Background()
	siteA := testutil.Site(t, pool, "a")
	siteB := testutil.Site(t, pool, "b")
	types := schema.NewService(pool)
	article, _ := types.Create(ctx, &schema.ContentType{SiteID: siteA, UID: "article", PluralName: "articles", Kind: schema.KindCollection, DisplayName: "Article"}, 0)
	page, _ := types.Create(ctx, &schema.ContentType{SiteID: siteA, UID: "page", PluralName: "pages", Kind: schema.KindCollection, DisplayName: "Page"}, 0)

	super, _ := svc.Create(ctx, users.Input{Email: "s@x.co", Name: "S", Password: "password1234", SuperAdmin: true, Active: true})
	author, _ := svc.Create(ctx, users.Input{Email: "a@x.co", Name: "A", Password: "password1234", Active: true})
	editor, _ := svc.Create(ctx, users.Input{Email: "e@x.co", Name: "E", Password: "password1234", Active: true})

	authorRole := roleByName(t, svc, "Author")
	editorRole := roleByName(t, svc, "Editor")
	if err := svc.SetMemberships(ctx, author.ID, map[int64]int64{siteA: authorRole.ID}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMemberships(ctx, editor.ID, map[int64]int64{siteA: editorRole.ID, siteB: authorRole.ID}); err != nil {
		t.Fatal(err)
	}

	sa, err := svc.SiteAccess(ctx, super, siteB)
	if err != nil || !sa.Can(users.SchemaManage) || !sa.CanContent(users.ContentDelete, article) {
		t.Fatal("super admin must have full access to every site")
	}
	if _, err := svc.SiteAccess(ctx, author, siteB); !errors.Is(err, users.ErrNoAccess) {
		t.Fatalf("non-member access: %v", err)
	}

	aa, err := svc.SiteAccess(ctx, author, siteA)
	if err != nil {
		t.Fatal(err)
	}
	own, other := author.ID, editor.ID
	switch {
	case !aa.CanContent(users.ContentCreate, article):
		t.Fatal("author can create")
	case !aa.OwnOnly(users.ContentUpdate, article):
		t.Fatal("author updates are own-only")
	case !aa.CanEntry(users.ContentUpdate, article, &own):
		t.Fatal("author can update own entry")
	case aa.CanEntry(users.ContentUpdate, article, &other):
		t.Fatal("author must not update others' entries")
	case aa.CanEntry(users.ContentDelete, article, nil):
		t.Fatal("author must not delete entries without an owner")
	case aa.CanContent(users.ContentPublish, article):
		t.Fatal("author cannot publish")
	case aa.Can(users.SchemaManage) || aa.Can(users.MediaDelete):
		t.Fatal("author has no schema or media delete rights")
	case !aa.Can(users.MediaUpload):
		t.Fatal("author can upload")
	}

	ea, _ := svc.SiteAccess(ctx, editor, siteA)
	if !ea.CanEntry(users.ContentDelete, article, &own) || !ea.CanContent(users.ContentPublish, page) {
		t.Fatal("editor manages all content")
	}
	if eb, _ := svc.SiteAccess(ctx, editor, siteB); eb.CanContent(users.ContentPublish, article) {
		t.Fatal("role is per site: editor is only an author on site B")
	}

	// A custom role scoped to one content type.
	custom, err := svc.SaveRole(ctx, &users.Role{Name: "Page writer", Permissions: []users.Permission{
		{Action: users.ContentRead, Subject: schema.PermissionSubject(page.ID)},
		{Action: users.ContentUpdate, Subject: schema.PermissionSubject(page.ID)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.SetMemberships(ctx, author.ID, map[int64]int64{siteA: custom.ID})
	ca, _ := svc.SiteAccess(ctx, author, siteA)
	if !ca.CanEntry(users.ContentUpdate, page, &other) || ca.CanContent(users.ContentRead, article) {
		t.Fatal("custom role subjects not respected")
	}

	if err := svc.DeleteRole(ctx, custom.ID); err == nil {
		t.Fatal("role in use must not be deleted")
	}
	if _, err := svc.SaveRole(ctx, &users.Role{Name: "Bad", Permissions: []users.Permission{{Action: "users.delete"}}}); err == nil {
		t.Fatal("unknown action accepted")
	}
	if _, err := svc.SaveRole(ctx, &users.Role{Name: "Bad", Permissions: []users.Permission{{Action: users.MediaRead, Subject: "*"}}}); err == nil {
		t.Fatal("site action with subject accepted")
	}

	// Deleting a content type removes permissions that target it.
	if err := types.Delete(ctx, siteA, "page"); err != nil {
		t.Fatal(err)
	}
	if r, _ := svc.GetRole(ctx, custom.ID); len(r.Permissions) != 0 {
		t.Fatalf("stale permissions: %+v", r.Permissions)
	}
}
