package dashboard

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

const migrationObjectsQuery = `
SELECT sort_order, kind, identity, owner_name, command
FROM (
    SELECT 10 AS sort_order,
           'Schemas' AS kind,
           format('%I', n.nspname) AS identity,
           pg_get_userbyid(n.nspowner) AS owner_name,
           format('ALTER SCHEMA %I OWNER TO %I', n.nspname, $1::text) AS command
    FROM pg_namespace n
    WHERE n.nspname <> 'information_schema'
      AND n.nspname NOT LIKE 'pg_%'
      AND NOT EXISTS (
          SELECT 1 FROM pg_depend d
          WHERE d.classid = 'pg_namespace'::regclass
            AND d.objid = n.oid AND d.deptype = 'e'
      )

    UNION ALL

    SELECT CASE c.relkind
               WHEN 'r' THEN 20 WHEN 'p' THEN 20
               WHEN 'v' THEN 30 WHEN 'm' THEN 40
               WHEN 'S' THEN 50 WHEN 'f' THEN 60
           END,
           CASE c.relkind
               WHEN 'r' THEN 'Tables' WHEN 'p' THEN 'Partitioned tables'
               WHEN 'v' THEN 'Views' WHEN 'm' THEN 'Materialized views'
               WHEN 'S' THEN 'Sequences' WHEN 'f' THEN 'Foreign tables'
           END,
           format('%I.%I', n.nspname, c.relname),
           pg_get_userbyid(c.relowner),
           format(
               CASE c.relkind
                   WHEN 'r' THEN 'ALTER TABLE %I.%I OWNER TO %I'
                   WHEN 'p' THEN 'ALTER TABLE %I.%I OWNER TO %I'
                   WHEN 'v' THEN 'ALTER VIEW %I.%I OWNER TO %I'
                   WHEN 'm' THEN 'ALTER MATERIALIZED VIEW %I.%I OWNER TO %I'
                   WHEN 'S' THEN 'ALTER SEQUENCE %I.%I OWNER TO %I'
                   WHEN 'f' THEN 'ALTER FOREIGN TABLE %I.%I OWNER TO %I'
               END,
               n.nspname, c.relname, $1::text
           )
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname <> 'information_schema'
      AND n.nspname NOT LIKE 'pg_%'
      AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
      AND NOT EXISTS (
          SELECT 1 FROM pg_depend d
          WHERE d.classid = 'pg_class'::regclass
            AND d.objid = c.oid AND d.deptype = 'e'
      )

    UNION ALL

    SELECT 70,
           CASE p.prokind
               WHEN 'p' THEN 'Procedures'
               WHEN 'a' THEN 'Aggregates'
               ELSE 'Functions'
           END,
           format('%I.%I(%s)', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid)),
           pg_get_userbyid(p.proowner),
           format(
               CASE p.prokind
                   WHEN 'p' THEN 'ALTER PROCEDURE %I.%I(%s) OWNER TO %I'
                   WHEN 'a' THEN 'ALTER AGGREGATE %I.%I(%s) OWNER TO %I'
                   ELSE 'ALTER FUNCTION %I.%I(%s) OWNER TO %I'
               END,
               n.nspname, p.proname, pg_get_function_identity_arguments(p.oid), $1::text
           )
    FROM pg_proc p
    JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname <> 'information_schema'
      AND n.nspname NOT LIKE 'pg_%'
      AND p.prokind IN ('f', 'w', 'p', 'a')
      AND NOT EXISTS (
          SELECT 1 FROM pg_depend d
          WHERE d.classid = 'pg_proc'::regclass
            AND d.objid = p.oid AND d.deptype = 'e'
      )

    UNION ALL

    SELECT 80,
           CASE WHEN t.typtype = 'd' THEN 'Domains' ELSE 'Types' END,
           format('%I.%I', n.nspname, t.typname),
           pg_get_userbyid(t.typowner),
           format(
               CASE WHEN t.typtype = 'd'
                    THEN 'ALTER DOMAIN %I.%I OWNER TO %I'
                    ELSE 'ALTER TYPE %I.%I OWNER TO %I'
               END,
               n.nspname, t.typname, $1::text
           )
    FROM pg_type t
    JOIN pg_namespace n ON n.oid = t.typnamespace
    LEFT JOIN pg_class composite_class ON composite_class.oid = t.typrelid
    WHERE n.nspname <> 'information_schema'
      AND n.nspname NOT LIKE 'pg_%'
      AND (
          (t.typrelid = 0 AND t.typtype IN ('b', 'd', 'e', 'r', 'm')
              AND NOT (t.typtype = 'b' AND t.typelem <> 0))
          OR (t.typtype = 'c' AND composite_class.relkind = 'c')
      )
      AND NOT EXISTS (
          SELECT 1 FROM pg_depend d
          WHERE d.classid = 'pg_type'::regclass
            AND d.objid = t.oid AND d.deptype = 'e'
      )
) objects
ORDER BY sort_order, identity`

func (s *PostgresService) PreviewMigration(ctx context.Context, database string) (*MigrationPlan, error) {
	ownerRole, err := sharedOwnerRole(database)
	if err != nil {
		return nil, err
	}
	conn, err := s.connectDatabase(ctx, database)
	if err != nil {
		return nil, fmt.Errorf("connect to %q: %w", database, err)
	}
	defer conn.Close(ctx)
	return buildMigrationPlan(ctx, conn, database, ownerRole)
}

func buildMigrationPlan(ctx context.Context, q queryer, database, ownerRole string) (*MigrationPlan, error) {
	plan := &MigrationPlan{
		Database: database, SharedOwnerRole: ownerRole, Counts: make(map[string]int),
	}
	if err := q.QueryRow(ctx, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1`, database).Scan(&plan.DatabaseOwner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("database %q does not exist", database)
		}
		return nil, err
	}
	plan.Managed = plan.DatabaseOwner == ownerRole

	owners := map[string]struct{}{plan.DatabaseOwner: {}}
	rows, err := q.Query(ctx, migrationObjectsQuery, ownerRole)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sortOrder int
		var object MigrationObject
		if err := rows.Scan(&sortOrder, &object.Kind, &object.Identity, &object.Owner, &object.command); err != nil {
			return nil, err
		}
		plan.Objects = append(plan.Objects, object)
		plan.Counts[object.Kind]++
		owners[object.Owner] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := q.QueryRow(ctx, `
SELECT count(*) FROM (
    SELECT d.objid FROM pg_depend d
    JOIN pg_class c ON d.classid = 'pg_class'::regclass AND d.objid = c.oid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE d.deptype = 'e' AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg_%'
    UNION ALL
    SELECT d.objid FROM pg_depend d
    JOIN pg_proc p ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
    JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE d.deptype = 'e' AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg_%'
    UNION ALL
    SELECT d.objid FROM pg_depend d
    JOIN pg_type t ON d.classid = 'pg_type'::regclass AND d.objid = t.oid
    JOIN pg_namespace n ON n.oid = t.typnamespace
    WHERE d.deptype = 'e' AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg_%'
) skipped`).Scan(&plan.ExtensionsSkipped); err != nil {
		return nil, err
	}

	delete(owners, ownerRole)
	delete(owners, "postgres")
	for owner := range owners {
		if strings.HasPrefix(owner, "pg_") {
			delete(owners, owner)
		}
	}
	plan.ExistingOwners = sortedKeys(owners)
	return plan, nil
}

func (s *PostgresService) GrantFullAccess(ctx context.Context, database, username string) (*MigrationPlan, error) {
	if err := validateIdentifier("username", username); err != nil {
		return nil, err
	}
	ownerRole, err := sharedOwnerRole(database)
	if err != nil {
		return nil, err
	}

	conn, err := s.connectDatabase(ctx, database)
	if err != nil {
		return nil, fmt.Errorf("connect to %q: %w", database, err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pgadmin-manager:user:' || $1::text, 0))`, username); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pgadmin-manager:database:' || $1::text, 0))`, database); err != nil {
		return nil, err
	}

	var login bool
	if err := tx.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, username).Scan(&login); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("login user %q does not exist", username)
		}
		return nil, err
	}
	if !login {
		return nil, fmt.Errorf("role %q is not a login user", username)
	}

	roleExists, err := validateSharedRole(ctx, tx, ownerRole)
	if err != nil {
		return nil, err
	}
	ownerIdentifier := pgx.Identifier{ownerRole}.Sanitize()
	if !roleExists {
		if _, err := tx.Exec(ctx, "CREATE ROLE "+ownerIdentifier+" NOLOGIN"); err != nil {
			return nil, fmt.Errorf("create shared owner role: %w", err)
		}
	}

	plan, err := buildMigrationPlan(ctx, tx, database, ownerRole)
	if err != nil {
		return nil, err
	}

	for _, oldOwner := range plan.ExistingOwners {
		var createsCycle bool
		if err := tx.QueryRow(ctx, `SELECT pg_has_role($1::name, $2::name, 'MEMBER')`, ownerRole, oldOwner).Scan(&createsCycle); err != nil {
			return nil, fmt.Errorf("check role membership for %q: %w", oldOwner, err)
		}
		if createsCycle {
			return nil, fmt.Errorf("cannot preserve owner %q because role membership would form a cycle", oldOwner)
		}

		oldIdentifier := pgx.Identifier{oldOwner}.Sanitize()
		if _, err := tx.Exec(ctx, "GRANT "+ownerIdentifier+" TO "+oldIdentifier); err != nil {
			return nil, fmt.Errorf("preserve owner %q: %w", oldOwner, err)
		}

		var oldOwnerCanLogin bool
		if err := tx.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, oldOwner).Scan(&oldOwnerCanLogin); err != nil {
			return nil, err
		}
		if oldOwnerCanLogin {
			if _, err := tx.Exec(ctx, alterRoleInDatabaseSQL(oldOwner, database, ownerRole)); err != nil {
				return nil, fmt.Errorf("configure preserved owner %q: %w", oldOwner, err)
			}
		}
	}

	for _, object := range plan.Objects {
		if object.Owner == ownerRole {
			continue
		}
		if _, err := tx.Exec(ctx, object.command); err != nil {
			return nil, fmt.Errorf("migrate %s %s: %w", object.Kind, object.Identity, err)
		}
	}

	userIdentifier := pgx.Identifier{username}.Sanitize()
	if _, err := tx.Exec(ctx, "GRANT "+ownerIdentifier+" TO "+userIdentifier); err != nil {
		return nil, fmt.Errorf("grant shared role: %w", err)
	}
	if _, err := tx.Exec(ctx, alterRoleInDatabaseSQL(username, database, ownerRole)); err != nil {
		return nil, fmt.Errorf("configure automatic role: %w", err)
	}

	databaseIdentifier := pgx.Identifier{database}.Sanitize()
	if _, err := tx.Exec(ctx, "ALTER DATABASE "+databaseIdentifier+" OWNER TO "+ownerIdentifier); err != nil {
		return nil, fmt.Errorf("change database owner: %w", err)
	}
	if _, err := tx.Exec(ctx, "GRANT ALL PRIVILEGES ON DATABASE "+databaseIdentifier+" TO "+ownerIdentifier); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return plan, nil
}

func (s *PostgresService) RevokeFullAccess(ctx context.Context, database, username string) error {
	if err := validateIdentifier("username", username); err != nil {
		return err
	}
	ownerRole, err := sharedOwnerRole(database)
	if err != nil {
		return err
	}

	conn, err := s.connectDatabase(ctx, database)
	if err != nil {
		return fmt.Errorf("connect to %q: %w", database, err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pgadmin-manager:user:' || $1::text, 0))`, username); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pgadmin-manager:database:' || $1::text, 0))`, database); err != nil {
		return err
	}

	var login bool
	if err := tx.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, username).Scan(&login); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("login user %q does not exist", username)
		}
		return err
	}
	if !login {
		return fmt.Errorf("role %q is not a login user", username)
	}

	roleExists, err := validateSharedRole(ctx, tx, ownerRole)
	if err != nil {
		return err
	}
	if !roleExists {
		return fmt.Errorf("database %q has no shared owner role", database)
	}

	var databaseManaged bool
	if err := tx.QueryRow(ctx, `
SELECT database.datdba = shared_role.oid
FROM pg_database database
JOIN pg_roles shared_role ON shared_role.rolname = $2
WHERE database.datname = $1`, database, ownerRole).Scan(&databaseManaged); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("database %q does not exist", database)
		}
		return err
	}
	if !databaseManaged {
		return fmt.Errorf("database %q is not fully managed; refusing ambiguous revoke", database)
	}

	var directMember, effectiveMember bool
	if err := tx.QueryRow(ctx, `
SELECT
    EXISTS (
        SELECT 1
        FROM pg_auth_members membership
        JOIN pg_roles granted_role ON granted_role.oid = membership.roleid
        JOIN pg_roles member_role ON member_role.oid = membership.member
        WHERE granted_role.rolname = $1 AND member_role.rolname = $2
    ),
    pg_has_role($2::name, $1::name, 'MEMBER')`, ownerRole, username).Scan(&directMember, &effectiveMember); err != nil {
		return err
	}
	if !directMember {
		if effectiveMember {
			return fmt.Errorf("user %q inherits full access through another role; direct revoke would not remove access", username)
		}
		return fmt.Errorf("user %q has no full-access mapping to database %q", username, database)
	}

	if _, err := tx.Exec(ctx, alterRoleResetInDatabaseSQL(username, database)); err != nil {
		return fmt.Errorf("reset automatic role: %w", err)
	}
	if _, err := tx.Exec(ctx, "REVOKE "+pgx.Identifier{ownerRole}.Sanitize()+" FROM "+pgx.Identifier{username}.Sanitize()); err != nil {
		return fmt.Errorf("revoke shared role: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func validateSharedRole(ctx context.Context, q queryer, role string) (bool, error) {
	var login, superuser, createDB, createRole, replication, bypassRLS bool
	err := q.QueryRow(ctx, `
SELECT rolcanlogin, rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls
FROM pg_roles WHERE rolname = $1`, role).Scan(&login, &superuser, &createDB, &createRole, &replication, &bypassRLS)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if login || superuser || createDB || createRole || replication || bypassRLS {
		return true, fmt.Errorf("existing shared role %q is privileged or can login; refusing to reuse it", role)
	}
	var inheritsOtherRoles bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM pg_auth_members membership
    JOIN pg_roles member_role ON member_role.oid = membership.member
    WHERE member_role.rolname = $1
)`, role).Scan(&inheritsOtherRoles); err != nil {
		return true, err
	}
	if inheritsOtherRoles {
		return true, fmt.Errorf("existing shared role %q is a member of another role; refusing unsafe privilege inheritance", role)
	}
	return true, nil
}

func alterRoleInDatabaseSQL(username, database, ownerRole string) string {
	return "ALTER ROLE " + pgx.Identifier{username}.Sanitize() +
		" IN DATABASE " + pgx.Identifier{database}.Sanitize() +
		" SET role TO " + quoteLiteral(ownerRole)
}

func alterRoleResetInDatabaseSQL(username, database string) string {
	return "ALTER ROLE " + pgx.Identifier{username}.Sanitize() +
		" IN DATABASE " + pgx.Identifier{database}.Sanitize() +
		" RESET role"
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
