package protocol

import (
	"reflect"
	"testing"
)

// Ported from crates/bitcomet-api/src/addtask.rs `mod tests`, one Go test per
// Rust #[test], each naming the Rust case it mirrors.

func TestClassifyAddTargetMagnetPrefixWinsOverTorrentSuffix(t *testing.T) {
	// Rust: magnet_prefix_wins_over_torrent_suffix
	// 同时满足"magnet: 开头"和"以 .torrent 结尾"，必须先判 magnet
	s := "magnet:?xt=urn:btih:abc&dn=Some.Movie.2020.torrent"
	if got := ClassifyAddTarget(s); got != AddKindTorrent {
		t.Fatalf("classify(%q) = %v, want Torrent", s, got)
	}
}

func TestClassifyAddTargetPlainTorrentURLIsTorrent(t *testing.T) {
	// Rust: plain_torrent_url_is_torrent
	if got := ClassifyAddTarget("https://example.com/a/b/c.torrent"); got != AddKindTorrent {
		t.Fatalf("got %v, want Torrent", got)
	}
	// 大小写不敏感
	if got := ClassifyAddTarget("https://example.com/a.TORRENT"); got != AddKindTorrent {
		t.Fatalf("got %v, want Torrent (case-insensitive)", got)
	}
}

func TestClassifyAddTargetTorrentWithQueryOrFragmentIsTorrent(t *testing.T) {
	// Rust: torrent_with_query_or_fragment_is_torrent
	if got := ClassifyAddTarget("https://ex.com/x.torrent?token=deadbeef"); got != AddKindTorrent {
		t.Fatalf("query form: got %v, want Torrent", got)
	}
	if got := ClassifyAddTarget("https://ex.com/x.torrent#frag"); got != AddKindTorrent {
		t.Fatalf("fragment form: got %v, want Torrent", got)
	}
}

func TestClassifyAddTargetHTTPAndFTPAreHTTPKind(t *testing.T) {
	// Rust: http_and_ftp_are_http_kind
	for _, s := range []string{
		"https://ex.com/file.mkv",
		"http://ex.com/file",
		"ftp://ex.com/a.iso",
		// 只是含 ".torrent" 字样，不是后缀
		"https://ex.com/x.torrent.mkv",
	} {
		if got := ClassifyAddTarget(s); got != AddKindHTTP {
			t.Fatalf("classify(%q) = %v, want HTTP", s, got)
		}
	}
}

func TestAddKindEndpointMatchesContract(t *testing.T) {
	// Rust: endpoint_matches_contract
	if got := AddKindTorrent.Endpoint(); got != "/api/task/torrent_links/add" {
		t.Fatalf("Torrent endpoint = %q", got)
	}
	if got := AddKindHTTP.Endpoint(); got != "/api/task/http/add" {
		t.Fatalf("HTTP endpoint = %q", got)
	}
}

func TestSplitAddLinksHandlesAllSeparatorsAndDoesNotSplitOnSpace(t *testing.T) {
	// Rust: split_handles_all_separators_and_does_not_split_on_space
	input := "magnet:?xt=urn:btih:1\nhttps://a/b.torrent,\nhttps://c/d?x=1 ; ftp://e/f\n\n"
	want := []string{
		"magnet:?xt=urn:btih:1",
		"https://a/b.torrent",
		"https://c/d?x=1",
		"ftp://e/f",
	}
	if got := SplitAddLinks(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("split = %#v, want %#v", got, want)
	}
	// 含空格的单条链接不能被切开
	one := SplitAddLinks("https://ex.com/a b c.mkv")
	if !reflect.DeepEqual(one, []string{"https://ex.com/a b c.mkv"}) {
		t.Fatalf("space-split = %#v", one)
	}
}

func TestSplitAddLinksDropsBlankAndKeepsChineseSeparators(t *testing.T) {
	// Rust: split_drops_blank_and_keeps_chinese_separators
	got := SplitAddLinks("a，b；c;\n  ,")
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("split = %#v, want %#v", got, want)
	}
	if n := len(SplitAddLinks("   \n  ")); n != 0 {
		t.Fatalf("blank input produced %d links", n)
	}
}

func TestCollectAddLinksDedupsAcrossInputs(t *testing.T) {
	// Rust: collect_dedups_across_inputs
	got := CollectAddLinks([]string{
		"magnet:?xt=urn:btih:1",
		"magnet:?xt=urn:btih:1\nhttps://a/b.torrent",
	})
	want := []string{"magnet:?xt=urn:btih:1", "https://a/b.torrent"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collect = %#v, want %#v", got, want)
	}
}

// Extra (beyond the Rust tests, derived from addtask.rs's doc comment): an
// empty/blank input classifies as HTTP.
func TestClassifyAddTargetEmptyIsHTTP(t *testing.T) {
	for _, s := range []string{"", "   ", "\t"} {
		if got := ClassifyAddTarget(s); got != AddKindHTTP {
			t.Fatalf("classify(%q) = %v, want HTTP", s, got)
		}
	}
}
