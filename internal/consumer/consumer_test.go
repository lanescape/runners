package consumer_test

import (
	"errors"
	"testing"

	"github.com/lanescape/runners/internal/consumer"
)

func TestTheSpecsKeyExample(t *testing.T) {
	got := consumer.Key("build", []consumer.PathContents{
		{Path: "deps/versions.json", Contents: []byte("café 1.0\n")},
		{Path: "rust-toolchain.toml", Contents: nil},
	}, true)
	if got != "be05417a720fa890bf52" {
		t.Fatalf("key = %s, want be05417a720fa890bf52 (specs/0003-disks.md, Keys)", got)
	}
	if consumer.Key("build", nil, false) != consumer.NoKey {
		t.Fatal("a family declared without from has the key of zeros")
	}
	// Order and paths count.
	swapped := consumer.Key("build", []consumer.PathContents{
		{Path: "rust-toolchain.toml", Contents: nil},
		{Path: "deps/versions.json", Contents: []byte("café 1.0\n")},
	}, true)
	if swapped == got {
		t.Fatal("the key ignores the order of the files")
	}
}

func TestParse(t *testing.T) {
	f, err := consumer.Parse([]byte(`{"branches": {"support/v1": "v1"}, "disks": {"build": {"from": ["a/b", "c"]}, "tools": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Branches["support/v1"] != "v1" || len(f.Disks["build"].From) != 2 || f.Disks["tools"].From != nil {
		t.Fatalf("parsed %+v", f)
	}
	for _, bad := range []string{
		``, `[]`, `null`, `{"Disks": {}}`, `{"bootstrap": "x"}`, `{"disks": []}`, `{"disks": {"build": {"from": "a"}}}`,
		`{"disks": {"build": {"from": ["/a"]}}}`, `{"disks": {"build": {"from": ["a/../b"]}}}`, `{"disks": {"build": {"from": ["a", "a"]}}}`,
		`{"disks": {"build": {"from": ["a\\b"]}}}`, `{"disks": {"build": {"other": 1}}}`, `{"disks": {"Build": {}}}`,
		`{"disks": {"Spot": {}}}`, `{"disks": {"a_b": {}}}`, `{} {}`,
		// There are no consumer images: "images" is an unknown field like
		// any other (specs/0005-interface.md, A consumer's files).
		`{"images": {}}`, `{"images": {"build": ".github/images/build"}}`, `{"disks": {}, "images": {}}`,
	} {
		if _, err := consumer.Parse([]byte(bad)); !errors.Is(err, consumer.ErrInvalid) {
			t.Errorf("Parse(%s) = %v, want invalid", bad, err)
		}
	}
}

func TestBranchScopes(t *testing.T) {
	f, err := consumer.Parse([]byte(`{"branches": {"main": "x", "support/v1": "v1", "a": "dup", "b": "dup", "c": "Bad", "d": 3}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, problems := f.BranchScopes("main")
	if len(got) != 1 || got["support/v1"] != "v1" || len(problems) != 4 {
		t.Fatalf("branches %v, problems %q", got, problems)
	}
}
