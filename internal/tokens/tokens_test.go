package tokens_test

import (
	"testing"

	"github.com/lanescape/runners/internal/tokens"
)

func TestRule(t *testing.T) {
	r, err := tokens.Rule("lanescape-runners-caches", "5001/lsr-5001-0123456789abcdef0123-1794781264-jxsew0m4/", tokens.Creator)
	if err != nil {
		t.Fatal(err)
	}
	if r.AvailableResource != "//storage.googleapis.com/projects/_/buckets/lanescape-runners-caches" ||
		len(r.AvailablePermissions) != 1 || r.AvailablePermissions[0] != "inRole:roles/storage.objectCreator" ||
		r.Condition == nil || r.Condition.Expression !=
		"resource.name.startsWith('projects/_/buckets/lanescape-runners-caches/objects/5001/lsr-5001-0123456789abcdef0123-1794781264-jxsew0m4/')" {
		t.Fatalf("rule = %+v, %+v", r, r.Condition)
	}
	// Nothing that could end the condition's string, or widen it, is a
	// prefix; nor is any other role.
	for _, bad := range []string{"", "5001/", "5001/x", "5001/x/y/", "5001/x')||true||('", "../5001/x/", "0/x/"} {
		if _, err := tokens.Rule("b", bad, tokens.Reader); err == nil {
			t.Errorf("Rule(%q) was allowed", bad)
		}
	}
	if _, err := tokens.Rule("b", "5001/x/", "inRole:roles/storage.objectAdmin"); err == nil {
		t.Error("an admin token was allowed")
	}
}
