package dashboard

import (
	"context"
	"strings"
	"testing"
)

func TestBuiltInDatabasesAreProtectedBeforeConnecting(t *testing.T) {
	service := &PostgresService{}
	for _, database := range []string{"postgres", "template0", "template1"} {
		t.Run(database, func(t *testing.T) {
			ctx := context.Background()
			if err := service.CreateDatabase(ctx, database); err == nil {
				t.Fatal("creating a built-in database was not rejected")
			}
			if _, err := service.PreviewMigration(ctx, database); err == nil {
				t.Fatal("previewing a built-in database was not rejected")
			}
			if _, err := service.GrantFullAccess(ctx, database, "user"); err == nil {
				t.Fatal("granting built-in database access was not rejected")
			}
			if err := service.RevokeFullAccess(ctx, database, "user"); err == nil {
				t.Fatal("revoking built-in database access was not rejected")
			}
		})
	}
}

func TestSharedOwnerRole(t *testing.T) {
	t.Parallel()

	role, err := sharedOwnerRole("project_x")
	if err != nil {
		t.Fatalf("sharedOwnerRole returned error: %v", err)
	}
	if role != "project_x__owner" {
		t.Fatalf("got %q, want %q", role, "project_x__owner")
	}
}

func TestSharedOwnerRoleEnforcesByteLimit(t *testing.T) {
	t.Parallel()

	_, err := sharedOwnerRole(strings.Repeat("a", 57))
	if err == nil {
		t.Fatal("expected generated owner role length error")
	}
	_, err = sharedOwnerRole(strings.Repeat("é", 32))
	if err == nil {
		t.Fatal("expected multibyte database name length error")
	}
}

func TestQuoteLiteral(t *testing.T) {
	t.Parallel()

	if got, want := quoteLiteral("team's_db"), "'team''s_db'"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestValidateIdentifierRejectsNullByte(t *testing.T) {
	t.Parallel()

	if err := validateIdentifier("username", "dev\x00user"); err == nil {
		t.Fatal("expected null-byte validation error")
	}
}
