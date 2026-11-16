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
		t.Fatal("a cache declared without from has the key of zeros")
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

// A key made one file at a time is the key made from the list.
func TestAKeyMadeFileByFile(t *testing.T) {
	k := consumer.NewKeyHash("build")
	k.Add("deps/versions.json", []byte("café 1.0\n"))
	k.Add("rust-toolchain.toml", nil)
	if got := k.Sum(); got != "be05417a720fa890bf52" {
		t.Fatalf("key = %s, want be05417a720fa890bf52 (specs/0003-disks.md, Keys)", got)
	}
	// A cache that lists no files has a key of its name alone.
	if consumer.NewKeyHash("build").Sum() != consumer.Key("build", nil, true) {
		t.Fatal("an empty list's key differs")
	}
}

func TestParse(t *testing.T) {
	f, err := consumer.Parse([]byte(`{"branches": {"support/v1": "v1"}, "caches": {"build": {"from": ["a/b", "c"]}, "tools": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Branches["support/v1"] != "v1" || len(f.Caches["build"].From) != 2 || f.Caches["tools"].From != nil {
		t.Fatalf("parsed %+v", f)
	}
	// A cache that lists no files has a key of its name alone; one
	// declared {} has the key of zeros.
	f, err = consumer.Parse([]byte(`{"caches": {"none": {"from": []}, "zero": {}}}`))
	if err != nil || f.Caches["none"].From == nil || f.Caches["zero"].From != nil {
		t.Fatalf("parsed %+v, %v", f, err)
	}
	for _, bad := range []string{
		``, `[]`, `null`, `{"Caches": {}}`, `{"bootstrap": "x"}`, `{"caches": []}`, `{"caches": {"build": {"from": "a"}}}`,
		`{"caches": {"build": {"from": ["/a"]}}}`, `{"caches": {"build": {"from": ["a/../b"]}}}`, `{"caches": {"build": {"from": ["a", "a"]}}}`,
		`{"caches": {"build": {"from": ["a\\b"]}}}`, `{"caches": {"build": {"other": 1}}}`, `{"caches": {"Build": {}}}`,
		`{"caches": {"Spot": {}}}`, `{"caches": {"a_b": {}}}`, `{} {}`,
		// There are no consumer images or saved disks: "images" and "disks"
		// are unknown fields like any other (specs/0005-interface.md, A
		// consumer's files).
		`{"disks": {}}`, `{"disks": {"build": {}}}`,
		`{"images": {}}`, `{"images": {"build": ".github/images/build"}}`, `{"caches": {}, "images": {}}`,
		// A declaration is exactly {} or {"from": [...]}.
		`{"caches": {"build": null}}`, `{"caches": {"build": {"from": null}}}`, `{"caches": {"build": {"FROM": ["deps.txt"]}}}`,
		`{"caches": {"build": {"From": []}}}`, `{"caches": {"build": []}}`, `{"caches": {"build": {"from": [1]}}}`, `{"branches": null}`,
		`{"caches": null}`,
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
