package monetdroid

import (
	"testing"
)

func TestParseUnifiedDiff_Empty(t *testing.T) {
	files := ParseUnifiedDiff("")
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}

func TestParseUnifiedDiff_SingleFileEdit(t *testing.T) {
	// Edit tool format: no "diff --git" prefix
	diff := `--- pkg/monetdroid/render.go
+++ pkg/monetdroid/render.go
@@ -10,7 +10,8 @@ func foo() {
 	ctx := context.Background()
 	name := "hello"
-	fmt.Println(name)
+	log.Printf("name: %s", name)
+	log.Printf("done")
 	return nil
 }
`
	files := ParseUnifiedDiff(diff)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	f := files[0]
	if f.OldName != "pkg/monetdroid/render.go" {
		t.Errorf("OldName = %q", f.OldName)
	}
	if f.NewName != "pkg/monetdroid/render.go" {
		t.Errorf("NewName = %q", f.NewName)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(f.Hunks))
	}
	h := f.Hunks[0]
	if h.OldStart != 10 || h.OldCount != 7 {
		t.Errorf("old range = %d,%d", h.OldStart, h.OldCount)
	}
	if h.NewStart != 10 || h.NewCount != 8 {
		t.Errorf("new range = %d,%d", h.NewStart, h.NewCount)
	}

	// Lines: 2 context, 1 remove, 2 add, 2 context
	if len(h.Lines) != 7 {
		t.Fatalf("expected 7 lines, got %d", len(h.Lines))
	}

	// First context line
	assertDiffLine(t, h.Lines[0], DiffLineContext, "\tctx := context.Background()", 10, 10)
	assertDiffLine(t, h.Lines[1], DiffLineContext, "\tname := \"hello\"", 11, 11)
	assertDiffLine(t, h.Lines[2], DiffLineRemove, "\tfmt.Println(name)", 12, 0)
	assertDiffLine(t, h.Lines[3], DiffLineAdd, "\tlog.Printf(\"name: %s\", name)", 0, 12)
	assertDiffLine(t, h.Lines[4], DiffLineAdd, "\tlog.Printf(\"done\")", 0, 13)
	assertDiffLine(t, h.Lines[5], DiffLineContext, "\treturn nil", 13, 14)
	assertDiffLine(t, h.Lines[6], DiffLineContext, "}", 14, 15)
}

func TestParseUnifiedDiff_MultiFile(t *testing.T) {
	diff := `diff --git a/file1.go b/file1.go
index abc..def 100644
--- a/file1.go
+++ b/file1.go
@@ -1,3 +1,4 @@
 package main
+import "fmt"

 func main() {
diff --git a/file2.go b/file2.go
new file mode 100644
--- /dev/null
+++ b/file2.go
@@ -0,0 +1,3 @@
+package main
+
+func helper() {}
`
	files := ParseUnifiedDiff(diff)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// File 1
	if files[0].NewName != "file1.go" {
		t.Errorf("file1 NewName = %q", files[0].NewName)
	}
	if len(files[0].Hunks) != 1 {
		t.Fatalf("file1: expected 1 hunk, got %d", len(files[0].Hunks))
	}
	if len(files[0].Hunks[0].Lines) != 4 {
		t.Errorf("file1 hunk lines = %d", len(files[0].Hunks[0].Lines))
	}

	// File 2 (new file)
	if files[1].NewName != "file2.go" {
		t.Errorf("file2 NewName = %q", files[1].NewName)
	}
	if len(files[1].Hunks) != 1 {
		t.Fatalf("file2: expected 1 hunk, got %d", len(files[1].Hunks))
	}
	h2 := files[1].Hunks[0]
	if h2.OldStart != 0 || h2.OldCount != 0 {
		t.Errorf("file2 old range = %d,%d", h2.OldStart, h2.OldCount)
	}
	if h2.NewStart != 1 || h2.NewCount != 3 {
		t.Errorf("file2 new range = %d,%d", h2.NewStart, h2.NewCount)
	}
	if len(h2.Lines) != 3 {
		t.Fatalf("file2 hunk lines = %d, want 3", len(h2.Lines))
	}
	assertDiffLine(t, h2.Lines[0], DiffLineAdd, "package main", 0, 1)
	assertDiffLine(t, h2.Lines[1], DiffLineAdd, "", 0, 2)
	assertDiffLine(t, h2.Lines[2], DiffLineAdd, "func helper() {}", 0, 3)
}

func TestParseUnifiedDiff_DeleteOnly(t *testing.T) {
	diff := `--- a/old.go
+++ b/old.go
@@ -5,4 +5,2 @@ package main
 func keep() {}
-func remove1() {}
-func remove2() {}
 func alsoKeep() {}
`
	files := ParseUnifiedDiff(diff)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	h := files[0].Hunks[0]
	if len(h.Lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(h.Lines))
	}
	assertDiffLine(t, h.Lines[0], DiffLineContext, "func keep() {}", 5, 5)
	assertDiffLine(t, h.Lines[1], DiffLineRemove, "func remove1() {}", 6, 0)
	assertDiffLine(t, h.Lines[2], DiffLineRemove, "func remove2() {}", 7, 0)
	assertDiffLine(t, h.Lines[3], DiffLineContext, "func alsoKeep() {}", 8, 6)
}

func TestParseUnifiedDiff_NoNewlineAtEOF(t *testing.T) {
	diff := `--- a/file.go
+++ b/file.go
@@ -1,2 +1,2 @@
 package main
-var x = 1
\ No newline at end of file
+var x = 2
\ No newline at end of file
`
	files := ParseUnifiedDiff(diff)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	h := files[0].Hunks[0]
	if len(h.Lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(h.Lines))
	}
	assertDiffLine(t, h.Lines[0], DiffLineContext, "package main", 1, 1)
	assertDiffLine(t, h.Lines[1], DiffLineRemove, "var x = 1", 2, 0)
	assertDiffLine(t, h.Lines[2], DiffLineAdd, "var x = 2", 0, 2)
}

func TestParseUnifiedDiff_MultipleHunks(t *testing.T) {
	diff := `--- a/file.go
+++ b/file.go
@@ -1,3 +1,3 @@
 line1
-line2
+LINE2
 line3
@@ -10,3 +10,3 @@
 line10
-line11
+LINE11
 line12
`
	files := ParseUnifiedDiff(diff)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if len(files[0].Hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d", len(files[0].Hunks))
	}

	h1 := files[0].Hunks[0]
	assertDiffLine(t, h1.Lines[1], DiffLineRemove, "line2", 2, 0)
	assertDiffLine(t, h1.Lines[2], DiffLineAdd, "LINE2", 0, 2)

	h2 := files[0].Hunks[1]
	if h2.OldStart != 10 {
		t.Errorf("hunk2 OldStart = %d", h2.OldStart)
	}
	assertDiffLine(t, h2.Lines[1], DiffLineRemove, "line11", 11, 0)
	assertDiffLine(t, h2.Lines[2], DiffLineAdd, "LINE11", 0, 11)
}

func TestParseUnifiedDiff_Binary(t *testing.T) {
	diff := `diff --git a/image.png b/image.png
index abc..def 100644
Binary files a/image.png and b/image.png differ
`
	files := ParseUnifiedDiff(diff)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if !files[0].Binary {
		t.Error("expected Binary=true")
	}
	if files[0].NewName != "image.png" {
		t.Errorf("NewName = %q", files[0].NewName)
	}
}

func TestParseUnifiedDiff_AddOnly(t *testing.T) {
	diff := `--- a/file.go
+++ b/file.go
@@ -3,2 +3,5 @@ package main
 func existing() {}
+func new1() {}
+func new2() {}
+func new3() {}
 func another() {}
`
	files := ParseUnifiedDiff(diff)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	h := files[0].Hunks[0]
	if len(h.Lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(h.Lines))
	}
	assertDiffLine(t, h.Lines[0], DiffLineContext, "func existing() {}", 3, 3)
	assertDiffLine(t, h.Lines[1], DiffLineAdd, "func new1() {}", 0, 4)
	assertDiffLine(t, h.Lines[2], DiffLineAdd, "func new2() {}", 0, 5)
	assertDiffLine(t, h.Lines[3], DiffLineAdd, "func new3() {}", 0, 6)
	assertDiffLine(t, h.Lines[4], DiffLineContext, "func another() {}", 4, 7)
}

func assertDiffLine(t *testing.T, got DiffLine, wantType DiffLineType, wantContent string, wantOld, wantNew int) {
	t.Helper()
	if got.Type != wantType {
		t.Errorf("line type = %d, want %d (content=%q)", got.Type, wantType, got.Content)
	}
	if got.Content != wantContent {
		t.Errorf("content = %q, want %q", got.Content, wantContent)
	}
	if got.OldLine != wantOld {
		t.Errorf("OldLine = %d, want %d (content=%q)", got.OldLine, wantOld, got.Content)
	}
	if got.NewLine != wantNew {
		t.Errorf("NewLine = %d, want %d (content=%q)", got.NewLine, wantNew, got.Content)
	}
}
