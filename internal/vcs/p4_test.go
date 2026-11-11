package vcs

import (
	"reflect"
	"testing"
)

func TestP4MapOpenedFiles(t *testing.T) {
	p4 := &P4VCS{client: "art"}
	files := []p4Opened{
		{ClientFile: "//art/Game/old.cpp", DepotFile: "//depot/old.cpp", HaveRev: "7", MovedFile: "//depot/new.cpp", Action: "move/delete", Type: "text"},
		{ClientFile: "//art/Game/new.cpp", DepotFile: "//depot/new.cpp", HaveRev: "7", MovedFile: "//depot/old.cpp", Action: "move/add", Type: "text"},
		{ClientFile: "//art/Game/Icon.uasset", DepotFile: "//depot/Icon.uasset", HaveRev: "2", Action: "edit", Type: "binary+l"},
		{ClientFile: "//art/Game/Read%23me.md", DepotFile: "//depot/Read%23me.md", HaveRev: "none", Action: "add", Type: "text"},
		{ClientFile: "//art/Game/obsolete.cpp", DepotFile: "//depot/obsolete.cpp", HaveRev: "4", Action: "delete", Type: "text"},
	}
	got, mapped, err := p4.mapOpenedFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	want := []FileChange{
		{Path: "Game/Icon.uasset", Status: "modified", Binary: true},
		{Path: "Game/Read#me.md", Status: "added"},
		{Path: "Game/new.cpp", OldPath: "Game/old.cpp", Status: "renamed"},
		{Path: "Game/obsolete.cpp", Status: "deleted"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %#v, want %#v", got, want)
	}
	if f := mapped["Game/new.cpp"]; f.baseSpec != "//depot/old.cpp#7" || f.depotPath != "//depot/new.cpp" {
		t.Fatalf("move mapping = %#v", f)
	}
	if got := mapped["Game/obsolete.cpp"].baseSpec; got != "//depot/obsolete.cpp#4" {
		t.Fatalf("deleted file base = %q", got)
	}
}

func TestP4RejectsInvalidClientPath(t *testing.T) {
	p4 := &P4VCS{client: "art"}
	for _, path := range []string{"//other/Game/Item.cpp", "//art/../../Item.cpp"} {
		if _, err := p4.relativePath(path); err == nil {
			t.Errorf("relativePath(%q) unexpectedly succeeded", path)
		}
	}
}

func TestP4DeletionHunksNormalizeCRLF(t *testing.T) {
	hunks := p4DeletionHunks(p4Text([]byte("first\r\nsecond\r\n")))
	if len(hunks) != 1 || hunks[0].Header != "@@ -1,2 +0,0 @@" {
		t.Fatalf("hunks = %#v", hunks)
	}
	if got := hunks[0].Lines[1]; got != (DiffLine{Type: "del", Content: "second", OldNum: 2}) {
		t.Fatalf("second line = %#v", got)
	}
}

func TestP4LocalDiffPreservesLineNumbersWhenIgnoringWhitespace(t *testing.T) {
	hunks := p4LocalDiff("first\n  second\nold\n", "first\nsecond\nnew\n", true)
	if len(hunks) != 1 || len(hunks[0].Lines) != 4 {
		t.Fatalf("hunks = %#v", hunks)
	}
	if line := hunks[0].Lines[1]; line.Type != "context" || line.OldNum != 2 || line.NewNum != 2 {
		t.Fatalf("whitespace-only change = %#v", line)
	}
	if line := hunks[0].Lines[3]; line.Type != "add" || line.NewNum != 3 || line.Content != "new" {
		t.Fatalf("added line = %#v", line)
	}
}
