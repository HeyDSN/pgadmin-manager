package dashboard

import (
	"bytes"
	"errors"
	"html/template"
	"reflect"
	"strings"
	"testing"
)

func TestDashboardOverviewRanksByBytesAndPreservesInventory(t *testing.T) {
	users := []User{
		{Name: "admin", Superuser: true},
		{Name: "alice", FullAccessDatabases: []string{"large", "small"}},
		{Name: "bob"},
	}
	databases := []Database{
		{Name: "small", Size: "900 kB", SizeBytes: 900 * 1024, Status: "Managed"},
		{Name: "large", Size: "10 MB", SizeBytes: 10 * 1024 * 1024, Status: "Managed"},
		{Name: "postgres", SizeBytes: 1, Status: "Legacy"},
		{Name: "partial", SizeBytes: 2, Status: "Partially Managed"},
		{Name: "empty-a", Status: "Managed"},
		{Name: "empty-b", Status: "Managed"},
	}
	var data PageData
	populateDashboard(&data, users, databases)
	if len(data.LargestDatabases) != 5 || data.LargestDatabases[0].Name != "large" || data.LargestDatabases[1].Name != "small" {
		t.Fatalf("unexpected largest databases: %#v", data.LargestDatabases)
	}
	if data.TotalDatabases != 6 || data.ManagedDatabases != 4 || data.LegacyDatabases != 2 || data.ManagedPercent != 66 {
		t.Fatalf("unexpected ownership counts: %#v", data)
	}
	if len(data.ReviewDatabases) != 2 || data.RegularUsers != 2 || data.SuperuserUsers != 1 || data.Users[0].Name != "alice" || !data.Users[2].Superuser {
		t.Fatalf("unexpected review or users: %#v", data)
	}
	if databases[0].Name != "small" || users[0].Name != "admin" {
		t.Fatal("dashboard sorting changed the original inventory")
	}
}

func TestDashboardTemplateHandlesUnavailableAndEscapesDatabaseNames(t *testing.T) {
	tmpl, err := template.ParseFS(webFiles, "web/templates/layout.html", "web/templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data PageData
		want string
	}{
		{"unavailable", PageData{}, "Database inventory unavailable"},
		{"empty", PageData{UsersAvailable: true, DatabasesAvailable: true}, "No databases found"},
		{"escaped", PageData{DatabasesAvailable: true, LargestDatabases: []Database{{Name: "<script>alert(1)</script>", Status: "Managed"}}}, "&lt;script&gt;alert(1)&lt;/script&gt;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, "layout", tc.data); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("rendered page missing %q", tc.want)
			}
			if strings.Contains(buf.String(), "<script>alert(1)</script>") {
				t.Fatal("database name rendered as executable HTML")
			}
		})
	}
}

func TestSharedLayoutShowsPageContextWithoutLiveBadge(t *testing.T) {
	for _, page := range []struct {
		name, active, subtitle string
	}{
		{"dashboard", "dashboard", "Database ownership &amp; access overview"},
		{"users", "users", "Login users &amp; database access overview"},
		{"databases", "databases", "Database inventory &amp; shared ownership overview"},
	} {
		t.Run(page.name, func(t *testing.T) {
			tmpl, err := template.ParseFS(webFiles, "web/templates/layout.html", "web/templates/"+page.name+".html")
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, "layout", PageData{Active: page.active, ServerStatus: "Connected"}); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{page.subtitle, "PostgreSQL · Connected", "/static/layout.css"} {
				if !strings.Contains(buf.String(), want) {
					t.Fatalf("missing %q", want)
				}
			}
			if strings.Contains(buf.String(), "LIVE") {
				t.Fatal("obsolete LIVE badge still rendered")
			}
		})
	}
}

func TestSyncMappingsGrantsBeforeRevokes(t *testing.T) {
	t.Parallel()

	var actions []string
	err := syncMappings(
		[]string{"keep", "remove"},
		[]string{"keep", "add"},
		func(value string) error { actions = append(actions, "grant:"+value); return nil },
		func(value string) error { actions = append(actions, "revoke:"+value); return nil },
	)
	if err != nil {
		t.Fatalf("syncMappings returned error: %v", err)
	}
	want := []string{"grant:add", "revoke:remove"}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("actions = %#v, want %#v", actions, want)
	}
}

func TestSyncMappingsReportsPartialCompletion(t *testing.T) {
	t.Parallel()

	err := syncMappings(
		nil,
		[]string{"first", "second"},
		func(value string) error {
			if value == "second" {
				return errors.New("boom")
			}
			return nil
		},
		func(string) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "1 earlier change(s) completed") {
		t.Fatalf("expected partial-completion error, got %v", err)
	}
}
