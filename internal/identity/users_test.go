package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/identity"
)

func TestCreateUserGeneratesTemporaryPasswordOnce(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	store.addUser(t, authz.RoleFull, false)
	service, _ := newTestService(t, store, 10)
	ctx := context.Background()

	result, err := service.CreateUser(ctx, identity.CreateUserInput{
		Username: "bob", Role: authz.RoleReadOnly, ActorUsername: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.GeneratedPassword == "" || len(result.GeneratedPassword) < 12 {
		t.Fatalf("generated password missing or too short: %q", result.GeneratedPassword)
	}
	if !result.User.MustChangePassword || result.User.Username != "bob" || result.User.Role != authz.RoleReadOnly {
		t.Fatalf("unexpected created user: %+v", result.User)
	}
	matched, err := identity.VerifyPassword(store.users["bob"].PasswordHash, result.GeneratedPassword)
	if err != nil || !matched {
		t.Fatalf("generated password does not match stored hash: %v", err)
	}
	if len(store.events) != 1 || store.events[0].Action != identity.EventUserCreated ||
		store.events[0].ActorUsername != "alice" || store.events[0].TargetUsername != "bob" {
		t.Fatalf("unexpected audit event: %+v", store.events)
	}
}

func TestCreateUserProvidedPasswordAndConflicts(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	store.addUser(t, authz.RoleFull, false)
	service, _ := newTestService(t, store, 10)
	ctx := context.Background()

	provided := "administrator-supplied-secret"
	result, err := service.CreateUser(ctx, identity.CreateUserInput{
		Username: "bob", Role: authz.RoleOperator, Password: provided, ActorUsername: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.GeneratedPassword != "" || result.User.MustChangePassword {
		t.Fatalf("provided password must not generate one: %+v", result)
	}
	if _, err := service.CreateUser(ctx, identity.CreateUserInput{
		Username: "bob", Role: authz.RoleReadOnly, ActorUsername: "alice",
	}); !errors.Is(err, identity.ErrUsernameTaken) {
		t.Fatalf("duplicate username: %v", err)
	}
	if _, err := service.CreateUser(ctx, identity.CreateUserInput{
		Username: "carol", Role: authz.RoleReadOnly, Password: "short", ActorUsername: "alice",
	}); !errors.Is(err, identity.ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	if _, err := service.CreateUser(ctx, identity.CreateUserInput{
		Username: "Bad.Name", Role: authz.RoleReadOnly, ActorUsername: "alice",
	}); !errors.Is(err, identity.ErrInvalidUsername) {
		t.Fatalf("invalid username: %v", err)
	}
}

func TestUpdateUserProtectsSelfAndLastAdmin(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	alice := store.addUser(t, authz.RoleFull, false)
	store.addNamedUser(t, "bob", authz.RoleReadOnly, false) // a second, distinct user
	service, _ := newTestService(t, store, 10)
	ctx := context.Background()
	toReadOnly := authz.RoleReadOnly
	yes := true

	if _, err := service.UpdateUser(ctx, alice.ID, alice.ID, "alice",
		identity.UserUpdate{Role: &toReadOnly}, "", ""); !errors.Is(err, identity.ErrCannotModifySelf) {
		t.Fatalf("self demotion: %v", err)
	}
	if _, err := service.UpdateUser(ctx, alice.ID, alice.ID, "alice",
		identity.UserUpdate{Disabled: &yes}, "", ""); !errors.Is(err, identity.ErrCannotModifySelf) {
		t.Fatalf("self disable: %v", err)
	}
	name := "Alice Updated"
	if _, err := service.UpdateUser(ctx, alice.ID, alice.ID, "alice",
		identity.UserUpdate{DisplayName: &name}, "", ""); err != nil {
		t.Fatalf("self display name: %v", err)
	}
	if _, err := service.UpdateUser(ctx, alice.ID, "actor-2", "admin",
		identity.UserUpdate{Disabled: &yes}, "", ""); !errors.Is(err, identity.ErrLastAdmin) {
		t.Fatalf("disabling last admin: %v", err)
	}
	if _, err := service.UpdateUser(ctx, alice.ID, "actor-2", "admin",
		identity.UserUpdate{Role: &toReadOnly}, "", ""); !errors.Is(err, identity.ErrLastAdmin) {
		t.Fatalf("demoting last admin: %v", err)
	}
}

func TestUpdateUserAllowsDemotionWhenAnotherAdminExists(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	alice := store.addUser(t, authz.RoleFull, false)
	bob := store.addNamedUser(t, "bob", authz.RoleFull, false)
	service, _ := newTestService(t, store, 10)
	toReadOnly := authz.RoleReadOnly

	view, err := service.UpdateUser(context.Background(), alice.ID, bob.ID, "bob",
		identity.UserUpdate{Role: &toReadOnly}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Role != authz.RoleReadOnly || store.users["alice"].Role != authz.RoleReadOnly {
		t.Fatalf("demotion did not persist: %+v", view)
	}
	view, err = service.GetUser(context.Background(), bob.ID)
	if err != nil || view.Role != authz.RoleFull {
		t.Fatalf("other admin changed: %+v, %v", view, err)
	}
}

func TestResetPasswordRevokesAllSessionsAndForcesChange(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	actor := store.addUser(t, authz.RoleFull, false)
	hash, err := identity.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	target := &identity.User{
		ID: "user-bob", Username: "bob", PasswordHash: hash, Role: authz.RoleReadOnly,
	}
	store.users["bob"] = target
	store.byID[target.ID] = target
	service, _ := newTestService(t, store, 10)
	ctx := context.Background()

	login, err := service.Login(ctx, "bob", testPassword, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	oldHash := target.PasswordHash
	generated, err := service.ResetPassword(ctx, target.ID, actor.Username, "", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if generated == "" || target.PasswordHash == oldHash || !target.MustChangePassword {
		t.Fatal("reset did not install a new password and force a change")
	}
	if _, err := service.Refresh(ctx, login.RefreshToken, "", "", ""); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("refresh token survived password reset: %v", err)
	}
	event := store.events[len(store.events)-1]
	if event.Action != identity.EventPasswordReset || event.ActorUsername != "alice" || event.TargetUsername != "bob" {
		t.Fatalf("unexpected reset audit: %+v", event)
	}
}

func TestUserViewsNeverExposePasswordHash(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	store.addUser(t, authz.RoleFull, false)
	service, _ := newTestService(t, store, 10)
	ctx := context.Background()

	view, err := service.GetUser(ctx, store.users["alice"].ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	users, _, err := service.ListUsers(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	listJSON, err := json.Marshal(users)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{string(encoded), string(listJSON)} {
		if strings.Contains(body, "password_hash") || strings.Contains(body, sampleHash) {
			t.Fatalf("password hash leaked in user view: %s", body)
		}
	}
}
