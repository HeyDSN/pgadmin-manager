package dashboard

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

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
