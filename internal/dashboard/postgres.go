package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxIdentifierBytes = 63
	appName            = "pgadmin-manager"
)

type PostgresService struct {
	pool       *pgxpool.Pool
	baseConfig *pgx.ConnConfig
}

type User struct {
	Name                string
	Superuser           bool
	CreateDB            bool
	CreateRole          bool
	FullAccessDatabases []string
}

type Database struct {
	Name            string
	Owner           string
	Size            string
	SizeBytes       int64
	SharedOwnerRole string
	FullAccessUsers []string
	Status          string
}

type Connection struct {
	PID         int
	Database    string
	User        string
	Application string
	Client      string
	State       string
	Since       string
	Query       string
	Wait        string
	BlockedBy   []int32
	IdleTxLong  bool
}

type ServerHealth struct {
	Version        string
	Uptime         string
	Connections    int
	MaxConnections int
	CachePercent   string
	TotalSize      string
	UsedPercent    int
}

type MigrationObject struct {
	Kind     string
	Identity string
	Owner    string
	command  string
}

type MigrationPlan struct {
	Database          string
	SharedOwnerRole   string
	Managed           bool
	DatabaseOwner     string
	ExistingOwners    []string
	Objects           []MigrationObject
	Counts            map[string]int
	ExtensionsSkipped int
}

func NewPostgresService(ctx context.Context) (*PostgresService, error) {
	connectionString := os.Getenv("DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(connectionString)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL configuration: %w", err)
	}
	cfg.MaxConns = 5
	cfg.MinConns = 0
	cfg.ConnConfig.RuntimeParams["application_name"] = appName

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}

	return &PostgresService{pool: pool, baseConfig: cfg.ConnConfig.Copy()}, nil
}

func (s *PostgresService) Close() { s.pool.Close() }

func (s *PostgresService) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *PostgresService) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
WITH RECURSIVE membership AS (
    SELECT roleid, member FROM pg_auth_members
    UNION
    SELECT m.roleid, membership.member
    FROM pg_auth_members m
    JOIN membership ON membership.roleid = m.member
), managed AS (
    SELECT d.datname, owner_role.oid AS role_oid
    FROM pg_database d
    JOIN pg_roles owner_role
      ON owner_role.rolname = d.datname || '__owner'
     AND owner_role.oid = d.datdba
    WHERE NOT d.datistemplate
)
SELECT r.rolname, r.rolsuper, r.rolcreatedb, r.rolcreaterole,
       COALESCE(
           array_agg(DISTINCT managed.datname::text ORDER BY managed.datname::text)
               FILTER (WHERE managed.datname IS NOT NULL),
           ARRAY[]::text[]
       )
FROM pg_roles r
LEFT JOIN membership ON membership.member = r.oid
LEFT JOIN managed ON managed.role_oid = membership.roleid
WHERE r.rolcanlogin
GROUP BY r.oid, r.rolname, r.rolsuper, r.rolcreatedb, r.rolcreaterole
ORDER BY r.rolname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.Name, &user.Superuser, &user.CreateDB, &user.CreateRole, &user.FullAccessDatabases); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *PostgresService) ListDatabases(ctx context.Context) ([]Database, error) {
	rows, err := s.pool.Query(ctx, `
WITH RECURSIVE membership AS (
    SELECT roleid, member FROM pg_auth_members
    UNION
    SELECT m.roleid, membership.member
    FROM pg_auth_members m
    JOIN membership ON membership.roleid = m.member
), database_sizes AS MATERIALIZED (
    SELECT oid, pg_database_size(oid) AS bytes
    FROM pg_database
    WHERE NOT datistemplate AND datname <> 'postgres'
)
SELECT d.datname,
       pg_get_userbyid(d.datdba),
       pg_size_pretty(database_sizes.bytes),
       database_sizes.bytes,
       COALESCE(owner_role.rolname, '-'),
       COALESCE(
           array_agg(DISTINCT member_role.rolname::text ORDER BY member_role.rolname::text)
               FILTER (WHERE member_role.rolcanlogin),
           ARRAY[]::text[]
       ),
       CASE
           WHEN owner_role.oid = d.datdba THEN 'Managed'
           WHEN owner_role.oid IS NOT NULL THEN 'Partially Managed'
           ELSE 'Legacy'
       END
FROM pg_database d
JOIN database_sizes ON database_sizes.oid = d.oid
LEFT JOIN pg_roles owner_role ON owner_role.rolname = d.datname || '__owner'
LEFT JOIN membership ON membership.roleid = owner_role.oid AND owner_role.oid = d.datdba
LEFT JOIN pg_roles member_role ON member_role.oid = membership.member
WHERE NOT d.datistemplate AND d.datname <> 'postgres'
GROUP BY d.datname, d.datdba, database_sizes.bytes, owner_role.oid, owner_role.rolname
ORDER BY d.datname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var databases []Database
	for rows.Next() {
		var database Database
		if err := rows.Scan(&database.Name, &database.Owner, &database.Size, &database.SizeBytes, &database.SharedOwnerRole, &database.FullAccessUsers, &database.Status); err != nil {
			return nil, err
		}
		databases = append(databases, database)
	}
	return databases, rows.Err()
}

func (s *PostgresService) CreateUser(ctx context.Context, username, password string) error {
	if err := validateIdentifier("username", username); err != nil {
		return err
	}
	if password == "" {
		return errors.New("password cannot be empty")
	}
	if strings.IndexByte(password, 0) >= 0 {
		return errors.New("password contains an invalid null byte")
	}

	var statement string
	err := s.pool.QueryRow(ctx, `SELECT format(
        'CREATE ROLE %I LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD %L',
        $1::text, $2::text
    )`, username, password).Scan(&statement)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, statement); err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (s *PostgresService) ChangeUserPassword(ctx context.Context, username, password string) error {
	if err := validateIdentifier("username", username); err != nil {
		return err
	}
	if password == "" {
		return errors.New("password cannot be empty")
	}
	if strings.IndexByte(password, 0) >= 0 {
		return errors.New("password contains an invalid null byte")
	}

	var login, superuser, currentUser bool
	err := s.pool.QueryRow(ctx, `SELECT rolcanlogin, rolsuper, rolname = current_user FROM pg_roles WHERE rolname = $1`, username).Scan(&login, &superuser, &currentUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("login user %q does not exist", username)
	}
	if err != nil {
		return err
	}
	if !login {
		return fmt.Errorf("role %q is not a login user", username)
	}
	if superuser || currentUser {
		return fmt.Errorf("refusing to change password of administrative user %q", username)
	}

	var statement string
	if err := s.pool.QueryRow(ctx, `SELECT format('ALTER ROLE %I PASSWORD %L', $1::text, $2::text)`, username, password).Scan(&statement); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, statement); err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	return nil
}

func (s *PostgresService) DeleteUser(ctx context.Context, username string) error {
	if err := validateIdentifier("username", username); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pgadmin-manager:user:' || $1::text, 0))`, username); err != nil {
		return err
	}

	var login, superuser, currentUser bool
	if err := tx.QueryRow(ctx, `
SELECT rolcanlogin, rolsuper, rolname = current_user
FROM pg_roles
WHERE rolname = $1`, username).Scan(&login, &superuser, &currentUser); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("login user %q does not exist", username)
		}
		return err
	}
	if !login {
		return fmt.Errorf("role %q is not a login user", username)
	}
	if superuser || currentUser {
		return fmt.Errorf("refusing to delete administrative user %q", username)
	}

	mappedDatabases, err := mappedDatabasesForUser(ctx, tx, username)
	if err != nil {
		return err
	}
	if len(mappedDatabases) > 0 {
		return fmt.Errorf(
			"user %q still has shared full access to: %s; revoke every mapping first",
			username,
			strings.Join(mappedDatabases, ", "),
		)
	}

	var ownedDependencies int
	if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM pg_shdepend
WHERE refclassid = 'pg_authid'::regclass
  AND refobjid = (SELECT oid FROM pg_roles WHERE rolname = $1)
  AND deptype = 'o'`, username).Scan(&ownedDependencies); err != nil {
		return err
	}
	if ownedDependencies > 0 {
		return fmt.Errorf(
			"user %q still owns %d database object(s); reassign ownership before deleting the user",
			username,
			ownedDependencies,
		)
	}

	databases, err := connectableDatabases(ctx, tx)
	if err != nil {
		return err
	}
	cleanedDatabases := 0
	for _, database := range databases {
		conn, err := s.connectDatabase(ctx, database)
		if err != nil {
			return deleteCleanupError(username, cleanedDatabases, database, err)
		}
		_, cleanupErr := conn.Exec(ctx, "DROP OWNED BY "+pgx.Identifier{username}.Sanitize())
		closeErr := conn.Close(ctx)
		if cleanupErr != nil {
			return deleteCleanupError(username, cleanedDatabases, database, cleanupErr)
		}
		if closeErr != nil {
			return deleteCleanupError(username, cleanedDatabases, database, closeErr)
		}
		cleanedDatabases++
	}

	if _, err := tx.Exec(ctx, "DROP ROLE "+pgx.Identifier{username}.Sanitize()); err != nil {
		return fmt.Errorf("ACL cleanup completed across %d database(s), but deleting user %q failed: %w", cleanedDatabases, username, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func connectableDatabases(ctx context.Context, q queryer) ([]string, error) {
	rows, err := q.Query(ctx, `
SELECT datname
FROM pg_database
WHERE datallowconn
ORDER BY datname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var database string
		if err := rows.Scan(&database); err != nil {
			return nil, err
		}
		databases = append(databases, database)
	}
	return databases, rows.Err()
}

func deleteCleanupError(username string, completed int, database string, err error) error {
	if completed > 0 {
		return fmt.Errorf(
			"ACL cleanup for user %q completed in %d database(s), then failed in %q: %w",
			username,
			completed,
			database,
			err,
		)
	}
	return fmt.Errorf("clean ACL dependencies for user %q in database %q: %w", username, database, err)
}

func mappedDatabasesForUser(ctx context.Context, q queryer, username string) ([]string, error) {
	rows, err := q.Query(ctx, `
WITH RECURSIVE membership AS (
    SELECT roleid, member FROM pg_auth_members
    UNION
    SELECT parent.roleid, membership.member
    FROM pg_auth_members parent
    JOIN membership ON membership.roleid = parent.member
)
SELECT DISTINCT database.datname
FROM pg_roles login_role
JOIN membership ON membership.member = login_role.oid
JOIN pg_roles shared_role ON shared_role.oid = membership.roleid
JOIN pg_database database ON shared_role.rolname = database.datname || '__owner'
WHERE login_role.rolname = $1
ORDER BY database.datname`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var database string
		if err := rows.Scan(&database); err != nil {
			return nil, err
		}
		databases = append(databases, database)
	}
	return databases, rows.Err()
}

func (s *PostgresService) CreateDatabase(ctx context.Context, database string) error {
	if err := validateIdentifier("database", database); err != nil {
		return err
	}
	ownerRole, err := sharedOwnerRole(database)
	if err != nil {
		return err
	}

	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, database).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("database %q already exists", database)
	}
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, ownerRole).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("shared owner role %q already exists", ownerRole)
	}

	ownerIdentifier := pgx.Identifier{ownerRole}.Sanitize()
	if _, err := s.pool.Exec(ctx, "CREATE ROLE "+ownerIdentifier+" NOLOGIN"); err != nil {
		return fmt.Errorf("create shared owner role: %w", err)
	}

	databaseIdentifier := pgx.Identifier{database}.Sanitize()
	if _, err := s.pool.Exec(ctx, "CREATE DATABASE "+databaseIdentifier+" OWNER "+ownerIdentifier); err != nil {
		if _, cleanupErr := s.pool.Exec(ctx, "DROP ROLE "+ownerIdentifier); cleanupErr != nil {
			return fmt.Errorf("create database: %w; cleanup shared role: %v", err, cleanupErr)
		}
		return fmt.Errorf("create database: %w", err)
	}

	conn, err := s.connectDatabase(ctx, database)
	if err != nil {
		return fmt.Errorf("database created but public schema setup failed: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "ALTER SCHEMA public OWNER TO "+ownerIdentifier); err != nil {
		return fmt.Errorf("database created but public schema setup failed: %w", err)
	}
	return nil
}

func (s *PostgresService) DeleteDatabase(ctx context.Context, database string) error {
	ownerRole, err := sharedOwnerRole(database)
	if err != nil {
		return err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1 AND NOT datistemplate)`, database).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("database %q does not exist", database)
	}
	if _, err := s.pool.Exec(ctx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		return fmt.Errorf("drop database: %w", err)
	}

	var roleExists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1 AND NOT rolcanlogin)`, ownerRole).Scan(&roleExists); err != nil {
		return fmt.Errorf("database dropped, but checking shared role %q failed: %w", ownerRole, err)
	}
	if roleExists {
		if _, err := s.pool.Exec(ctx, "DROP ROLE "+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
			return fmt.Errorf("database dropped, but removing shared role %q failed: %w", ownerRole, err)
		}
	}
	return nil
}

const clientSessions = `backend_type = 'client backend' AND pid <> pg_backend_pid() AND application_name <> '` + appName + `'`

func (s *PostgresService) ListConnections(ctx context.Context) ([]Connection, error) {
	rows, err := s.pool.Query(ctx, `
SELECT pid, COALESCE(datname, ''), COALESCE(usename, ''), COALESCE(application_name, ''),
	   COALESCE(client_addr::text, 'local'), COALESCE(state, ''),
	   COALESCE(extract(epoch FROM now() - state_change)::bigint, 0),
       regexp_replace(left(COALESCE(query, ''), 200), '\s+', ' ', 'g'),
       COALESCE(wait_event_type || ':' || wait_event, ''),
       pg_blocking_pids(pid)
FROM pg_stat_activity
WHERE `+clientSessions+`
ORDER BY datname, state_change DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var connections []Connection
	for rows.Next() {
		var c Connection
		var seconds int64
		if err := rows.Scan(&c.PID, &c.Database, &c.User, &c.Application, &c.Client, &c.State, &seconds, &c.Query, &c.Wait, &c.BlockedBy); err != nil {
			return nil, err
		}
		c.Since = formatDuration(seconds)
		c.IdleTxLong = strings.HasPrefix(c.State, "idle in transaction") && seconds >= 60
		connections = append(connections, c)
	}
	return connections, rows.Err()
}

func (s *PostgresService) TerminateConnection(ctx context.Context, pid int) error {
	var terminated bool
	err := s.pool.QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid = $1 AND `+clientSessions, pid).Scan(&terminated)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("connection %d no longer exists", pid)
	}
	if err != nil {
		return err
	}
	if !terminated {
		return fmt.Errorf("could not terminate connection %d", pid)
	}
	return nil
}

func (s *PostgresService) TerminateDatabaseConnections(ctx context.Context, database string) (int, error) {
	if err := validateIdentifier("database", database); err != nil {
		return 0, err
	}
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND `+clientSessions+`) t`, database).Scan(&count)
	return count, err
}

func (s *PostgresService) ServerHealth(ctx context.Context) (ServerHealth, error) {
	var h ServerHealth
	var uptime int64
	err := s.pool.QueryRow(ctx, `
SELECT current_setting('server_version'),
       extract(epoch FROM now() - pg_postmaster_start_time())::bigint,
       (SELECT count(*)::int FROM pg_stat_activity WHERE backend_type = 'client backend'),
       current_setting('max_connections')::int,
       COALESCE(round(100.0 * sum(blks_hit) / NULLIF(sum(blks_hit) + sum(blks_read), 0), 1)::text, 'n/a'),
       (SELECT pg_size_pretty(sum(pg_database_size(oid))) FROM pg_database)
FROM pg_stat_database`).Scan(&h.Version, &uptime, &h.Connections, &h.MaxConnections, &h.CachePercent, &h.TotalSize)
	if err != nil {
		return h, err
	}
	h.Version, _, _ = strings.Cut(h.Version, " ")
	h.Uptime = formatDuration(uptime)
	if h.MaxConnections > 0 {
		h.UsedPercent = h.Connections * 100 / h.MaxConnections
	}
	return h, nil
}

func formatDuration(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	case seconds < 86400:
		return fmt.Sprintf("%dh %dm", seconds/3600, seconds%3600/60)
	}
	return fmt.Sprintf("%dd %dh", seconds/86400, seconds%86400/3600)
}

func (s *PostgresService) Endpoint() string {
	host := s.baseConfig.Host
	if host == "" || strings.HasPrefix(host, "/") {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(int(s.baseConfig.Port)))
}

func connectionURL(endpoint, user, database string) string {
	if endpoint == "" {
		return ""
	}
	u := url.URL{Scheme: "postgresql", User: url.User(user), Host: endpoint, Path: "/" + database}
	return u.String()
}

func validateIdentifier(label, value string) error {
	if value == "" {
		return fmt.Errorf("%s cannot be empty", label)
	}
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s contains invalid characters", label)
	}
	if len([]byte(value)) > maxIdentifierBytes {
		return fmt.Errorf("%s exceeds PostgreSQL's %d-byte identifier limit", label, maxIdentifierBytes)
	}
	return nil
}

func sharedOwnerRole(database string) (string, error) {
	if database == "postgres" || database == "template0" || database == "template1" {
		return "", fmt.Errorf("built-in database %q is protected from management", database)
	}
	if err := validateIdentifier("database", database); err != nil {
		return "", err
	}
	role := database + "__owner"
	if len([]byte(role)) > maxIdentifierBytes {
		return "", fmt.Errorf("generated shared owner role exceeds PostgreSQL's %d-byte identifier limit", maxIdentifierBytes)
	}
	return role, nil
}

func (s *PostgresService) connectDatabase(ctx context.Context, database string) (*pgx.Conn, error) {
	cfg := s.baseConfig.Copy()
	cfg.Database = database
	return pgx.ConnectConfig(ctx, cfg)
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
