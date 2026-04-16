package monetdroid

import (
	"strings"
	"testing"
)

func TestRenderKBMarkdown_RelativeLinks(t *testing.T) {
	got := renderKBMarkdown(`[Architecture](architecture.md)`, "/work")
	if !strings.Contains(got, `href="/kb/architecture.md?cwd=/work"`) {
		t.Fatalf("expected rewritten relative link, got: %s", got)
	}
}

func TestRenderKBMarkdown_SubdirLinks(t *testing.T) {
	got := renderKBMarkdown(`[Plan](plans/kb-web-view.md)`, "/work")
	if !strings.Contains(got, `href="/kb/plans/kb-web-view.md?cwd=/work"`) {
		t.Fatalf("expected rewritten subdir link, got: %s", got)
	}
}

func TestRenderKBMarkdown_FragmentLinks(t *testing.T) {
	got := renderKBMarkdown(`[Section](#implementation)`, "/work")
	if strings.Contains(got, `/kb/`) {
		t.Fatalf("fragment-only link should not be rewritten, got: %s", got)
	}
}

func TestRenderKBMarkdown_ExternalLinks(t *testing.T) {
	got := renderKBMarkdown(`[Google](https://google.com)`, "/work")
	if !strings.Contains(got, `target="_blank"`) {
		t.Fatalf("external link should have target=_blank, got: %s", got)
	}
	if strings.Contains(got, `/kb/`) {
		t.Fatalf("external link should not be rewritten, got: %s", got)
	}
}

func TestRenderKBMarkdown_MdLinkWithFragment(t *testing.T) {
	got := renderKBMarkdown(`[Section](architecture.md#core)`, "/work")
	if !strings.Contains(got, `href="/kb/architecture.md#core?cwd=/work"`) {
		t.Fatalf("expected .md link with fragment preserved, got: %s", got)
	}
}
